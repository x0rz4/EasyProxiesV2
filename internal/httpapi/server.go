package httpapi

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodetag"
	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"

	"golang.org/x/sync/semaphore"
)

//go:embed assets/*
var embeddedFS embed.FS

const (
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 60 * time.Second
)

// Server exposes HTTP endpoints for monitoring.
type Server struct {
	cfg    monitor.Config
	cfgMu  sync.RWMutex   // protects cfgSrc pointer assignment and local cfg fields
	cfgSrc *config.Config // 可持久化的配置对象; fields protected by cfgSrc.mu
	mgr    *monitor.Manager
	srv    *http.Server
	logger *log.Logger
	store  store.Store // 数据存储

	// auth owns sessions, the login rate limiter and API-key credentials. It
	// holds no HTTP concepts, so the domain can be tested without a server.
	// Built lazily by authService because tests assemble Server as a literal.
	authMu sync.Mutex
	auth   *controlplane.Auth

	// settings owns the config document: validation, persistence and the
	// hot-apply plan. Like auth it is built lazily because tests assemble
	// Server as a literal.
	settingsMu sync.Mutex
	settings   *controlplane.Settings

	// Concurrency control
	probeSem            *semaphore.Weighted
	groupMutationMu     sync.Mutex
	groupOperationLocks sync.Map // map[int64]*sync.Mutex

	// Lifecycle
	done           chan struct{} // closed on Shutdown to stop background goroutines
	shutdownOnce   sync.Once
	profileEnabled bool // startup opt-in; profiles also require a local authenticated caller

	subRefresher   controlplane.SubscriptionRefresher
	nodeMgr        controlplane.NodeManager
	geoLookup      *geoip.Lookup // optional, used by unlock checks for IP purity
	geoLookupMu    sync.Mutex    // protects lazy open/reload of geoLookup
	geoLookupPath  string        // path the cached geoLookup was opened from
	geoLookupMtime time.Time     // mtime of the db file when geoLookup was opened
	nodeChecks     *nodeCheckManager
	retag          RetagQueue       // optional, nil disables automatic re-tagging
	tagSvc         *nodetag.Service // optional, backs the tag API
}

// RetagQueue is the slice of *nodetag.Queue the detection pipeline drives. It is
// an interface so a test can observe enqueues without a store-backed service;
// every method is safe on a nil implementation, which is what lets probe
// goroutines call it without knowing the shutdown order.
type RetagQueue interface {
	Enqueue(nodeIDs ...int64)
	EnqueueAll()
}

// NewServer constructs a server; it can be nil when disabled.
func NewServer(cfg monitor.Config, mgr *monitor.Manager, logger *log.Logger) *Server {
	if !cfg.Enabled || mgr == nil {
		return nil
	}
	if logger == nil {
		logger = log.Default()
	}

	s := &Server{
		cfg:            cfg,
		mgr:            mgr,
		logger:         logger,
		probeSem:       processDiagnosticBudgets[probe.Unlock],
		done:           make(chan struct{}),
		profileEnabled: os.Getenv("EASY_PROXIES_PROFILE_ENABLED") == "1",
	}
	// Build the auth domain eagerly so the session-cleanup goroutine below has
	// something to drive. The password is read through the server, so a settings
	// change takes effect without rebinding the service.
	s.authService()

	// Start session cleanup goroutine
	go s.cleanupExpiredSessions()

	mux := s.routes()
	s.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	return s
}

func batchOperationConcurrency() int {
	limit := runtime.NumCPU() * 4
	if limit < 10 {
		return 10
	}
	return limit
}

type unlockBatchResult struct {
	result *unlock.Result
	err    string
}

func collectUnlockBatch(
	ctx context.Context,
	limit int,
	global *semaphore.Weighted,
	snapshots []monitor.Snapshot,
	run func(context.Context, monitor.Snapshot) unlockBatchResult,
) <-chan unlockBatchResult {
	return monitor.CollectLimited(limit, snapshots, func(snapshot monitor.Snapshot) unlockBatchResult {
		if err := global.Acquire(ctx, 1); err != nil {
			return unlockBatchResult{err: "unlock cancelled: " + err.Error()}
		}
		defer global.Release(1)
		return run(ctx, snapshot)
	})
}

// SetSubscriptionRefresher sets the subscription refresher for API endpoints.
func (s *Server) SetSubscriptionRefresher(sr controlplane.SubscriptionRefresher) {
	if s != nil {
		s.subRefresher = sr
	}
}

// SetNodeManager enables config-node CRUD endpoints.
func (s *Server) SetNodeManager(nm controlplane.NodeManager) {
	if s != nil {
		s.nodeMgr = nm
	}
}

// SetGeoipLookup binds the runtime GeoIP lookup used by unlock checks to
// classify the node's exit IP. May be nil (GeoIP disabled); unlock checks
// then fall back to the trace endpoint's coarse loc field.
func (s *Server) SetGeoipLookup(l *geoip.Lookup) {
	if s != nil {
		s.geoLookupMu.Lock()
		defer s.geoLookupMu.Unlock()
		s.geoLookup = l
		s.geoLookupPath = ""
		s.geoLookupMtime = time.Time{}
	}
}

// geoipLookupForCheck returns a GeoIP lookup suitable for landing-IP checks.
// Landing country is foundational node metadata used by groups, so an
// available database may be used even when the optional GeoIP listener is off.
// It first prefers the runtime lookup injected via SetGeoipLookup (kept live
// by the builder); when none is available it lazily opens one from the
// configured database path, re-opening if the on-disk file changed (e.g.
// after the user downloaded/updated the IP library via the WebUI).
func (s *Server) geoipLookupForCheck() *geoip.Lookup {
	if s == nil {
		return nil
	}
	dbPath := s.geoipDatabasePath()
	if dbPath == "" {
		return nil
	}

	s.geoLookupMu.Lock()
	defer s.geoLookupMu.Unlock()

	st, err := os.Stat(dbPath)
	if err != nil {
		return nil
	}
	mtime := st.ModTime()
	if s.geoLookup != nil && s.geoLookupPath == dbPath && !mtime.After(s.geoLookupMtime) {
		return s.geoLookup
	}

	// Reopen: close the stale lookup and open a fresh one bound to dbPath.
	if s.geoLookup != nil {
		s.geoLookup.Close()
		s.geoLookup = nil
	}
	l, err := geoip.OpenExisting(dbPath)
	if err != nil || l == nil {
		return nil
	}
	s.geoLookup = l
	s.geoLookupPath = dbPath
	s.geoLookupMtime = mtime
	return s.geoLookup
}

// closeGeoipLookup releases the memory-mapped database before replacing it.
// This is required on Windows, where an open MMDB cannot be renamed over.
func (s *Server) closeGeoipLookup() {
	if s == nil {
		return
	}
	s.geoLookupMu.Lock()
	defer s.geoLookupMu.Unlock()
	if s.geoLookup != nil {
		_ = s.geoLookup.Close()
	}
	s.geoLookup = nil
	s.geoLookupPath = ""
	s.geoLookupMtime = time.Time{}
}

// SetStore sets the data store for session persistence and other operations.
// The node-check manager is process-local state and must survive repeated
// bindings; startup orphan repair is owned by app.Run before runtimes start.
func (s *Server) SetStore(st store.Store) {
	if s != nil {
		s.store = st
		s.authService().SetStore(st)
		if s.mgr != nil {
			s.mgr.SetProbeStore(st)
		}
		if s.nodeChecks == nil {
			s.nodeChecks = newNodeCheckManager(s)
		}
	}
}

// SetRetagQueue binds the queue that recomputes auto tags after a detection.
// Nil means detections make no tag changes at all.
func (s *Server) SetRetagQueue(queue RetagQueue) {
	if s != nil {
		s.retag = queue
	}
}

// SetTagService binds the tagging service backing the tag API.
func (s *Server) SetTagService(svc *nodetag.Service) {
	if s != nil {
		s.tagSvc = svc
	}
}

// enqueueRetag asks for a recompute of the given nodes. It never blocks and is
// a no-op when no queue is wired, so a detection path can call it
// unconditionally.
func (s *Server) enqueueRetag(nodeIDs ...int64) {
	if s == nil || s.retag == nil || len(nodeIDs) == 0 {
		return
	}
	s.retag.Enqueue(nodeIDs...)
}

// enqueueRetagAll asks for a recompute of every node. Node CRUD and a
// subscription refresh use it because they change which nodes exist rather than
// what is known about one node.
func (s *Server) enqueueRetagAll() {
	if s == nil || s.retag == nil {
		return
	}
	s.retag.EnqueueAll()
}

// refreshGroupMembership tells the runtime that these nodes' facts moved, so any
// group whose member set depends on them has to be rebuilt.
//
// A runtime that cannot do this incrementally falls back to a full reload, which
// is correct but rebuilds every listener; a runtime with neither capability makes
// this a no-op, which is what the lightweight test managers want.
func (s *Server) refreshGroupMembership(changedNodeIDs []int64) error {
	if s == nil || len(changedNodeIDs) == 0 || s.nodeMgr == nil {
		return nil
	}
	if membershipMgr, ok := s.nodeMgr.(controlplane.GroupMembershipManager); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return membershipMgr.ApplyGroupMembershipChanges(ctx, changedNodeIDs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.nodeMgr.TriggerReload(ctx)
}

// SetConfig binds the persistable config object for settings API.
func (s *Server) SetConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfgSrc = cfg
	if cfg != nil {
		cfg.RLock()
		s.cfg.ExternalIP = cfg.ExternalIP
		s.cfg.ProbeTarget = cfg.Management.ProbeTarget
		s.cfg.StartupAvailabilityPolicy = cfg.Management.StartupAvailabilityPolicy
		s.cfg.SkipCertVerify = cfg.SkipCertVerify
		cfg.RUnlock()
	}
}

// Start launches the HTTP server.
func (s *Server) Start(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	s.logger.Printf("Starting monitor server on %s", s.cfg.Listen)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("❌ Monitor server error: %v", err)
		}
	}()
	// Give server a moment to start and check for immediate errors
	time.Sleep(100 * time.Millisecond)
	s.logger.Printf("✅ Monitor server started on http://%s", s.cfg.Listen)

	go func() {
		<-ctx.Done()
		budget, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.Shutdown(budget)
	}()
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) {
	if s == nil || s.srv == nil {
		return
	}
	if s.nodeChecks != nil {
		s.nodeChecks.shutdown()
	}
	// Signal background goroutines to stop
	s.shutdownOnce.Do(func() { close(s.done) })
	if err := s.srv.Shutdown(ctx); err != nil {
		_ = s.srv.Close()
	}
	if s.nodeChecks != nil {
		s.nodeChecks.workers.Wait()
	}
	s.authMu.Lock()
	auth := s.auth
	s.authMu.Unlock()
	if auth != nil {
		if err := auth.Close(ctx); err != nil && s.logger != nil {
			s.logger.Printf("[auth] shutdown usage writer: %v", err)
		}
	}
}

// authService returns the authentication domain, building it on first use.
// Server is assembled as a struct literal in tests, so the service cannot be a
// NewServer-only invariant; every auth path goes through here instead of
// touching the field.
func (s *Server) authService() *controlplane.Auth {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.auth == nil {
		s.auth = controlplane.NewAuth(controlplane.AuthOptions{
			Store:    s.store,
			Logger:   s.logger,
			Password: s.authPassword,
		})
	}
	return s.auth
}

// settingsService returns the settings domain, building it on first use. It
// reads and writes the config through the callbacks below so the object can be
// replaced wholesale on every save without rebinding the service.
func (s *Server) settingsService() *controlplane.Settings {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if s.settings == nil {
		s.settings = controlplane.NewSettings(controlplane.SettingsOptions{
			Logger:                s.logger,
			Config:                s.persistableConfig,
			Commit:                s.publishConfig,
			Runtime:               runtimeApplierOf(s.mgr),
			Reloader:              reloaderOf(s.nodeMgr),
			Subs:                  s.subRefresher,
			SubStore:              storeAsRefreshStore(s.store),
			ValidateProbeTarget:   validateProbeTarget,
			NormalizeAvailability: monitor.NormalizeStartupAvailabilityPolicy,
		})
	}
	return s.settings
}

// persistableConfig returns the config object settings are written to.
func (s *Server) persistableConfig() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfgSrc
}

// publishConfig installs a freshly saved config and refreshes the local
// runtime view the HTTP layer reads directly.
func (s *Server) publishConfig(cfg *config.Config) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.cfgSrc = cfg
	if cfg == nil {
		return
	}
	cfg.RLock()
	defer cfg.RUnlock()
	s.cfg.ExternalIP = cfg.ExternalIP
	s.cfg.ProbeTarget = cfg.Management.ProbeTarget
	s.cfg.StartupAvailabilityPolicy = cfg.Management.StartupAvailabilityPolicy
	s.cfg.SkipCertVerify = cfg.SkipCertVerify
	s.cfg.Password = cfg.Management.Password
}

// validateProbeTarget wraps the runtime parser so the settings service can
// reject a bad target without importing monitor.
func validateProbeTarget(target string) error {
	_, err := monitor.ParseProbeTarget(target)
	return err
}

// reloaderOf adapts the node manager to the settings service's reload port.
// A manager that cannot reload leaves the trigger nil, which makes the service
// report the change as pending rather than silently claiming success.
func reloaderOf(nodeMgr controlplane.NodeManager) controlplane.ReloadTrigger {
	if nodeMgr == nil {
		return nil
	}
	return nodeMgr
}

// runtimeApplier adapts *monitor.Manager to the settings service's port. The
// service defines its own ProbePolicy so it does not import monitor; this
// adapter is the single place the two shapes meet.
type runtimeApplier struct{ mgr *monitor.Manager }

func (r runtimeApplier) SetStartupAvailabilityPolicy(policy string) {
	r.mgr.SetStartupAvailabilityPolicy(policy)
}

func (r runtimeApplier) SetHealthCheckInterval(interval time.Duration) {
	r.mgr.SetHealthCheckInterval(interval)
}

func (r runtimeApplier) UpdateProbeTarget(target string) error {
	return r.mgr.UpdateProbeTarget(target)
}

func (r runtimeApplier) UpdateProbePolicy(policy controlplane.ProbePolicy) {
	r.mgr.UpdateProbePolicy(monitor.ProbePolicy{
		Concurrency:     policy.Concurrency,
		StartupTimeout:  policy.StartupTimeout,
		RoutineTimeout:  policy.RoutineTimeout,
		DialTimeout:     policy.DialTimeout,
		ResponseTimeout: policy.ResponseTimeout,
		RoutineRetries:  policy.RoutineRetries,
	})
}

// runtimeApplierOf returns a settings port for the manager, or nil when there
// is no manager (which makes the service skip hot-apply entirely).
func runtimeApplierOf(mgr *monitor.Manager) controlplane.RuntimeApplier {
	if mgr == nil {
		return nil
	}
	return runtimeApplier{mgr: mgr}
}

// storeAsRefreshStore adapts the store to the narrow fan-out port. A nil store
// yields a nil port so the service skips the fan-out entirely.
func storeAsRefreshStore(st store.Store) controlplane.SubscriptionRefreshStore {
	if st == nil {
		return nil
	}
	return st.(controlplane.SubscriptionRefreshStore)
}
