package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// Config mirrors user settings needed by the monitoring server.
type Config struct {
	Enabled              bool
	Listen               string
	ProbeTarget          string
	Password             string
	ProxyUsername        string // 代理池的用户名（用于导出）
	ProxyPassword        string // 代理池的密码（用于导出）
	ExternalIP           string // 外部 IP 地址，用于导出时替换 0.0.0.0
	SkipCertVerify       bool   // 全局跳过 SSL 证书验证
	ProbeConcurrency     int
	StartupProbeTimeout  time.Duration
	RoutineProbeTimeout  time.Duration
	ProbeDialTimeout     time.Duration
	ProbeResponseTimeout time.Duration
	RoutineProbeRetries  int
}

type ProbeRoundKind string

const (
	ProbeRoundStartup  ProbeRoundKind = "startup"
	ProbeRoundPeriodic ProbeRoundKind = "periodic"
	ProbeRoundManual   ProbeRoundKind = "manual"
)

var ErrProbeRoundInProgress = errors.New("probe round already in progress")

const initialProbeRetryDelay = 500 * time.Millisecond

type ProbePolicy struct {
	Concurrency     int           `json:"probe_concurrency"`
	StartupTimeout  time.Duration `json:"-"`
	RoutineTimeout  time.Duration `json:"-"`
	DialTimeout     time.Duration `json:"-"`
	ResponseTimeout time.Duration `json:"-"`
	RoutineRetries  int           `json:"routine_probe_retries"`
}

type probePhaseTimeouts struct {
	dial     time.Duration
	response time.Duration
}

type probePhaseTimeoutsContextKey struct{}

func withProbePhaseTimeouts(ctx context.Context, dial, response time.Duration) context.Context {
	return context.WithValue(ctx, probePhaseTimeoutsContextKey{}, probePhaseTimeouts{dial: dial, response: response})
}

type ProbeBatchResult struct {
	Tag      string
	Name     string
	Latency  time.Duration
	Err      error
	Attempts int
}

type ProbeBatchSummary struct {
	Total   int
	Success int
	Failed  int
}

// probeWorkItem binds a probe to the concrete runtime generation that was
// reserved. Runtime tags can migrate while a worker is waiting for its turn;
// keeping this immutable snapshot prevents releasing or updating the new tag
// with an old outbound's result.
type probeWorkItem struct {
	entry *entry
	tag   string
	name  string
	probe probeFunc
}

type ProbeRoundStatus struct {
	InFlight  bool           `json:"in_flight"`
	Kind      ProbeRoundKind `json:"kind,omitempty"`
	StartedAt time.Time      `json:"started_at,omitempty"`
	Total     int            `json:"total"`
	Completed int            `json:"completed"`
	Success   int            `json:"success"`
	Failed    int            `json:"failed"`
}

type InitialProbeStatus struct {
	Converging bool
	Pending    int
	Queued     int
}

func normalizeProbePolicy(policy ProbePolicy) ProbePolicy {
	if policy.Concurrency < 0 || policy.Concurrency > 512 {
		policy.Concurrency = 0
	}
	if policy.StartupTimeout <= 0 {
		policy.StartupTimeout = 5 * time.Second
	}
	if policy.RoutineTimeout <= 0 {
		policy.RoutineTimeout = 10 * time.Second
	}
	if policy.DialTimeout <= 0 {
		policy.DialTimeout = 3 * time.Second
	}
	if policy.ResponseTimeout <= 0 {
		policy.ResponseTimeout = 2 * time.Second
	}
	if policy.RoutineRetries < 0 {
		policy.RoutineRetries = 0
	} else if policy.RoutineRetries > 2 {
		policy.RoutineRetries = 2
	}
	return policy
}

func autoProbeConcurrency(nodeCount int) int {
	workerLimit := (nodeCount + 9) / 10
	if workerLimit < 32 {
		workerLimit = 32
	}
	if workerLimit > 128 {
		workerLimit = 128
	}
	return workerLimit
}

func effectiveProbeConcurrency(configured, nodeCount int) int {
	workerLimit := configured
	if workerLimit <= 0 {
		workerLimit = autoProbeConcurrency(nodeCount)
	}
	if nodeCount < workerLimit {
		workerLimit = nodeCount
	}
	if workerLimit < 1 {
		return 0
	}
	return workerLimit
}

// ProbeTarget is the complete HTTP endpoint used by node health checks.
// Keeping the scheme and request URI is essential: HTTPS must perform a real
// TLS handshake, and configured paths must not be silently replaced.
type ProbeTarget struct {
	Scheme      string
	Host        string
	ServerName  string
	RequestURI  string
	Destination M.Socksaddr
}

// NodeInfo is static metadata about a proxy entry.
type NodeInfo struct {
	NodeID        int64  `json:"node_id"`
	Tag           string `json:"tag"`
	Name          string `json:"name"`
	URI           string `json:"uri"`
	Mode          string `json:"mode"`
	ListenAddress string `json:"listen_address,omitempty"`
	Port          uint16 `json:"port,omitempty"`
	Region        string `json:"region,omitempty"`  // GeoIP region code: "jp", "kr", "us", "hk", "tw", "other"
	Country       string `json:"country,omitempty"` // Full country name from GeoIP
}

// TimelineEvent represents a single usage event for debug tracking.
type TimelineEvent struct {
	Time        time.Time `json:"time"`
	Success     bool      `json:"success"`
	LatencyMs   int64     `json:"latency_ms"`
	Error       string    `json:"error,omitempty"`
	Destination string    `json:"destination,omitempty"`
}

// DebugLogEvent identifies the node associated with a timeline event.
type DebugLogEvent struct {
	NodeTag  string        `json:"node_tag"`
	NodeName string        `json:"node_name"`
	Event    TimelineEvent `json:"event"`
}

// HealthResultEvent is emitted for every completed automatic, startup, or
// manual probe. Subscribers are invoked synchronously after node state is
// committed so health transitions cannot be dropped.
type HealthResultEvent struct {
	Tag       string
	NodeID    int64
	Success   bool
	Latency   time.Duration
	Error     string
	CheckedAt time.Time
}

const maxTimelineSize = 20

// Snapshot is a runtime view of a proxy node.
type Snapshot struct {
	NodeInfo
	FailureCount      int             `json:"failure_count"`
	SuccessCount      int64           `json:"success_count"`
	Blacklisted       bool            `json:"blacklisted"`
	BlacklistedUntil  time.Time       `json:"blacklisted_until"`
	ActiveConnections int32           `json:"active_connections"`
	LastError         string          `json:"last_error,omitempty"`
	LastFailure       time.Time       `json:"last_failure,omitempty"`
	LastSuccess       time.Time       `json:"last_success,omitempty"`
	LastProbeLatency  time.Duration   `json:"last_probe_latency,omitempty"`
	LastLatencyMs     int64           `json:"last_latency_ms"`
	Available         bool            `json:"available"`
	InitialCheckDone  bool            `json:"initial_check_done"`
	TotalUpload       int64           `json:"total_upload"`
	TotalDownload     int64           `json:"total_download"`
	UploadSpeed       int64           `json:"upload_speed"`   // bytes/sec
	DownloadSpeed     int64           `json:"download_speed"` // bytes/sec
	Timeline          []TimelineEvent `json:"timeline,omitempty"`
}

type NodeTrafficSpeed struct {
	Tag           string `json:"tag"`
	UploadSpeed   int64  `json:"upload_speed"`   // bytes/sec
	DownloadSpeed int64  `json:"download_speed"` // bytes/sec
	TotalUpload   int64  `json:"total_upload"`
	TotalDownload int64  `json:"total_download"`
}

type TrafficSummary struct {
	NodeCount     int                `json:"node_count"`
	TotalUpload   int64              `json:"total_upload"`
	TotalDownload int64              `json:"total_download"`
	UploadSpeed   int64              `json:"upload_speed"`   // bytes/sec
	DownloadSpeed int64              `json:"download_speed"` // bytes/sec
	Nodes         []NodeTrafficSpeed `json:"nodes,omitempty"`
	SampledAt     time.Time          `json:"sampled_at"`
}

type probeFunc func(ctx context.Context) (time.Duration, error)
type releaseFunc func()

// DialerFunc dials a raw connection to address ("host:port") through a node's
// outbound. The signature matches http.Transport.DialContext so it can be
// plugged directly into an HTTP client whose traffic is routed via the node.
type DialerFunc func(ctx context.Context, network, address string) (net.Conn, error)

type EntryHandle struct {
	ref *entry
}

type entry struct {
	info             NodeInfo
	failure          int
	success          int64
	timeline         []TimelineEvent
	blacklist        bool
	until            time.Time
	lastError        string
	lastFail         time.Time
	lastOK           time.Time
	lastProbe        time.Duration
	lastHealthCheck  time.Time
	active           atomic.Int32
	totalUpload      atomic.Int64
	totalDownload    atomic.Int64
	uploadSpeed      int64
	downloadSpeed    int64
	lastSpeedUpload  int64
	lastSpeedDown    int64
	lastSpeedAt      time.Time
	probe            probeFunc
	release          releaseFunc
	dialer           DialerFunc
	initialCheckDone bool
	available        bool
	reloadGen        uint64 // generation counter to track active registrations
	mu               sync.RWMutex
	onTimeline       func(DebugLogEvent)
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg         Config
	reloadGen   uint64 // current reload generation
	probeTarget ProbeTarget
	probeReady  bool
	mu          sync.RWMutex
	nodes       map[string]*entry
	ctx         context.Context
	cancel      context.CancelFunc
	logger      Logger

	// periodic health check control
	healthMu          sync.Mutex
	healthInterval    time.Duration
	healthTicker      *time.Ticker
	healthIntervalC   chan time.Duration
	policyMu          sync.RWMutex
	probePolicy       ProbePolicy
	probeAllInFlight  atomic.Bool
	probeRoundMu      sync.RWMutex
	probeRound        ProbeRoundStatus
	probeTagMu        sync.Mutex
	probeTagsInFlight map[string]struct{}
	probeWake         chan struct{}
	initialMu         sync.Mutex
	initialQueue      map[string]struct{}
	initialRunning    bool
	routineRequestMu  sync.Mutex
	routinePending    bool
	routineDueOnly    bool
	debugSubMu        sync.RWMutex
	debugSubscribers  map[chan DebugLogEvent]struct{}
	healthSubMu       sync.RWMutex
	healthSubNextID   atomic.Uint64
	healthSubscribers map[uint64]func(HealthResultEvent)
	groupScheduleMu   sync.RWMutex
	groupSchedules    map[int64]groupHealthSchedule
	groupScheduleNext uint64
}

type groupHealthSchedule struct {
	tags     map[string]struct{}
	nodeIDs  map[int64]struct{}
	interval time.Duration
	token    uint64
}

// Logger interface for logging
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
}

// NewManager constructs a manager and pre-validates the probe target.
func NewManager(cfg Config) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:               cfg,
		nodes:             make(map[string]*entry),
		ctx:               ctx,
		cancel:            cancel,
		debugSubscribers:  make(map[chan DebugLogEvent]struct{}),
		probeTagsInFlight: make(map[string]struct{}),
		probeWake:         make(chan struct{}, 1),
		initialQueue:      make(map[string]struct{}),
		healthSubscribers: make(map[uint64]func(HealthResultEvent)),
		groupSchedules:    make(map[int64]groupHealthSchedule),
		probePolicy: normalizeProbePolicy(ProbePolicy{
			Concurrency: cfg.ProbeConcurrency, StartupTimeout: cfg.StartupProbeTimeout,
			RoutineTimeout: cfg.RoutineProbeTimeout, DialTimeout: cfg.ProbeDialTimeout,
			ResponseTimeout: cfg.ProbeResponseTimeout, RoutineRetries: cfg.RoutineProbeRetries,
		}),
	}
	if strings.TrimSpace(cfg.ProbeTarget) != "" {
		target, err := parseProbeTarget(cfg.ProbeTarget)
		if err != nil {
			cancel()
			return nil, err
		}
		m.probeTarget = target
		m.probeReady = true
	}
	go m.startTrafficSpeedSampler()
	go m.runProbeCoordinator()
	return m, nil
}

// SetLogger sets the logger for the manager.
func (m *Manager) SetLogger(logger Logger) {
	m.logger = logger
}

// StartPeriodicHealthCheck starts a background goroutine that periodically checks all nodes.
// interval: how often to check (e.g., 30 * time.Second)
func (m *Manager) StartPeriodicHealthCheck(interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Hour
	}

	m.healthMu.Lock()
	if m.healthIntervalC == nil {
		m.healthIntervalC = make(chan time.Duration, 1)
	}
	m.healthInterval = interval
	if m.healthTicker != nil {
		m.healthTicker.Stop()
	}
	m.healthTicker = time.NewTicker(time.Second)
	ticker := m.healthTicker
	intervalC := m.healthIntervalC
	m.healthMu.Unlock()

	go func() {
		for {
			select {
			case <-m.ctx.Done():
				return
			case newInterval := <-intervalC:
				if newInterval <= 0 {
					newInterval = 2 * time.Hour
				}
				m.healthMu.Lock()
				m.healthInterval = newInterval
				m.healthMu.Unlock()
				if m.logger != nil {
					m.logger.Info("periodic health check interval updated: ", newInterval)
				}
			case <-ticker.C:
				m.RequestDueProbesOnce()
			}
		}
	}()

	if m.logger != nil {
		m.logger.Info("periodic health check started, interval: ", interval)
	}
}

// SetHealthCheckInterval updates the periodic health check interval at runtime.
// It is safe to call before StartPeriodicHealthCheck; it will be applied on start.
func (m *Manager) SetHealthCheckInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	m.healthMu.Lock()
	m.healthInterval = d
	intervalC := m.healthIntervalC
	m.healthMu.Unlock()

	if intervalC != nil {
		select {
		case intervalC <- d:
		default:
			// drop if a newer update is already queued
		}
	}
}

// RegisterGroupHealthSchedule sets the member set and requested interval for
// one group. A shared node is probed at the shortest interval requested by
// management or any enabled group that contains it.
func (m *Manager) RegisterGroupHealthSchedule(groupID int64, tags []string, interval time.Duration) func() {
	if groupID == 0 || interval <= 0 {
		return func() {}
	}
	tagSet := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag != "" {
			tagSet[tag] = struct{}{}
		}
	}
	m.groupScheduleMu.Lock()
	m.groupScheduleNext++
	token := m.groupScheduleNext
	m.groupSchedules[groupID] = groupHealthSchedule{tags: tagSet, interval: interval, token: token}
	m.groupScheduleMu.Unlock()
	return func() {
		m.groupScheduleMu.Lock()
		if current, ok := m.groupSchedules[groupID]; ok && current.token == token {
			delete(m.groupSchedules, groupID)
		}
		m.groupScheduleMu.Unlock()
	}
}

// RegisterGroupHealthScheduleByNodeID keeps schedules stable while concrete
// runtime tags migrate between outbound generations.
func (m *Manager) RegisterGroupHealthScheduleByNodeID(groupID int64, nodeIDs []int64, interval time.Duration) func() {
	if groupID == 0 || interval <= 0 {
		return func() {}
	}
	set := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID != 0 {
			set[nodeID] = struct{}{}
		}
	}
	m.groupScheduleMu.Lock()
	m.groupScheduleNext++
	token := m.groupScheduleNext
	m.groupSchedules[groupID] = groupHealthSchedule{nodeIDs: set, interval: interval, token: token}
	m.groupScheduleMu.Unlock()
	return func() {
		m.groupScheduleMu.Lock()
		if current, ok := m.groupSchedules[groupID]; ok && current.token == token {
			delete(m.groupSchedules, groupID)
		}
		m.groupScheduleMu.Unlock()
	}
}

func (m *Manager) UnregisterGroupHealthSchedule(groupID int64) {
	if groupID == 0 {
		return
	}
	m.groupScheduleMu.Lock()
	delete(m.groupSchedules, groupID)
	m.groupScheduleMu.Unlock()
}

// probeDue reports whether a node's effective interval has elapsed. The caller
// supplies nodeID rather than letting this look it up: RunProbeBatch calls this
// while holding m.mu for reading, and a nested m.mu.RLock() deadlocks for good
// as soon as any writer (Register, MigrateRuntimeTag, SweepStaleNodes, a
// reload) is queued between the two acquisitions. sync.RWMutex does not allow
// recursive read locking.
func (m *Manager) probeDue(tag string, nodeID int64, lastCheck, now time.Time) bool {
	if lastCheck.IsZero() {
		return true
	}
	m.healthMu.Lock()
	interval := m.healthInterval
	m.healthMu.Unlock()
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	m.groupScheduleMu.RLock()
	for _, schedule := range m.groupSchedules {
		_, tagMatch := schedule.tags[tag]
		_, nodeMatch := schedule.nodeIDs[nodeID]
		if (tagMatch || nodeMatch) && schedule.interval > 0 && schedule.interval < interval {
			interval = schedule.interval
		}
	}
	m.groupScheduleMu.RUnlock()
	return !now.Before(lastCheck.Add(interval))
}

// RequestStartupProbeAllOnce triggers the one fast full round used after all
// nodes have been registered at startup or reload.
func (m *Manager) RequestStartupProbeAllOnce() {
	if _, ready := m.TargetForProbe(); !ready {
		m.mu.RLock()
		tags := make([]string, 0, len(m.nodes))
		for tag := range m.nodes {
			tags = append(tags, tag)
		}
		m.mu.RUnlock()
		for _, tag := range tags {
			_ = m.MarkAvailableWithoutProbe(tag)
		}
		return
	}
	m.mu.RLock()
	tags := make([]string, 0, len(m.nodes))
	for tag := range m.nodes {
		tags = append(tags, tag)
	}
	m.mu.RUnlock()
	m.enqueueInitialProbeTags(tags)
}

// RequestRoutineProbeAllOnce triggers a periodic-policy full round.
func (m *Manager) RequestRoutineProbeAllOnce() {
	m.requestProbeBatch(false)
}

// RequestDueProbesOnce checks only nodes whose effective management/group
// interval has elapsed. Batch rounds never overlap.
func (m *Manager) RequestDueProbesOnce() {
	m.requestProbeBatch(true)
}

func (m *Manager) requestProbeBatch(dueOnly bool) {
	if _, ready := m.TargetForProbe(); !ready {
		return
	}
	m.routineRequestMu.Lock()
	if !m.routinePending {
		m.routineDueOnly = dueOnly
	} else if !dueOnly {
		// A full request subsumes every due-only request accumulated while a
		// different round owns the global gate.
		m.routineDueOnly = false
	}
	m.routinePending = true
	m.routineRequestMu.Unlock()
	m.signalProbeCoordinator()
}

func (m *Manager) signalProbeCoordinator() {
	select {
	case m.probeWake <- struct{}{}:
	default:
	}
}

func (m *Manager) enqueueInitialProbeTags(tags []string) {
	if len(tags) == 0 {
		return
	}
	started := false
	added := 0
	m.initialMu.Lock()
	if !m.initialRunning {
		m.initialRunning = true
		started = true
	}
	for _, tag := range tags {
		if tag != "" {
			if _, exists := m.initialQueue[tag]; !exists {
				m.initialQueue[tag] = struct{}{}
				added++
			}
		}
	}
	queued := len(m.initialQueue)
	m.initialMu.Unlock()
	if started && m.logger != nil {
		m.logger.Info("initial health convergence started with ", queued, " queued nodes")
	} else if added > 0 && m.logger != nil {
		m.logger.Info("initial health convergence queued ", added, " additional nodes; ", queued, " waiting")
	}
	m.signalProbeCoordinator()
}

func (m *Manager) runProbeCoordinator() {
	// The ticker is a low-frequency safety net for lost/coalesced notifications.
	// Correctness does not depend on it during normal operation, but a pending
	// convergence can always recover even if no further reload or lease-release
	// event arrives to wake the coordinator.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.probeWake:
		case <-ticker.C:
		}

		for m.ctx.Err() == nil {
			ranInitial, blockedInitial := m.runInitialConvergenceBatch()
			if ranInitial {
				continue
			}
			if blockedInitial {
				break
			}
			ranRoutine, blockedRoutine := m.runPendingRoutineRound()
			if ranRoutine {
				continue
			}
			if blockedRoutine {
				break
			}
			break
		}
	}
}

// runInitialConvergenceBatch returns whether a batch ran and whether work is
// waiting on the global round gate or a per-tag probe lease.
func (m *Manager) runInitialConvergenceBatch() (bool, bool) {
	m.initialMu.Lock()
	active := m.initialRunning
	m.initialMu.Unlock()
	if !active {
		return false, false
	}
	if !m.probeAllInFlight.CompareAndSwap(false, true) {
		return false, true
	}

	items, pending, waiting := m.collectInitialProbeItems()
	if len(items) == 0 {
		m.probeAllInFlight.Store(false)
		if pending == 0 {
			m.finishInitialConvergence()
			return false, false
		}
		return false, waiting
	}

	policy := m.ProbePolicy()
	m.probeRoundMu.Lock()
	m.probeRound = ProbeRoundStatus{InFlight: true, Kind: ProbeRoundStartup, StartedAt: time.Now(), Total: len(items)}
	m.probeRoundMu.Unlock()
	defer func() {
		m.probeAllInFlight.Store(false)
		m.probeRoundMu.Lock()
		m.probeRound.InFlight = false
		m.probeRoundMu.Unlock()
		m.signalProbeCoordinator()
	}()

	workerLimit := effectiveProbeConcurrency(policy.Concurrency, len(items))
	results := collectLimited(workerLimit, items, func(item probeWorkItem) ProbeBatchResult {
		return m.probeInitialItem(m.ctx, item, policy)
	})
	summary := ProbeBatchSummary{Total: len(items)}
	for result := range results {
		if result.Err != nil {
			summary.Failed++
			if m.logger != nil && !errors.Is(result.Err, context.Canceled) {
				m.logger.Warn("initial probe failed for ", result.Tag, ": ", result.Err)
			}
		} else {
			summary.Success++
		}
		m.probeRoundMu.Lock()
		m.probeRound.Completed++
		m.probeRound.Success = summary.Success
		m.probeRound.Failed = summary.Failed
		m.probeRoundMu.Unlock()
	}
	if m.logger != nil {
		m.logger.Info("initial health convergence batch completed: ", summary.Success, " available, ", summary.Failed, " unavailable")
	}
	m.refreshInitialQueue()
	return true, false
}

func (m *Manager) collectInitialProbeItems() ([]probeWorkItem, int, bool) {
	m.mu.RLock()
	current := make(map[string]probeWorkItem, len(m.nodes))
	pendingTags := make([]string, 0, len(m.nodes))
	for tag, item := range m.nodes {
		item.mu.RLock()
		work := probeWorkItem{entry: item, tag: tag, name: item.info.Name, probe: item.probe}
		pending := !item.initialCheckDone
		item.mu.RUnlock()
		current[tag] = work
		if pending {
			pendingTags = append(pendingTags, tag)
		}
	}
	m.mu.RUnlock()
	pendingSet := make(map[string]struct{}, len(pendingTags))
	for _, tag := range pendingTags {
		pendingSet[tag] = struct{}{}
	}

	m.initialMu.Lock()
	for tag := range m.initialQueue {
		if _, exists := pendingSet[tag]; !exists {
			delete(m.initialQueue, tag)
		}
	}
	for _, tag := range pendingTags {
		m.initialQueue[tag] = struct{}{}
	}
	queuedTags := make([]string, 0, len(m.initialQueue))
	for tag := range m.initialQueue {
		queuedTags = append(queuedTags, tag)
	}
	m.initialMu.Unlock()

	items := make([]probeWorkItem, 0, len(queuedTags))
	m.probeTagMu.Lock()
	for _, tag := range queuedTags {
		work, exists := current[tag]
		if !exists {
			continue
		}
		if _, busy := m.probeTagsInFlight[tag]; busy {
			continue
		}
		m.probeTagsInFlight[tag] = struct{}{}
		items = append(items, work)
	}
	m.probeTagMu.Unlock()
	if len(items) > 0 {
		m.initialMu.Lock()
		for _, item := range items {
			delete(m.initialQueue, item.tag)
		}
		m.initialMu.Unlock()
	}
	return items, len(pendingTags), len(items) == 0 && len(queuedTags) > 0
}

func (m *Manager) probeInitialItem(ctx context.Context, item probeWorkItem, policy ProbePolicy) ProbeBatchResult {
	result := ProbeBatchResult{Tag: item.tag, Name: item.name}
	defer m.endTagProbe(item.tag)
	if ctx.Err() != nil {
		result.Err = ctx.Err()
		return result
	}
	if item.probe == nil {
		result.Attempts = 1
		result.Err = errors.New("probe function not configured")
	} else {
		for attempt := 1; attempt <= 2; attempt++ {
			if ctx.Err() != nil {
				result.Err = ctx.Err()
				break
			}
			attemptCtx, cancel := context.WithTimeout(ctx, policy.StartupTimeout)
			attemptCtx = withProbePhaseTimeouts(attemptCtx, policy.DialTimeout, policy.ResponseTimeout)
			result.Attempts = attempt
			result.Latency, result.Err = item.probe(attemptCtx)
			cancel()
			if result.Err == nil || ctx.Err() != nil || attempt == 2 {
				break
			}
			timer := time.NewTimer(initialProbeRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				result.Err = ctx.Err()
			case <-timer.C:
			}
		}
	}
	if ctx.Err() == nil && entryRuntimeTag(item.entry) == item.tag {
		m.applyHealthResult(item.entry, result.Latency, result.Err, time.Now())
	}
	return result
}

func (m *Manager) refreshInitialQueue() {
	m.mu.RLock()
	pending := make([]string, 0, len(m.nodes))
	for tag, item := range m.nodes {
		item.mu.RLock()
		isPending := !item.initialCheckDone
		item.mu.RUnlock()
		if isPending {
			pending = append(pending, tag)
		}
	}
	m.mu.RUnlock()
	if len(pending) == 0 {
		m.finishInitialConvergence()
		return
	}
	m.initialMu.Lock()
	if m.initialRunning {
		for _, tag := range pending {
			m.initialQueue[tag] = struct{}{}
		}
	}
	m.initialMu.Unlock()
}

func (m *Manager) finishInitialConvergence() {
	finished := false
	m.initialMu.Lock()
	// Revalidate while enqueue operations are excluded. A subscription may
	// register a new pending generation between the preceding batch scan and
	// this transition; its follow-up enqueue will either be visible here or
	// reopen convergence after this lock is released.
	m.mu.RLock()
	pending := make([]string, 0)
	for tag, item := range m.nodes {
		item.mu.RLock()
		isPending := !item.initialCheckDone
		item.mu.RUnlock()
		if isPending {
			pending = append(pending, tag)
		}
	}
	m.mu.RUnlock()
	if len(pending) > 0 {
		for _, tag := range pending {
			m.initialQueue[tag] = struct{}{}
		}
		m.initialMu.Unlock()
		m.signalProbeCoordinator()
		return
	}
	if m.initialRunning {
		m.initialRunning = false
		clear(m.initialQueue)
		finished = true
	}
	m.initialMu.Unlock()
	if finished && m.logger != nil {
		m.logger.Info("initial health convergence completed: pending nodes reached zero")
	}
}

func (m *Manager) runPendingRoutineRound() (bool, bool) {
	m.routineRequestMu.Lock()
	if !m.routinePending {
		m.routineRequestMu.Unlock()
		return false, false
	}
	dueOnly := m.routineDueOnly
	m.routinePending = false
	m.routineDueOnly = false
	m.routineRequestMu.Unlock()

	_, err := m.RunProbeBatch(m.ctx, ProbeRoundPeriodic, dueOnly, nil, nil)
	if errors.Is(err, ErrProbeRoundInProgress) {
		m.routineRequestMu.Lock()
		if !m.routinePending {
			m.routineDueOnly = dueOnly
		} else if !dueOnly {
			m.routineDueOnly = false
		}
		m.routinePending = true
		m.routineRequestMu.Unlock()
		return false, true
	}
	if err != nil && !errors.Is(err, context.Canceled) && m.logger != nil {
		m.logger.Warn("probe round failed: ", err)
	}
	return true, false
}

// RunProbeBatch executes one mutually exclusive worker-pool round. The result
// callback is serialized on the caller goroutine, which makes it safe for SSE.
func (m *Manager) RunProbeBatch(ctx context.Context, kind ProbeRoundKind, dueOnly bool,
	onStart func(total int), onResult func(ProbeBatchResult)) (ProbeBatchSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ready := m.TargetForProbe(); !ready {
		return ProbeBatchSummary{}, errors.New("probe target not configured")
	}
	m.mu.RLock()
	items := make([]probeWorkItem, 0, len(m.nodes))
	for _, e := range m.nodes {
		e.mu.RLock()
		probeFn := e.probe
		lastHealthCheck := e.lastHealthCheck
		tag := e.info.Tag
		nodeID := e.info.NodeID
		e.mu.RUnlock()
		if probeFn == nil || (dueOnly && !m.probeDue(tag, nodeID, lastHealthCheck, time.Now())) {
			continue
		}
		items = append(items, probeWorkItem{entry: e, tag: tag, name: e.info.Name, probe: probeFn})
	}
	m.mu.RUnlock()

	if !m.probeAllInFlight.CompareAndSwap(false, true) {
		return ProbeBatchSummary{}, ErrProbeRoundInProgress
	}
	items = m.reserveBatchProbeItems(items)
	policy := m.ProbePolicy()
	timeout, retries := policy.RoutineTimeout, policy.RoutineRetries
	if kind == ProbeRoundStartup {
		timeout, retries = policy.StartupTimeout, 0
	}
	status := ProbeRoundStatus{InFlight: true, Kind: kind, StartedAt: time.Now(), Total: len(items)}
	m.probeRoundMu.Lock()
	m.probeRound = status
	m.probeRoundMu.Unlock()
	defer func() {
		m.probeAllInFlight.Store(false)
		m.probeRoundMu.Lock()
		m.probeRound.InFlight = false
		m.probeRoundMu.Unlock()
		m.signalProbeCoordinator()
	}()
	if onStart != nil {
		onStart(len(items))
	}
	if len(items) == 0 {
		return ProbeBatchSummary{}, nil
	}

	if m.logger != nil {
		m.logger.Info("starting ", kind, " health check for ", len(items), " nodes")
	}
	workerLimit := effectiveProbeConcurrency(policy.Concurrency, len(items))
	results := collectLimited(workerLimit, items, func(item probeWorkItem) ProbeBatchResult {
		if kind == ProbeRoundStartup {
			return m.probeInitialItem(ctx, item, policy)
		}
		return m.probeBatchItem(ctx, item, timeout, retries, policy.DialTimeout, policy.ResponseTimeout)
	})
	summary := ProbeBatchSummary{Total: len(items)}
	for result := range results {
		if result.Err != nil {
			summary.Failed++
			if m.logger != nil {
				m.logger.Warn("probe failed for ", result.Tag, ": ", result.Err)
			}
		} else {
			summary.Success++
		}
		m.probeRoundMu.Lock()
		m.probeRound.Completed++
		m.probeRound.Success = summary.Success
		m.probeRound.Failed = summary.Failed
		m.probeRoundMu.Unlock()
		if onResult != nil {
			onResult(result)
		}
	}
	if m.logger != nil {
		m.logger.Info(kind, " health check completed: ", summary.Success, " available, ", summary.Failed, " failed")
	}
	return summary, ctx.Err()
}

func (m *Manager) probeBatchItem(ctx context.Context, item probeWorkItem, timeout time.Duration, retries int, dialTimeout, responseTimeout time.Duration) ProbeBatchResult {
	result := ProbeBatchResult{Tag: item.tag, Name: item.name}
	defer m.endTagProbe(item.tag)
	if item.probe == nil {
		result.Err = errors.New("probe function not configured")
		return result
	}
	nodeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nodeCtx = withProbePhaseTimeouts(nodeCtx, dialTimeout, responseTimeout)
	for attempt := 0; attempt <= retries; attempt++ {
		result.Attempts = attempt + 1
		result.Latency, result.Err = item.probe(nodeCtx)
		if result.Err == nil || nodeCtx.Err() != nil {
			break
		}
	}
	if ctx.Err() == nil && entryRuntimeTag(item.entry) == result.Tag {
		m.applyHealthResult(item.entry, result.Latency, result.Err, time.Now())
	}
	return result
}

func (m *Manager) reserveBatchProbeItems(items []probeWorkItem) []probeWorkItem {
	m.probeTagMu.Lock()
	defer m.probeTagMu.Unlock()
	reserved := items[:0]
	for _, item := range items {
		if _, exists := m.probeTagsInFlight[item.tag]; exists {
			continue
		}
		m.probeTagsInFlight[item.tag] = struct{}{}
		reserved = append(reserved, item)
	}
	return reserved
}

func (m *Manager) ProbePolicy() ProbePolicy {
	m.policyMu.RLock()
	defer m.policyMu.RUnlock()
	return m.probePolicy
}

func (m *Manager) UpdateProbePolicy(policy ProbePolicy) {
	m.policyMu.Lock()
	m.probePolicy = normalizeProbePolicy(policy)
	m.policyMu.Unlock()
}

func (m *Manager) ProbePhaseTimeouts() (time.Duration, time.Duration) {
	policy := m.ProbePolicy()
	return policy.DialTimeout, policy.ResponseTimeout
}

// ProbePhaseTimeoutsFor returns the round snapshot carried by ctx, falling
// back to the current policy for standalone probes.
func (m *Manager) ProbePhaseTimeoutsFor(ctx context.Context) (time.Duration, time.Duration) {
	if ctx != nil {
		if snapshot, ok := ctx.Value(probePhaseTimeoutsContextKey{}).(probePhaseTimeouts); ok {
			return snapshot.dial, snapshot.response
		}
	}
	return m.ProbePhaseTimeouts()
}

func (m *Manager) ProbeRoundStatus() ProbeRoundStatus {
	m.probeRoundMu.RLock()
	defer m.probeRoundMu.RUnlock()
	return m.probeRound
}

func (m *Manager) ProbeNodeCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.nodes)
}

func (m *Manager) InitialProbeStatus() InitialProbeStatus {
	m.mu.RLock()
	pending := 0
	for _, item := range m.nodes {
		item.mu.RLock()
		if !item.initialCheckDone {
			pending++
		}
		item.mu.RUnlock()
	}
	m.mu.RUnlock()
	m.initialMu.Lock()
	status := InitialProbeStatus{Converging: m.initialRunning, Pending: pending, Queued: len(m.initialQueue)}
	m.initialMu.Unlock()
	return status
}

// Stop stops the periodic health check.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Manager) startTrafficSpeedSampler() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.sampleTrafficSpeeds(now)
		}
	}
}

func (m *Manager) sampleTrafficSpeeds(now time.Time) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	for _, e := range entries {
		e.updateTrafficSpeed(now)
	}
}

// BeginReload bumps the generation counter. Nodes registered after this call
// will be marked with the new generation. Call SweepStaleNodes after reload
// to remove nodes that were not re-registered (disabled/deleted nodes).
func (m *Manager) BeginReload() {
	m.mu.Lock()
	m.reloadGen++
	m.mu.Unlock()
}

// SweepStaleNodes removes nodes that were not re-registered during the current
// reload cycle. This preserves monitoring data (latency, success/failure counts)
// for nodes that are still active, while cleaning up disabled/removed nodes.
func (m *Manager) SweepStaleNodes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tag, e := range m.nodes {
		e.mu.RLock()
		generation := e.reloadGen
		e.mu.RUnlock()
		if generation != m.reloadGen {
			delete(m.nodes, tag)
		}
	}
	m.signalProbeCoordinator()
}

// ClearNodes removes all registered nodes. Use BeginReload + SweepStaleNodes
// for reload scenarios to preserve data for active nodes.
func (m *Manager) ClearNodes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes = make(map[string]*entry)
}

// Register ensures a node is tracked and returns its entry.
// If the node already exists, its info is updated but monitoring stats
// (latency, success/failure counts, etc.) are preserved.
func (m *Manager) Register(info NodeInfo) *EntryHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.nodes[info.Tag]
	if !ok {
		e = &entry{
			info:       info,
			timeline:   make([]TimelineEvent, 0, maxTimelineSize),
			reloadGen:  m.reloadGen,
			onTimeline: m.publishDebugLog,
		}
		m.nodes[info.Tag] = e
	} else {
		e.mu.Lock()
		e.info = info
		e.reloadGen = m.reloadGen
		e.onTimeline = m.publishDebugLog
		e.mu.Unlock()
	}
	return &EntryHandle{ref: e}
}

// MigrateRuntimeTag atomically moves the monitor identity of a stable node to
// a new concrete runtime tag. The entry object is retained, so latency,
// counters, traffic and timeline history survive an outbound replacement.
// If no entry with nodeID exists this behaves like Register.
func (m *Manager) MigrateRuntimeTag(nodeID int64, info NodeInfo) *EntryHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.nodes[info.Tag]; existing != nil {
		existing.mu.Lock()
		existing.info = info
		existing.reloadGen = m.reloadGen
		existing.onTimeline = m.publishDebugLog
		existing.mu.Unlock()
		return &EntryHandle{ref: existing}
	}
	var oldTag string
	var existing *entry
	if nodeID != 0 {
		for tag, candidate := range m.nodes {
			candidate.mu.RLock()
			matches := candidate.info.NodeID == nodeID
			candidate.mu.RUnlock()
			if matches {
				oldTag, existing = tag, candidate
				break
			}
		}
	}
	if existing == nil {
		existing = &entry{timeline: make([]TimelineEvent, 0, maxTimelineSize), onTimeline: m.publishDebugLog}
	} else {
		delete(m.nodes, oldTag)
	}
	existing.mu.Lock()
	existing.info = info
	existing.reloadGen = m.reloadGen
	existing.onTimeline = m.publishDebugLog
	if oldTag != "" && oldTag != info.Tag {
		// Health belongs to the concrete outbound generation. Keep historical
		// counters/timeline/latency for the stable node, but require the new
		// runtime to prove availability before it becomes a pool candidate.
		existing.initialCheckDone = false
		existing.available = false
		existing.lastHealthCheck = time.Time{}
	}
	existing.mu.Unlock()
	m.nodes[info.Tag] = existing
	return &EntryHandle{ref: existing}
}

// UnregisterRuntimeTag removes a retired runtime identity if it is still the
// map owner. A migrated entry is keyed by its new tag and is left untouched.
func (m *Manager) UnregisterRuntimeTag(tag string) {
	m.mu.Lock()
	if existing := m.nodes[tag]; existing != nil {
		existing.mu.RLock()
		currentTag := existing.info.Tag
		existing.mu.RUnlock()
		if currentTag == tag {
			delete(m.nodes, tag)
		}
	}
	m.mu.Unlock()
	m.signalProbeCoordinator()
}

// RequestProbeTagsOnce adds concrete runtime generations to the same bounded
// initial-convergence queue used by startup and full reloads.
func (m *Manager) RequestProbeTagsOnce(tags []string) {
	if len(tags) == 0 {
		return
	}
	if _, ready := m.TargetForProbe(); !ready {
		for _, tag := range tags {
			_ = m.MarkAvailableWithoutProbe(tag)
		}
		return
	}
	m.enqueueInitialProbeTags(tags)
}

// HandleForTag returns an existing monitor entry without changing its reload
// generation, callbacks, or node metadata. Independent group runtimes use this
// to observe the base runtime's health state without becoming probe owners.
func (m *Manager) HandleForTag(tag string) *EntryHandle {
	m.mu.RLock()
	entry := m.nodes[tag]
	m.mu.RUnlock()
	if entry == nil {
		return nil
	}
	return &EntryHandle{ref: entry}
}

// HandleForNodeID resolves the current monitor owner for a stable node.
func (m *Manager) HandleForNodeID(nodeID int64) *EntryHandle {
	if nodeID == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.nodes {
		item.mu.RLock()
		matches := item.info.NodeID == nodeID
		item.mu.RUnlock()
		if matches {
			return &EntryHandle{ref: item}
		}
	}
	return nil
}

// DestinationForProbe exposes the configured destination for health checks.
func (m *Manager) DestinationForProbe() (M.Socksaddr, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.probeReady {
		return M.Socksaddr{}, false
	}
	return m.probeTarget.Destination, true
}

// TargetForProbe returns the full endpoint needed for a scheme-aware HTTP
// health check. Callers should prefer this over DestinationForProbe.
func (m *Manager) TargetForProbe() (ProbeTarget, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.probeReady {
		return ProbeTarget{}, false
	}
	return m.probeTarget, true
}

// UpdateProbeTarget dynamically updates the probe destination at runtime.
func (m *Manager) UpdateProbeTarget(target string) error {
	if err := m.SetProbeTarget(target); err != nil {
		return err
	}
	m.RequestRoutineProbeAllOnce()
	return nil
}

// SetProbeTarget updates the destination without scheduling a round. Reload
// uses this so the target is applied before its single startup-policy round.
func (m *Manager) SetProbeTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		m.mu.Lock()
		m.probeTarget = ProbeTarget{}
		m.probeReady = false
		m.cfg.ProbeTarget = ""
		m.mu.Unlock()
		return nil
	}
	parsed, err := parseProbeTarget(target)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.probeTarget = parsed
	m.probeReady = true
	m.cfg.ProbeTarget = target
	m.mu.Unlock()
	return nil
}

func parseProbeTarget(raw string) (ProbeTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ProbeTarget{}, errors.New("probe target is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ProbeTarget{}, fmt.Errorf("parse probe target: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ProbeTarget{}, fmt.Errorf("unsupported probe target scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return ProbeTarget{}, errors.New("probe target must not contain credentials")
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return ProbeTarget{}, errors.New("probe target is missing host")
	}
	port := uint16(80)
	if scheme == "https" {
		port = 443
	}
	if rawPort := parsed.Port(); rawPort != "" {
		value, parseErr := strconv.ParseUint(rawPort, 10, 16)
		if parseErr != nil || value == 0 {
			return ProbeTarget{}, fmt.Errorf("probe target has invalid port %q", rawPort)
		}
		port = uint16(value)
	}
	requestURI := parsed.EscapedPath()
	if requestURI == "" {
		requestURI = "/generate_204"
	}
	if parsed.RawQuery != "" {
		requestURI += "?" + parsed.RawQuery
	}
	destination := M.ParseSocksaddrHostPort(hostname, port)
	if !destination.IsValid() {
		return ProbeTarget{}, errors.New("probe target destination is invalid")
	}
	return ProbeTarget{Scheme: scheme, Host: parsed.Host, ServerName: hostname, RequestURI: requestURI, Destination: destination}, nil
}

// Snapshot returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
func (m *Manager) Snapshot() []Snapshot {
	return m.SnapshotFiltered(false)
}

// UpdateNodeLocation refreshes the landing-IP-derived location metadata for
// every runtime registration of a stable node ID. It does not alter health,
// failure counters, or routing availability.
func (m *Manager) UpdateNodeLocation(nodeID int64, region, country string) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, item := range m.nodes {
		entries = append(entries, item)
	}
	m.mu.RUnlock()
	for _, item := range entries {
		item.mu.Lock()
		if item.info.NodeID == nodeID {
			item.info.Region = strings.ToLower(strings.TrimSpace(region))
			item.info.Country = strings.TrimSpace(country)
		}
		item.mu.Unlock()
	}
}

// SnapshotForTag returns a snapshot of a single node by tag, or nil if the
// node is not registered.
func (m *Manager) SnapshotForTag(tag string) *Snapshot {
	m.mu.RLock()
	e, ok := m.nodes[tag]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	snap := e.snapshot()
	return &snap
}

// SnapshotForNodeID returns health for the current runtime generation of a
// stable node, independent of a group box's concrete tag.
func (m *Manager) SnapshotForNodeID(nodeID int64) *Snapshot {
	handle := m.HandleForNodeID(nodeID)
	if handle == nil {
		return nil
	}
	snapshot := handle.ref.snapshot()
	return &snapshot
}

// SnapshotFiltered returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
// Nodes that haven't been checked yet are also included (they will be checked on first use).
func (m *Manager) SnapshotFiltered(onlyAvailable bool) []Snapshot {
	m.mu.RLock()
	list := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		list = append(list, e)
	}
	m.mu.RUnlock()
	snapshots := make([]Snapshot, 0, len(list))
	for _, e := range list {
		snap := e.snapshot()
		// 如果只要可用节点：
		// - 跳过已完成检查但不可用的节点
		// - 保留未完成检查的节点（它们会在首次使用时被检查）
		if onlyAvailable && snap.InitialCheckDone && !snap.Available {
			continue
		}
		snapshots = append(snapshots, snap)
	}
	// 按延迟排序（延迟小的在前面，未测试的排在最后）
	sort.Slice(snapshots, func(i, j int) bool {
		latencyI := snapshots[i].LastLatencyMs
		latencyJ := snapshots[j].LastLatencyMs
		// -1 表示未测试，排在最后
		if latencyI < 0 && latencyJ < 0 {
			return snapshots[i].Name < snapshots[j].Name // 都未测试时按名称排序
		}
		if latencyI < 0 {
			return false // i 未测试，排在后面
		}
		if latencyJ < 0 {
			return true // j 未测试，i 排在前面
		}
		if latencyI == latencyJ {
			return snapshots[i].Name < snapshots[j].Name // 延迟相同时按名称排序
		}
		return latencyI < latencyJ
	})
	return snapshots
}

// TrafficSummary returns aggregated traffic totals/speeds and per-node speeds.
// includeNodes controls whether per-node details are returned.
func (m *Manager) TrafficSummary(includeNodes bool) TrafficSummary {
	m.mu.RLock()
	list := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		list = append(list, e)
	}
	m.mu.RUnlock()

	summary := TrafficSummary{
		NodeCount: len(list),
		SampledAt: time.Now(),
	}
	if includeNodes {
		summary.Nodes = make([]NodeTrafficSpeed, 0, len(list))
	}

	for _, e := range list {
		totalUp := e.totalUpload.Load()
		totalDown := e.totalDownload.Load()

		e.mu.RLock()
		upSpeed := e.uploadSpeed
		downSpeed := e.downloadSpeed
		tag := e.info.Tag
		e.mu.RUnlock()

		summary.TotalUpload += totalUp
		summary.TotalDownload += totalDown
		summary.UploadSpeed += upSpeed
		summary.DownloadSpeed += downSpeed

		if includeNodes {
			summary.Nodes = append(summary.Nodes, NodeTrafficSpeed{
				Tag:           tag,
				UploadSpeed:   upSpeed,
				DownloadSpeed: downSpeed,
				TotalUpload:   totalUp,
				TotalDownload: totalDown,
			})
		}
	}

	return summary
}

// Probe triggers a manual health check.
func (m *Manager) Probe(ctx context.Context, tag string) (time.Duration, error) {
	e, err := m.entry(tag)
	if err != nil {
		return 0, err
	}
	e.mu.RLock()
	probe := e.probe
	e.mu.RUnlock()
	if probe == nil {
		return 0, errors.New("probe not available for this node")
	}
	if !m.beginTagProbe(tag) {
		return 0, errors.New("probe already in progress for this node")
	}
	defer m.endTagProbe(tag)
	policy := m.ProbePolicy()
	probeCtx, cancel := context.WithTimeout(ctx, policy.RoutineTimeout)
	defer cancel()
	probeCtx = withProbePhaseTimeouts(probeCtx, policy.DialTimeout, policy.ResponseTimeout)
	var latency time.Duration
	for attempt := 0; attempt <= policy.RoutineRetries; attempt++ {
		latency, err = probe(probeCtx)
		if err == nil || probeCtx.Err() != nil {
			break
		}
	}
	if entryRuntimeTag(e) == tag {
		m.applyHealthResult(e, latency, err, time.Now())
	}
	if err != nil {
		return 0, err
	}
	return latency, nil
}

func entryRuntimeTag(e *entry) string {
	if e == nil {
		return ""
	}
	e.mu.RLock()
	tag := e.info.Tag
	e.mu.RUnlock()
	return tag
}

func (m *Manager) beginTagProbe(tag string) bool {
	m.probeTagMu.Lock()
	defer m.probeTagMu.Unlock()
	if _, exists := m.probeTagsInFlight[tag]; exists {
		return false
	}
	m.probeTagsInFlight[tag] = struct{}{}
	return true
}

func (m *Manager) endTagProbe(tag string) {
	m.probeTagMu.Lock()
	delete(m.probeTagsInFlight, tag)
	m.probeTagMu.Unlock()
	m.signalProbeCoordinator()
}

func (m *Manager) applyHealthResult(e *entry, latency time.Duration, probeErr error, checkedAt time.Time) {
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	destination := ""
	if dst, ok := m.DestinationForProbe(); ok {
		destination = dst.String()
	}
	if probeErr != nil {
		e.recordFailure(probeErr, destination)
		e.mu.Lock()
		e.available = false
		e.initialCheckDone = true
		e.lastProbe = 0
		e.lastHealthCheck = checkedAt
		e.mu.Unlock()
	} else {
		e.recordSuccessWithLatency(latency)
		e.mu.Lock()
		e.lastHealthCheck = checkedAt
		e.mu.Unlock()
	}
	e.mu.RLock()
	tag, nodeID := e.info.Tag, e.info.NodeID
	e.mu.RUnlock()
	event := HealthResultEvent{Tag: tag, NodeID: nodeID, Success: probeErr == nil, Latency: latency, CheckedAt: checkedAt}
	if probeErr != nil {
		event.Error = probeErr.Error()
	}
	m.publishHealthResult(event)
}

// MarkAvailableWithoutProbe keeps the existing compatibility behavior used
// when no probe target is configured while still notifying group schedulers.
func (m *Manager) MarkAvailableWithoutProbe(tag string) error {
	e, err := m.entry(tag)
	if err != nil {
		return err
	}
	e.mu.RLock()
	alreadyAvailable := e.initialCheckDone && e.available
	e.mu.RUnlock()
	if alreadyAvailable {
		return nil
	}
	m.applyHealthResult(e, 0, nil, time.Now())
	return nil
}

// Release clears blacklist state for the given node.
func (m *Manager) Release(tag string) error {
	e, err := m.entry(tag)
	if err != nil {
		return err
	}
	if e.release == nil {
		return errors.New("release not available for this node")
	}
	e.release()
	return nil
}

// DialerFor returns the registered dial-through function for a node, if any.
// The returned function dials a raw connection to "host:port" via the node's
// outbound; nil is returned when the node has no dialer wired (e.g. the pool
// outbound was not started yet).
func (m *Manager) DialerFor(tag string) (DialerFunc, error) {
	e, err := m.entry(tag)
	if err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.dialer == nil {
		return nil, errors.New("dialer not available for this node")
	}
	return e.dialer, nil
}

func (m *Manager) entry(tag string) (*entry, error) {
	m.mu.RLock()
	e, ok := m.nodes[tag]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("node %s not found", tag)
	}
	return e, nil
}

func (e *entry) snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	latencyMs := int64(-1)
	if e.lastProbe > 0 {
		latencyMs = e.lastProbe.Milliseconds()
		if latencyMs == 0 {
			latencyMs = 1
		}
	}

	var timelineCopy []TimelineEvent
	if len(e.timeline) > 0 {
		timelineCopy = make([]TimelineEvent, len(e.timeline))
		copy(timelineCopy, e.timeline)
	}

	return Snapshot{
		NodeInfo:          e.info,
		FailureCount:      e.failure,
		SuccessCount:      e.success,
		Blacklisted:       e.blacklist,
		BlacklistedUntil:  e.until,
		ActiveConnections: e.active.Load(),
		LastError:         e.lastError,
		LastFailure:       e.lastFail,
		LastSuccess:       e.lastOK,
		LastProbeLatency:  e.lastProbe,
		LastLatencyMs:     latencyMs,
		Available:         e.available,
		InitialCheckDone:  e.initialCheckDone,
		TotalUpload:       e.totalUpload.Load(),
		TotalDownload:     e.totalDownload.Load(),
		UploadSpeed:       e.uploadSpeed,
		DownloadSpeed:     e.downloadSpeed,
		Timeline:          timelineCopy,
	}
}

func (e *entry) recordFailure(err error, destination string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	errStr := err.Error()
	e.failure++
	e.lastError = errStr
	e.lastFail = time.Now()
	// 注意：不修改 available/initialCheckDone
	// 流量失败不代表节点不可用（可能是目标网站的问题）
	// available 只由探测操作控制
	e.appendTimelineLocked(false, 0, errStr, destination)
}

func (e *entry) recordSuccess(destination string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	// 注意：不修改 available/initialCheckDone
	// 流量成功不代表需要更新探测状态
	// available 只由探测操作控制
	e.appendTimelineLocked(true, 0, "", destination)
}

func (e *entry) recordSuccessWithLatency(latency time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.success++
	e.lastOK = time.Now()
	e.lastProbe = latency
	e.available = true
	e.initialCheckDone = true
	latencyMs := latency.Milliseconds()
	if latencyMs == 0 && latency > 0 {
		latencyMs = 1
	}
	e.appendTimelineLocked(true, latencyMs, "", "")
}

func (e *entry) appendTimelineLocked(success bool, latencyMs int64, errStr string, destination string) {
	evt := TimelineEvent{
		Time:        time.Now(),
		Success:     success,
		LatencyMs:   latencyMs,
		Error:       errStr,
		Destination: destination,
	}
	if len(e.timeline) >= maxTimelineSize {
		copy(e.timeline, e.timeline[1:])
		e.timeline[len(e.timeline)-1] = evt
	} else {
		e.timeline = append(e.timeline, evt)
	}
	if e.onTimeline != nil {
		e.onTimeline(DebugLogEvent{NodeTag: e.info.Tag, NodeName: e.info.Name, Event: evt})
	}
}

// SubscribeDebugLogs returns a bounded real-time event stream and an unsubscribe function.
func (m *Manager) SubscribeDebugLogs() (<-chan DebugLogEvent, func()) {
	ch := make(chan DebugLogEvent, 128)
	m.debugSubMu.Lock()
	m.debugSubscribers[ch] = struct{}{}
	m.debugSubMu.Unlock()
	return ch, func() {
		m.debugSubMu.Lock()
		delete(m.debugSubscribers, ch)
		m.debugSubMu.Unlock()
	}
}

func (m *Manager) publishDebugLog(event DebugLogEvent) {
	m.debugSubMu.RLock()
	defer m.debugSubMu.RUnlock()
	for subscriber := range m.debugSubscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

// SubscribeHealthResults installs a lossless in-process health observer.
// Callbacks run outside manager and entry locks and must remain lightweight.
func (m *Manager) SubscribeHealthResults(callback func(HealthResultEvent)) func() {
	if callback == nil {
		return func() {}
	}
	id := m.healthSubNextID.Add(1)
	m.healthSubMu.Lock()
	m.healthSubscribers[id] = callback
	m.healthSubMu.Unlock()
	return func() {
		m.healthSubMu.Lock()
		delete(m.healthSubscribers, id)
		m.healthSubMu.Unlock()
	}
}

func (m *Manager) publishHealthResult(event HealthResultEvent) {
	m.healthSubMu.RLock()
	callbacks := make([]func(HealthResultEvent), 0, len(m.healthSubscribers))
	for _, callback := range m.healthSubscribers {
		callbacks = append(callbacks, callback)
	}
	m.healthSubMu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}

func (e *entry) blacklistUntil(until time.Time) {
	e.mu.Lock()
	e.blacklist = true
	e.until = until
	e.mu.Unlock()
}

func (e *entry) clearBlacklist() {
	e.mu.Lock()
	e.blacklist = false
	e.until = time.Time{}
	e.mu.Unlock()
}

func (e *entry) incActive() {
	e.active.Add(1)
}

func (e *entry) decActive() {
	e.active.Add(-1)
}

func (e *entry) setProbe(fn probeFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probe = fn
}

func (e *entry) setRelease(fn releaseFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.release = fn
}

func (e *entry) setDialer(fn DialerFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dialer = fn
}

func (e *entry) recordProbeLatency(d time.Duration) {
	e.mu.Lock()
	e.lastProbe = d
	e.mu.Unlock()
}

func (e *entry) updateTrafficSpeed(now time.Time) {
	curUp := e.totalUpload.Load()
	curDown := e.totalDownload.Load()

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.lastSpeedAt.IsZero() {
		e.lastSpeedAt = now
		e.lastSpeedUpload = curUp
		e.lastSpeedDown = curDown
		e.uploadSpeed = 0
		e.downloadSpeed = 0
		return
	}

	elapsed := now.Sub(e.lastSpeedAt).Seconds()
	if elapsed <= 0 {
		return
	}

	deltaUp := curUp - e.lastSpeedUpload
	deltaDown := curDown - e.lastSpeedDown
	if deltaUp < 0 {
		deltaUp = 0
	}
	if deltaDown < 0 {
		deltaDown = 0
	}

	e.uploadSpeed = int64(float64(deltaUp) / elapsed)
	e.downloadSpeed = int64(float64(deltaDown) / elapsed)
	e.lastSpeedUpload = curUp
	e.lastSpeedDown = curDown
	e.lastSpeedAt = now
}

// RecordFailure updates failure counters with destination info.
func (h *EntryHandle) RecordFailure(err error, destination string) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordFailure(err, destination)
}

// RecordSuccess updates the last success timestamp with destination info.
func (h *EntryHandle) RecordSuccess(destination string) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccess(destination)
}

// RecordSuccessWithLatency updates the last success timestamp and latency.
func (h *EntryHandle) RecordSuccessWithLatency(latency time.Duration) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.recordSuccessWithLatency(latency)
}

// Blacklist marks the node unavailable until the given deadline.
func (h *EntryHandle) Blacklist(until time.Time) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.blacklistUntil(until)
}

// ClearBlacklist removes the blacklist flag.
func (h *EntryHandle) ClearBlacklist() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.clearBlacklist()
}

// IncActive increments the active connection counter.
func (h *EntryHandle) IncActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.incActive()
}

// DecActive decrements the active connection counter.
func (h *EntryHandle) DecActive() {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.decActive()
}

// SetProbe assigns a probe function.
func (h *EntryHandle) SetProbe(fn func(ctx context.Context) (time.Duration, error)) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setProbe(fn)
}

// SetRelease assigns a release function.
func (h *EntryHandle) SetRelease(fn func()) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setRelease(fn)
}

// SetDialer assigns a dial-through-node function so the monitor layer can
// open arbitrary connections via this node's outbound (used by unlock tests).
func (h *EntryHandle) SetDialer(fn DialerFunc) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.setDialer(fn)
}

// MarkInitialCheckDone marks the initial health check as completed.
func (h *EntryHandle) MarkInitialCheckDone(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.initialCheckDone = true
	h.ref.available = available
	h.ref.mu.Unlock()
}

// MarkAvailable updates the availability status.
func (h *EntryHandle) MarkAvailable(available bool) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.mu.Lock()
	h.ref.available = available
	h.ref.mu.Unlock()
}

// AddTraffic adds upload and download byte counts to the node's traffic counters.
func (h *EntryHandle) AddTraffic(upload, download int64) {
	if h == nil || h.ref == nil {
		return
	}
	if upload > 0 {
		h.ref.totalUpload.Add(upload)
	}
	if download > 0 {
		h.ref.totalDownload.Add(download)
	}
}

// SetTraffic sets the traffic counters to specific values (used for restoring from store).
func (h *EntryHandle) SetTraffic(upload, download int64) {
	if h == nil || h.ref == nil {
		return
	}
	h.ref.totalUpload.Store(upload)
	h.ref.totalDownload.Store(download)
}
