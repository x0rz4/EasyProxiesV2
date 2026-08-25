package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
	"easy_proxies/internal/subscription"
)

// Run builds the runtime components from config and blocks until shutdown.
func Run(ctx context.Context, cfg *config.Config) error {
	// ── 1. Open SQLite store ──
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
	defer dataStore.Close()

	// ── 2. Load nodes from Store into config (if any exist) ──
	if err := loadNodesFromStore(ctx, cfg, dataStore); err != nil {
		log.Printf("⚠️ Failed to load nodes from store: %v", err)
	}
	if err := loadGroupsFromStore(ctx, cfg, dataStore); err != nil {
		return fmt.Errorf("load group pools: %w", err)
	}

	// ── 3. Build monitor config ──
	proxyUsername := cfg.Listener.Username
	proxyPassword := cfg.Listener.Password
	if cfg.Mode == "multi-port" || cfg.Mode == "hybrid" {
		proxyUsername = cfg.MultiPort.Username
		proxyPassword = cfg.MultiPort.Password
	}

	monitorCfg := monitor.Config{
		Enabled:       cfg.ManagementEnabled(),
		Listen:        cfg.Management.Listen,
		ProbeTarget:   cfg.Management.ProbeTarget,
		Password:      cfg.Management.Password,
		ProxyUsername: proxyUsername,
		ProxyPassword: proxyPassword,
		ExternalIP:    cfg.ExternalIP,
	}

	// ── 4. Create and start BoxManager ──
	boxMgr := boxmgr.New(cfg, monitorCfg, boxmgr.WithStore(dataStore))
	group.SetGroupStateObserver(func(event group.GroupStateEvent) {
		if event.StateChanged && event.NodeID != 0 {
			_ = dataStore.UpsertGroupNodeState(context.Background(), &store.GroupNodeState{
				GroupID: event.GroupID, NodeID: event.NodeID, FailureHistory: event.FailureHistory,
				Evicted: event.Evicted, LastError: event.LastError, EvictedAt: event.EvictedAt})
		}
		if event.CurrentNodeID != 0 {
			if group, err := dataStore.GetGroupPool(context.Background(), event.GroupID); err == nil && group != nil {
				group.CurrentActiveNodeID = event.CurrentNodeID
				_ = dataStore.UpdateGroupPool(context.Background(), group)
			}
		}
	})
	defer group.SetGroupStateObserver(nil)
	if err := boxMgr.Start(ctx); err != nil {
		return fmt.Errorf("start box manager: %w", err)
	}
	defer boxMgr.Close()

	// Wire up config and store to monitor server for settings API
	if server := boxMgr.MonitorServer(); server != nil {
		server.SetConfig(cfg)
		server.SetStore(dataStore)
	}

	// ── 5. Create and start SubscriptionManager ──
	// Always created so it can dynamically respond to config changes
	// (e.g., user enables subscriptions via WebUI). The manager's internal
	// refresh loop checks config state to decide when to actually refresh.
	subMgr := subscription.New(cfg, boxMgr, subscription.WithStore(dataStore))
	subMgr.Start()
	defer subMgr.Stop()

	// Register as config update listener so baseCfg stays in sync after reloads
	boxMgr.AddConfigListener(subMgr)

	if server := boxMgr.MonitorServer(); server != nil {
		server.SetSubscriptionRefresher(subMgr)
	}

	// ── 6. Start periodic stats flush ──
	statsCtx, statsCancel := context.WithCancel(ctx)
	defer statsCancel()
	go periodicStatsFlush(statsCtx, boxMgr, dataStore)
	go syncGroupHealthFailures(statsCtx, boxMgr)

	// ── 7. Wait for shutdown signal ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-ctx.Done():
		fmt.Println("Context cancelled, initiating graceful shutdown...")
	case sig := <-sigCh:
		fmt.Printf("Received %s, initiating graceful shutdown...\n", sig)
	}

	// ── 8. Graceful shutdown ──
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Flush stats one final time before shutdown
	flushStatsToStore(context.Background(), boxMgr, dataStore)

	fmt.Println("Stopping subscription manager...")
	if subMgr != nil {
		subMgr.Stop()
	}

	fmt.Println("Stopping box manager...")
	if err := boxMgr.Close(); err != nil {
		fmt.Printf("Error closing box manager: %v\n", err)
	}

	fmt.Println("Waiting for connections to drain...")
	select {
	case <-time.After(2 * time.Second):
		fmt.Println("Graceful shutdown completed")
	case <-shutdownCtx.Done():
		fmt.Println("Shutdown timeout exceeded, forcing exit")
	}

	return nil
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
	legacySnapshotPending := len(subscriptions) == 0
	for _, sub := range subscriptions {
		if sub.LastSuccess.IsZero() && sub.NodeCount == 0 {
			legacySnapshotPending = true
			break
		}
	}
	legacyBootstrap := len(effectiveSubscriptionNodes) == 0 && legacySnapshotPending && len(cfg.Subscriptions) > 0 && len(loadedConfigNodes) > 0
	if legacyBootstrap {
		cfg.Nodes = loadedConfigNodes
		log.Printf("[app] using %d freshly fetched nodes while subscription memberships are initialized", len(loadedConfigNodes))
		return nil
	}

	// Convert enabled non-subscription and effective subscription nodes.
	var configNodes []config.NodeConfig
	seen := make(map[string]struct{}, len(nodes)+len(effectiveSubscriptionNodes))
	for _, n := range nodes {
		if !n.Enabled || n.Source == store.NodeSourceSubscription {
			continue
		}
		if _, ok := seen[n.URI]; ok {
			continue
		}
		seen[n.URI] = struct{}{}
		configNodes = append(configNodes, config.NodeConfig{
			ID:       n.ID,
			Name:     n.Name,
			URI:      n.URI,
			Port:     n.Port,
			Username: n.Username,
			Password: n.Password,
			Source:   config.NodeSource(n.Source),
			Region:   n.Region,
			Country:  n.Country,
		})
	}
	for _, n := range effectiveSubscriptionNodes {
		if _, ok := seen[n.URI]; ok {
			continue
		}
		seen[n.URI] = struct{}{}
		configNodes = append(configNodes, config.NodeConfig{ID: n.ID, Name: n.Name, URI: n.URI, Port: n.Port,
			Username: n.Username, Password: n.Password, Source: config.NodeSourceSubscription,
			Region: n.Region, Country: n.Country})
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
	cfg.Groups = make([]config.GroupPoolConfig, 0, len(groups))
	for _, group := range groups {
		converted := config.GroupPoolConfig{ID: group.ID, Name: group.Name, BindAddress: group.BindAddress,
			BindPort: group.BindPort, Protocol: group.Protocol, Username: group.Username, Password: group.Password,
			DispatchMode: group.DispatchMode, Regions: group.Regions, ExplicitNodeIDs: group.ExplicitNodeIDs,
			FailureWindow:    time.Duration(group.FailureWindowSeconds) * time.Second,
			FailureThreshold: group.FailureThreshold, HealthCheckInterval: time.Duration(group.HealthCheckSeconds) * time.Second,
			CurrentActiveNodeID: group.CurrentActiveNodeID, Enabled: group.Enabled,
			SubscriptionEnabled: group.SubscriptionEnabled, SubscriptionToken: group.SubscriptionToken,
			SubscriptionMode: group.SubscriptionMode, ExternalHost: group.ExternalHost,
			CreatedAt: group.CreatedAt, UpdatedAt: group.UpdatedAt}
		for _, state := range group.NodeStates {
			converted.NodeStates = append(converted.NodeStates, config.GroupNodeStateConfig{NodeID: state.NodeID,
				FailureHistory: state.FailureHistory, Evicted: state.Evicted, LastError: state.LastError, EvictedAt: state.EvictedAt})
		}
		cfg.Groups = append(cfg.Groups, converted)
	}
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

// syncGroupHealthFailures projects the existing global health checker into
// each group-specific sliding failure window. Traffic failures are recorded by
// the group outbound itself; this loop covers idle nodes that only fail probes.
func syncGroupHealthFailures(ctx context.Context, boxMgr *boxmgr.Manager) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mgr := boxMgr.MonitorManager()
			if mgr == nil {
				continue
			}
			snapshots := make(map[string]monitor.Snapshot)
			for _, snapshot := range mgr.Snapshot() {
				snapshots[snapshot.Tag] = snapshot
			}
			for groupID, runtime := range group.GroupRuntimeSnapshots() {
				for _, member := range runtime.Members {
					snapshot, ok := snapshots[member.Tag]
					if !ok || !snapshot.InitialCheckDone || snapshot.Available || snapshot.LastFailure.IsZero() {
						continue
					}
					group.RecordGroupHealthFailure(groupID, member.Tag, errors.New(snapshot.LastError), snapshot.LastFailure)
				}
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
	nameToID := make(map[string]int64, len(storeNodes))
	for _, n := range storeNodes {
		uriToID[n.URI] = n.ID
		nameToID[n.Name] = n.ID
	}

	var updates []store.StatsUpdate
	for _, snap := range snapshots {
		nodeID, ok := uriToID[snap.URI]
		if !ok {
			nodeID, ok = nameToID[snap.Name]
		}
		if !ok || nodeID == 0 {
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
}
