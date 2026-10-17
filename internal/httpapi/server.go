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
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"

	"golang.org/x/sync/semaphore"
)

//go:embed assets/*
var embeddedFS embed.FS

// Session represents a user session with expiration.
type Session struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type ApplyPlan struct {
	NeedReload  bool     `json:"need_reload"`
	NeedRestart bool     `json:"need_restart"`
	Applied     []string `json:"applied"`
	Pending     []string `json:"pending"`
}

type SettingsUpdateResult struct {
	Message     string   `json:"message"`
	Saved       bool     `json:"saved"`
	NeedReload  bool     `json:"need_reload"`
	NeedRestart bool     `json:"need_restart"`
	Reloaded    bool     `json:"reloaded"`
	ReloadError string   `json:"reload_error,omitempty"`
	Applied     []string `json:"applied"`
	Pending     []string `json:"pending"`
}

type settingsValidationError struct{ err error }

func (e settingsValidationError) Error() string { return e.err.Error() }

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

	// Session management
	sessionMu  sync.RWMutex
	sessions   map[string]*Session
	sessionTTL time.Duration

	// loginMu guards the failed-login rate limiter. Per-source windows are
	// cheap: brute-forcing the management password is the one threat this
	// endpoint faces, and a burst of failures from one source must not be able
	// to grind through guesses unthrottled.
	loginMu           sync.Mutex
	loginFailures     map[string]*loginFailureState
	loginFailuresLast time.Time

	// Concurrency control
	probeSem            *semaphore.Weighted
	configMutationMu    sync.Mutex
	groupMutationMu     sync.Mutex
	groupOperationLocks sync.Map // map[int64]*sync.Mutex

	// Lifecycle
	done chan struct{} // closed on Shutdown to stop background goroutines

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

// Login rate limiting: maxLoginFailuresWithin wrong passwords from one source
// inside loginFailureWindow lock that source out for loginLockout. The counter
// decays by the window, so slow guessing stays throttled too.
const (
	maxLoginFailuresWithin = 8
	loginFailureWindow     = 10 * time.Minute
	loginLockout           = 5 * time.Minute

	// maxSessions caps the in-memory session table. Every login inserts one,
	// so without a cap a scripted login loop grows the map (and the SQLite
	// sessions table) without bound.
	maxSessions = 256
)

type loginFailureState struct {
	count       int
	firstSeen   time.Time
	lockedUntil time.Time
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
		cfg:           cfg,
		mgr:           mgr,
		logger:        logger,
		sessions:      make(map[string]*Session),
		sessionTTL:    24 * time.Hour,
		loginFailures: make(map[string]*loginFailureState),
		probeSem:      semaphore.NewWeighted(int64(batchOperationConcurrency())),
		done:          make(chan struct{}),
	}

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
		s.Shutdown(context.Background())
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
	select {
	case <-s.done:
		// already closed
	default:
		close(s.done)
	}
	_ = s.srv.Shutdown(ctx)
}
