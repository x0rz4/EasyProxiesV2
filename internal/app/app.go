package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/nodetag"
	"easy_proxies/internal/runtimenodes"
	"easy_proxies/internal/store"
	"easy_proxies/internal/subscription"
)

// newTagService assembles the tagging service. The unlock providers come from the
// checker registry rather than a list here, so registering a new checker adds its
// rule fields and its builtin template with no change in this file.
//
// notify receives the nodes whose visible tag set changed; passing nil leaves the
// tagging service purely persistent, which is what the offline tests want.
func newTagService(dataStore store.Store, notify func([]int64)) *nodetag.Service {
	unlockProviders := monitor.TagUnlockFactProviders()
	options := []nodetag.Option{
		nodetag.WithRegistry(monitor.NewTagFactRegistry()),
		nodetag.WithUnlockProviders(unlockProviders),
	}
	if notify != nil {
		options = append(options, nodetag.WithMembershipNotifier(notify))
	}
	return nodetag.NewService(dataStore, options...)
}

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) (runErr error) {
	ctx, cancel := context.WithCancel(ctx)
	shutdown := &shutdownStack{cancel: cancel}
	defer func() {
		budget, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := shutdown.close(budget); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	// ── 1. Prepare the GeoIP database before any landing-location work ──
	// A download failure must not take down the proxy runtime. BoxManager opens
	// only an existing validated database and will pause location classification
	// while this prerequisite is unavailable.
	if dbPath := strings.TrimSpace(cfg.GeoIP.DatabasePath); dbPath != "" {
		if err := geoip.EnsureDatabase(dbPath); err != nil {
			log.Printf("⚠️ GeoIP initialization failed; node location classification is paused: %v", err)
		}
	}

	// ── 2. Open SQLite store ──
	dbPath := cfg.DatabasePath
	if dbPath == "" {
		dbPath = filepath.Join(filepath.Dir(cfg.FilePath()), "data", "data.db")
	}

	// Ensure the directory for the database file exists
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create database directory %q: %w", dir, err)
		}
	}

	dataStore, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	shutdown.add(func(context.Context) error { return dataStore.Close() })
	if err := dataStore.InterruptRunningNodeDetectionTasks(ctx); err != nil {
		return fmt.Errorf("interrupt stale node detection tasks: %w", err)
	}
	if err := dataStore.PruneNodeDetectionTasks(ctx, 20); err != nil {
		log.Printf("⚠️ Failed to prune node detection task history: %v", err)
	}

	// ── 3. Load nodes from Store into config (if any exist) ──
	if err := loadNodesFromStore(ctx, cfg, dataStore); err != nil {
		log.Printf("⚠️ Failed to load nodes from store: %v", err)
	}
	if err := loadGroupsFromStore(ctx, cfg, dataStore); err != nil {
		return fmt.Errorf("load group pools: %w", err)
	}
	if err := loadTagNamesFromStore(ctx, cfg, dataStore); err != nil {
		return fmt.Errorf("load tags: %w", err)
	}

	// ── 4. Build monitor config ──
	proxyUsername := cfg.Listener.Username
	proxyPassword := cfg.Listener.Password
	if !cfg.Listener.Enabled && cfg.MultiPort.Enabled {
		proxyUsername = cfg.MultiPort.Username
		proxyPassword = cfg.MultiPort.Password
	} else if !cfg.Listener.Enabled {
		proxyUsername = ""
		proxyPassword = ""
	}

	monitorCfg := monitor.Config{
		Enabled:                   cfg.ManagementEnabled(),
		Listen:                    cfg.Management.Listen,
		ProbeTarget:               cfg.Management.ProbeTarget,
		StartupAvailabilityPolicy: cfg.Management.StartupAvailabilityPolicy,
		Password:                  cfg.Management.Password,
		ProxyUsername:             proxyUsername,
		ProxyPassword:             proxyPassword,
		ExternalIP:                cfg.ExternalIP,
		SkipCertVerify:            cfg.SkipCertVerify,
		ProbeConcurrency:          cfg.Management.ProbeConcurrency,
		StartupProbeTimeout:       cfg.Management.StartupProbeTimeout,
		RoutineProbeTimeout:       cfg.Management.RoutineProbeTimeout,
		ProbeDialTimeout:          cfg.Management.ProbeDialTimeout,
		ProbeResponseTimeout:      cfg.Management.ProbeResponseTimeout,
		RoutineProbeRetries:       cfg.RoutineProbeRetryCount(),
	}

	// ── 5. Create and start BoxManager ──
	boxMgr := boxmgr.New(cfg, monitorCfg, boxmgr.WithStore(dataStore))
	groupPersister := newGroupStatePersister(dataStore)
	group.SetGroupStateObserver(groupPersister.Observe)
	shutdown.add(func(ctx context.Context) error {
		group.SetGroupStateObserver(nil)
		return groupPersister.Shutdown(ctx)
	})
	shutdown.add(func(ctx context.Context) error { return boxMgr.CloseContext(ctx) })
	if err := boxMgr.Start(ctx); err != nil {
		return fmt.Errorf("start box manager: %w", err)
	}

	// ── 5b. Automatic node tagging ──
	// The queue is what keeps a detection from writing tags inline: detections
	// persist facts, the queue coalesces them, and the service derives tags from
	// the facts. Nothing is tagged until the builtin templates are seeded through
	// POST /api/tags/templates, or an operator writes a rule.
	//
	// A tag change reaches the runtime as a per-group rebuild, never as a full
	// reload: only the groups whose member set actually moved are touched, so the
	// other listeners keep their live connections.
	membershipUpdates := newMembershipQueue(ctx, boxMgr.ApplyGroupMembershipChanges, time.Second)
	shutdown.add(func(context.Context) error { membershipUpdates.Close(); return nil })
	tagService := newTagService(dataStore, membershipUpdates.Enqueue)
	retagQueue := nodetag.NewQueue(tagService)
	shutdown.add(retagQueue.CloseContext)
	if server := boxMgr.MonitorServer(); server != nil {
		server.SetTagService(tagService)
		server.SetRetagQueue(retagQueue)
	}
	// Facts change while the process is down (a node was edited, a rule was
	// imported), so the first run reconciles everything.
	retagQueue.EnqueueAll()
	stopProbeProjections := startProbeProjections(ctx, dataStore, boxMgr.ProjectProbeOutcome, retagQueue.Enqueue)
	shutdown.add(func(context.Context) error { stopProbeProjections(); return nil })

	// ── 6. Create and start SubscriptionManager ──
	// Always created so it can dynamically respond to config changes
	// (e.g., user enables subscriptions via WebUI). The manager's internal
	// refresh loop checks config state to decide when to actually refresh.
	// A refresh changes which nodes exist, so every auto tag needs re-deriving.
	// This runs for background (create/update) and timer refreshes too, which the
	// HTTP handler cannot see.
	subMgr := subscription.New(cfg, boxMgr, subscription.WithStore(dataStore),
		subscription.WithRefreshObserver(retagQueue.EnqueueAll))
	subMgr.Start()
	shutdown.add(subMgr.Shutdown)

	// Register as config update listener so baseCfg stays in sync after reloads
	boxMgr.AddConfigListener(subMgr)

	if server := boxMgr.MonitorServer(); server != nil {
		server.SetSubscriptionRefresher(subMgr)
	}
	stopGroupHealer := startGroupHealer(ctx, &healingBackend{Manager: boxMgr, db: dataStore, subscriptions: subMgr})
	shutdown.add(func(context.Context) error { stopGroupHealer(); return nil })

	// ── 7. Start periodic stats flush ──
	statsCtx, statsCancel := context.WithCancel(ctx)
	var statsWorkers sync.WaitGroup
	statsWorkers.Go(func() { periodicStatsFlush(statsCtx, boxMgr, dataStore) })
	// Timeline rows were never cleaned up (CleanupTimeline had no caller),
	// so node_timeline grew without bound for as long as the process ran.
	statsWorkers.Go(func() { periodicTimelineCleanup(statsCtx, dataStore) })
	shutdown.add(func(ctx context.Context) error {
		statsCancel()
		statsWorkers.Wait()
		flushStatsToStore(ctx, boxMgr, dataStore)
		return ctx.Err()
	})
	if server := boxMgr.MonitorServer(); server != nil {
		shutdown.add(func(ctx context.Context) error { server.Shutdown(ctx); return ctx.Err() })
	}

	// ── 8. Wait for shutdown signal ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
		fmt.Println("Context cancelled, initiating graceful shutdown...")
	case sig := <-sigCh:
		fmt.Printf("Received %s, initiating graceful shutdown...\n", sig)
	}

	// The deferred stack applies one budget to the whole chain, including
	// error exits during startup. Cancellation reaches all producers first.
	return nil
}

// groupStatePersister writes member state changes to group_node_states.
//
// It coalesces by member instead of queueing one event per change. State events
// carry the complete member state (failure history, Evicted, EvictedAt), so the
// newest event for a group+node fully supersedes older ones — and a failure
// storm produces many changes for the same handful of members. Holding the
// pending set under a mutex means a burst can never overflow a fixed buffer and
// shed an eviction, which is the failure this type used to have: a dropped
// eviction leaves no row, so after a restart the bad node comes back healthy.
type groupStatePersister struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	store     store.Store
	wake      chan struct{}
	stop      chan struct{}
	mu        sync.RWMutex
	closed    bool
	wg        sync.WaitGroup
	// pending is keyed by group+node and always holds the newest state. Guarded
	// by mu.
	pending map[groupStateKey]group.GroupStateEvent
	// current holds the newest current-tag per group, flushed on a ticker so a
	// per-connection dial does not become a database write.
	current map[int64]int64
	// merged counts events superseded by a newer one for the same member. This
	// is normal operation, not loss.
	merged atomic.Int64
	// drainMu serialises drains. Close flushes synchronously while the worker
	// may still be inside its own drain; two drains racing on the same member
	// let the older snapshot win, resurrecting an eviction that was just
	// cleared. Holding this across a drain keeps writes in queue order.
	drainMu sync.Mutex
}

type groupStateKey struct {
	groupID int64
	nodeID  int64
}

func newGroupStatePersister(dataStore store.Store) *groupStatePersister {
	ctx, cancel := context.WithCancel(context.Background())
	p := &groupStatePersister{
		ctx: ctx, cancel: cancel,
		store:   dataStore,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		pending: make(map[groupStateKey]group.GroupStateEvent),
		current: make(map[int64]int64),
	}
	p.wg.Add(1)
	go p.run()
	return p
}

// Observe records a state change. The observer runs on the group failure/dial
// paths, so it must never block or fail: every accepted event is kept, newest
// wins for a given member.
func (p *groupStatePersister) Observe(event group.GroupStateEvent) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if event.StateChanged && event.NodeID != 0 {
		key := groupStateKey{groupID: event.GroupID, nodeID: event.NodeID}
		if _, replaced := p.pending[key]; replaced {
			p.merged.Add(1)
		}
		p.pending[key] = event
	}
	if event.CurrentChanged {
		p.current[event.GroupID] = event.CurrentNodeID
	}
	p.mu.Unlock()
	// Wake only after releasing mu: the worker takes mu to drain, and nothing
	// here should hold it while signalling.
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Merged reports state changes superseded by a newer change for the same
// member before they were written. This is expected coalescing, not loss.
func (p *groupStatePersister) Merged() int64 { return p.merged.Load() }

// Close flushes what is already pending and stops the worker. Once it returns no
// Observe call is accepted, so a late state change is not silently dropped
// behind a stopped writer.
func (p *groupStatePersister) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.Shutdown(ctx)
}

func (p *groupStatePersister) Shutdown(ctx context.Context) error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		p.cancel()
		close(p.stop)
		p.wg.Wait()
		p.drainStates(ctx)
		p.flushCurrent(ctx)
	})
	return ctx.Err()
}

func (p *groupStatePersister) run() {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-p.wake:
			p.drainStates(p.ctx)
		case <-ticker.C:
			p.drainStates(p.ctx)
			p.flushCurrent(p.ctx)
		}
	}
}

// drainStates writes every pending member state. A failed write stays pending so
// the next tick retries it instead of losing the transition.
func (p *groupStatePersister) drainStates(parent context.Context) {
	p.drainMu.Lock()
	defer p.drainMu.Unlock()
	p.mu.Lock()
	batch := p.pending
	p.pending = make(map[groupStateKey]group.GroupStateEvent, len(batch))
	closed := p.closed
	p.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	var failed map[groupStateKey]group.GroupStateEvent
	for key, event := range batch {
		if err := p.store.UpsertGroupNodeState(ctx, &store.GroupNodeState{
			GroupID: event.GroupID, NodeID: event.NodeID, FailureHistory: event.FailureHistory,
			Evicted: event.Evicted, LastError: event.LastError, EvictedAt: event.EvictedAt,
			Revision: event.Revision,
		}); err != nil {
			log.Printf("[app] persist group node state %d/%d: %v", event.GroupID, event.NodeID, err)
			if failed == nil {
				failed = make(map[groupStateKey]group.GroupStateEvent, len(batch))
			}
			failed[key] = event
		}
	}
	if len(failed) == 0 {
		return
	}
	p.mu.Lock()
	for key, event := range failed {
		// A newer event already queued for this member supersedes the retry.
		if _, newer := p.pending[key]; !newer {
			p.pending[key] = event
		}
	}
	p.mu.Unlock()
	if !closed {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (p *groupStatePersister) flushCurrent(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	p.mu.Lock()
	batch := p.current
	p.current = make(map[int64]int64, len(batch))
	p.mu.Unlock()
	for groupID, nodeID := range batch {
		err := p.store.UpdateGroupCurrentActiveNode(ctx, groupID, nodeID)
		if err == nil {
			continue
		}
		log.Printf("[app] persist group current %d: %v", groupID, err)
		if strings.Contains(err.Error(), "not found") {
			continue
		}
		// Transient failure: retry next tick unless a newer value superseded it.
		p.mu.Lock()
		if _, newer := p.current[groupID]; !newer {
			p.current[groupID] = nodeID
		}
		p.mu.Unlock()
	}
}

// loadNodesFromStore replaces config.Nodes with nodes from the Store.
// If the Store is empty, seeds it with whatever nodes config already has
// (from config.yaml inline nodes or subscription fetch), then returns.
func loadNodesFromStore(ctx context.Context, cfg *config.Config, s store.Store) error {
	loadedConfigNodes := append([]config.NodeConfig(nil), cfg.Nodes...)
	nodes, err := s.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		return fmt.Errorf("list nodes from store: %w", err)
	}

	if len(nodes) == 0 {
		// Store is empty — seed it with the nodes that config already loaded
		// (inline nodes, subscription fetch, manual nodes, etc.)
		if len(cfg.Nodes) > 0 {
			log.Printf("[app] store is empty, seeding %d nodes from config", len(cfg.Nodes))
			if err := seedStoreFromConfig(ctx, cfg, s); err != nil {
				log.Printf("⚠️ Failed to seed store from config: %v", err)
			}
		} else {
			log.Printf("[app] no nodes in store and no nodes in config")
		}
		return nil
	}

	// Subscription nodes are effective only through enabled memberships.
	effectiveSubscriptionNodes, err := s.ListEffectiveSubscriptionNodes(ctx)
	if err != nil {
		return fmt.Errorf("list effective subscription nodes: %w", err)
	}

	// During the first v3 startup, legacy subscription nodes exist in the
	// nodes table but no subscription membership has been imported yet. Keep
	// the nodes fetched by config.Load for this one startup; SubscriptionManager
	// imports the URLs and commits proper memberships immediately afterwards.
	subscriptions, err := s.ListSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	legacySnapshotPending := len(subscriptions) == 0 && len(cfg.Subscriptions) > 0
	for _, sub := range subscriptions {
		if sub.LastSuccess.IsZero() && sub.NodeCount == 0 {
			legacySnapshotPending = true
			break
		}
	}
	if !legacySnapshotPending {
		adopted, adoptErr := s.AdoptOrphanSubscriptionNodes(ctx)
		if adoptErr != nil {
			return fmt.Errorf("recover orphan subscription nodes: %w", adoptErr)
		}
		if adopted > 0 {
			log.Printf("[app] recovered %d orphan subscription nodes as manual nodes", adopted)
			effectiveSubscriptionNodes, err = s.ListEffectiveSubscriptionNodes(ctx)
			if err != nil {
				return fmt.Errorf("reload effective subscription nodes: %w", err)
			}
		}
	}
	legacyBootstrap := len(effectiveSubscriptionNodes) == 0 && legacySnapshotPending && len(cfg.Subscriptions) > 0 && len(loadedConfigNodes) > 0
	if legacyBootstrap {
		cfg.Nodes = loadedConfigNodes
		log.Printf("[app] using %d freshly fetched nodes while subscription memberships are initialized", len(loadedConfigNodes))
		return nil
	}

	// The runtime set is owned by runtimenodes: enabled non-subscription rows
	// plus the rows reachable through an enabled subscription membership. The
	// two lists read above are still needed for the startup-only decisions
	// (orphan adoption and the legacy bootstrap), but the conversion itself has
	// exactly one implementation.
	configNodes, err := runtimenodes.Load(ctx, s)
	if err != nil {
		return fmt.Errorf("load runtime nodes from store: %w", err)
	}

	// Always replace cfg.Nodes when the DB is non-empty, including all-disabled state.
	cfg.Nodes = configNodes
	log.Printf("[app] loaded %d effective nodes from store", len(configNodes))

	return nil
}

func loadGroupsFromStore(ctx context.Context, cfg *config.Config, s store.Store) error {
	groups, err := s.ListGroupPools(ctx)
	if err != nil {
		return err
	}
	cfg.Groups = boxmgr.GroupConfigsFromStore(groups)
	return nil
}

// loadTagNamesFromStore fills the tag ID → name mapping group membership joins
// through. A group's tag filter holds IDs while a node carries tag names, so
// without this mapping a tag-filtered group would match nothing.
func loadTagNamesFromStore(ctx context.Context, cfg *config.Config, s store.Store) error {
	tags, err := s.ListTags(ctx)
	if err != nil {
		return err
	}
	tagNames := make(map[int64]string, len(tags))
	for _, tag := range tags {
		tagNames[tag.ID] = tag.Name
	}
	cfg.TagNames = tagNames
	return nil
}

// seedStoreFromConfig writes all cfg.Nodes into the Store as a bulk upsert.
// This is called once on first startup when the database is empty.
func seedStoreFromConfig(ctx context.Context, cfg *config.Config, s store.Store) error {
	var storeNodes []store.Node
	for _, n := range cfg.Nodes {
		source := string(n.Source)
		if source == "" {
			source = store.NodeSourceSubscription
		}
		storeNodes = append(storeNodes, store.Node{
			URI:      n.URI,
			Name:     n.Name,
			Source:   source,
			Port:     n.Port,
			Username: n.Username,
			Password: n.Password,
			Enabled:  true,
		})
	}

	if err := s.BulkUpsertNodes(ctx, storeNodes); err != nil {
		return fmt.Errorf("bulk upsert seed nodes: %w", err)
	}

	log.Printf("[app] seeded %d nodes into store", len(storeNodes))
	return nil
}

// periodicStatsFlush periodically writes in-memory node stats to the Store.
func periodicStatsFlush(ctx context.Context, boxMgr *boxmgr.Manager, s store.Store) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			flushStatsToStore(ctx, boxMgr, s)
		}
	}
}

// periodicTimelineCleanup bounds node_timeline growth. The per-node keep
// count matches the CleanupTimeline default; an hourly cadence is plenty
// because the table only exceeds its steady-state size (nodes × keep) by the
// probes of one interval.
func periodicTimelineCleanup(ctx context.Context, s store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.CleanupTimeline(ctx, 20); err != nil {
				log.Printf("[app] cleanup node timeline: %v", err)
			}
		}
	}
}

// flushStatsToStore writes current node stats from monitor to the Store.
func flushStatsToStore(ctx context.Context, boxMgr *boxmgr.Manager, s store.Store) {
	mgr := boxMgr.MonitorManager()
	if mgr == nil {
		return
	}

	snapshots := mgr.Snapshot()
	if len(snapshots) == 0 {
		return
	}

	// Build URI/name -> node ID lookup from store
	storeNodes, err := s.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		log.Printf("[app] stats flush: failed to list store nodes: %v", err)
		return
	}
	uriToID := make(map[string]int64, len(storeNodes))
	identityToID := make(map[string]int64, len(storeNodes))
	// Name is not unique across nodes; a name only resolves when exactly one
	// store node carries it. A naive map would silently attribute A's stats
	// to B whenever two nodes share a display name.
	nameToID := make(map[string]int64, len(storeNodes))
	ambiguousNames := make(map[string]struct{})
	for _, n := range storeNodes {
		uriToID[n.URI] = n.ID
		if n.IdentityHash != "" {
			identityToID[n.IdentityHash] = n.ID
		}
		if _, seen := nameToID[n.Name]; seen {
			ambiguousNames[n.Name] = struct{}{}
			continue
		}
		nameToID[n.Name] = n.ID
	}

	var updates []store.StatsUpdate
	var unresolved int
	for _, snap := range snapshots {
		// Preserve the last committed database health snapshot until this concrete
		// runtime generation has completed its own startup verification.
		if !snap.InitialCheckDone {
			continue
		}
		nodeID, ok := snap.NodeID, snap.NodeID > 0
		if !ok {
			nodeID, ok = uriToID[snap.URI]
		}
		if !ok {
			if identity, identityErr := nodecodec.ParseURI(snap.URI); identityErr == nil {
				nodeID, ok = identityToID[identity.Hash]
			}
		}
		if !ok {
			// Name fallback: only when unambiguous.
			if _, dup := ambiguousNames[snap.Name]; !dup {
				nodeID, ok = nameToID[snap.Name]
			}
		}
		if !ok || nodeID == 0 {
			unresolved++
			continue
		}

		updates = append(updates, store.StatsUpdate{
			NodeID:             nodeID,
			FailureCount:       snap.FailureCount,
			SuccessCount:       snap.SuccessCount,
			Blacklisted:        snap.Blacklisted,
			BlacklistedUntil:   snap.BlacklistedUntil,
			LastError:          snap.LastError,
			LastFailureAt:      snap.LastFailure,
			LastSuccessAt:      snap.LastSuccess,
			LastLatencyMs:      snap.LastLatencyMs,
			Available:          snap.Available,
			InitialCheckDone:   snap.InitialCheckDone,
			TotalUploadBytes:   snap.TotalUpload,
			TotalDownloadBytes: snap.TotalDownload,
		})
	}

	if len(updates) > 0 {
		if err := s.BatchUpdateStats(ctx, updates); err != nil {
			log.Printf("[app] stats flush failed: %v", err)
		}
	}
	if unresolved > 0 {
		log.Printf("[app] stats flush: %d runtime snapshot(s) could not be matched to a store node and were skipped", unresolved)
	}
}
