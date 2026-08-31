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
	Enabled                   bool
	Listen                    string
	ProbeTarget               string
	StartupAvailabilityPolicy string
	Password                  string
	ProxyUsername             string // 代理池的用户名（用于导出）
	ProxyPassword             string // 代理池的密码（用于导出）
	ExternalIP                string // 外部 IP 地址，用于导出时替换 0.0.0.0
	SkipCertVerify            bool   // 全局跳过 SSL 证书验证
	ProbeConcurrency          int
	StartupProbeTimeout       time.Duration
	RoutineProbeTimeout       time.Duration
	ProbeDialTimeout          time.Duration
	ProbeResponseTimeout      time.Duration
	RoutineProbeRetries       int
}

const (
	StartupAvailabilityOptimistic = "optimistic"
	StartupAvailabilityStrict     = "strict"
)

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
	InFlight       bool           `json:"in_flight"`
	Kind           ProbeRoundKind `json:"kind,omitempty"`
	StartedAt      time.Time      `json:"started_at,omitempty"`
	Total          int            `json:"total"`
	Completed      int            `json:"completed"`
	Success        int            `json:"success"`
	Failed         int            `json:"failed"`
	Attempt        int            `json:"attempt"`
	ActiveWorkers  int            `json:"active_workers"`
	HardTimeouts   int            `json:"hard_timeouts"`
	DetachedProbes int            `json:"detached_probes"`
	LastProgressAt time.Time      `json:"last_progress_at,omitempty"`
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
	// SnapshotOnly asks routing observers to rebuild from the current monitor
	// snapshot without treating the notification as a new health result.
	SnapshotOnly bool
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
	Provisional       bool            `json:"provisional"`
	RoutingEligible   bool            `json:"routing_eligible"`
	HealthSource      string          `json:"health_source"`
	HealthUpdatedAt   time.Time       `json:"health_updated_at,omitempty"`
	TotalUpload       int64           `json:"total_upload"`
	TotalDownload     int64           `json:"total_download"`
	UploadSpeed       int64           `json:"upload_speed"`   // bytes/sec
	DownloadSpeed     int64           `json:"download_speed"` // bytes/sec
	Timeline          []TimelineEvent `json:"timeline,omitempty"`
}

// PersistedHealthState is the storage-neutral health snapshot restored before
// startup convergence. It intentionally does not restore InitialCheckDone:
// every concrete runtime generation must still be probed in the background.
type PersistedHealthState struct {
	NodeID           int64
	FailureCount     int
	SuccessCount     int64
	Blacklisted      bool
	BlacklistedUntil time.Time
	LastError        string
	LastFailure      time.Time
	LastSuccess      time.Time
	LastLatencyMs    int64
	Available        bool
	TotalUpload      int64
	TotalDownload    int64
	UpdatedAt        time.Time
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
	healthSource     string
	healthUpdatedAt  time.Time
	reloadGen        uint64 // generation counter to track active registrations
	mu               sync.RWMutex
	onTimeline       func(DebugLogEvent)
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg         Config
	probeTarget ProbeTarget
	probeReady  bool
	targetMu    sync.RWMutex
	registry    *nodeRegistry
	ctx         context.Context
	cancel      context.CancelFunc
	logger      Logger

	// periodic health check control
	healthMu                  sync.Mutex
	healthInterval            time.Duration
	healthStarted             bool
	policyMu                  sync.RWMutex
	probePolicy               ProbePolicy
	probeScheduler            *probeScheduler
	debugSubMu                sync.RWMutex
	debugSubscribers          map[chan DebugLogEvent]struct{}
	healthEvents              *healthEventHub
	groupScheduleMu           sync.RWMutex
	groupSchedules            map[int64]groupHealthSchedule
	groupScheduleNext         uint64
	availabilityMu            sync.RWMutex
	startupAvailabilityPolicy string
}

type groupHealthSchedule struct {
	tags     map[string]struct{}
	nodeIDs  map[int64]struct{}
	interval time.Duration
	token    uint64
}

type probeScheduleSnapshot struct {
	started  bool
	base     time.Duration
	byTag    map[string]time.Duration
	byNodeID map[int64]time.Duration
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
		cfg:              cfg,
		ctx:              ctx,
		cancel:           cancel,
		debugSubscribers: make(map[chan DebugLogEvent]struct{}),
		groupSchedules:   make(map[int64]groupHealthSchedule),
		probePolicy: normalizeProbePolicy(ProbePolicy{
			Concurrency: cfg.ProbeConcurrency, StartupTimeout: cfg.StartupProbeTimeout,
			RoutineTimeout: cfg.RoutineProbeTimeout, DialTimeout: cfg.ProbeDialTimeout,
			ResponseTimeout: cfg.ProbeResponseTimeout, RoutineRetries: cfg.RoutineProbeRetries,
		}),
	}
	m.SetStartupAvailabilityPolicy(cfg.StartupAvailabilityPolicy)
	if strings.TrimSpace(cfg.ProbeTarget) != "" {
		target, err := parseProbeTarget(cfg.ProbeTarget)
		if err != nil {
			cancel()
			return nil, err
		}
		m.probeTarget = target
		m.probeReady = true
	}
	m.registry = newNodeRegistry(m)
	m.healthEvents = newHealthEventHub()
	m.probeScheduler = newProbeScheduler(m)
	go m.startTrafficSpeedSampler()
	go m.probeScheduler.run()
	return m, nil
}

func normalizeStartupAvailabilityPolicy(policy string) string {
	if strings.EqualFold(strings.TrimSpace(policy), StartupAvailabilityStrict) {
		return StartupAvailabilityStrict
	}
	return StartupAvailabilityOptimistic
}

func (m *Manager) SetStartupAvailabilityPolicy(policy string) {
	normalized := normalizeStartupAvailabilityPolicy(policy)
	m.availabilityMu.Lock()
	changed := m.startupAvailabilityPolicy != "" && m.startupAvailabilityPolicy != normalized
	m.startupAvailabilityPolicy = normalized
	m.cfg.StartupAvailabilityPolicy = m.startupAvailabilityPolicy
	m.availabilityMu.Unlock()
	if changed && m.healthEvents != nil {
		m.healthEvents.broadcast(HealthResultEvent{SnapshotOnly: true, CheckedAt: time.Now()})
	}
}

func (m *Manager) StartupAvailabilityPolicy() string {
	m.availabilityMu.RLock()
	defer m.availabilityMu.RUnlock()
	return m.startupAvailabilityPolicy
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
	m.healthInterval = interval
	m.healthStarted = true
	m.healthMu.Unlock()
	m.probeScheduler.reschedulePeriodic()

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
	m.healthMu.Unlock()
	m.probeScheduler.reschedulePeriodic()
	if m.logger != nil {
		m.logger.Info("periodic health check interval updated: ", d)
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
	m.probeScheduler.reschedulePeriodic()
	return func() {
		m.groupScheduleMu.Lock()
		if current, ok := m.groupSchedules[groupID]; ok && current.token == token {
			delete(m.groupSchedules, groupID)
		}
		m.groupScheduleMu.Unlock()
		m.probeScheduler.reschedulePeriodic()
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
	m.probeScheduler.reschedulePeriodic()
	return func() {
		m.groupScheduleMu.Lock()
		if current, ok := m.groupSchedules[groupID]; ok && current.token == token {
			delete(m.groupSchedules, groupID)
		}
		m.groupScheduleMu.Unlock()
		m.probeScheduler.reschedulePeriodic()
	}
}

func (m *Manager) UnregisterGroupHealthSchedule(groupID int64) {
	if groupID == 0 {
		return
	}
	m.groupScheduleMu.Lock()
	delete(m.groupSchedules, groupID)
	m.groupScheduleMu.Unlock()
	m.probeScheduler.reschedulePeriodic()
}

// probeDue reports whether a node's effective interval has elapsed. The caller
// supplies nodeID so due-time calculation stays independent of the registry
// and can operate on an immutable scheduling snapshot.
func (m *Manager) probeDue(tag string, nodeID int64, lastCheck, now time.Time) bool {
	if lastCheck.IsZero() {
		return true
	}
	schedule := m.probeScheduleSnapshot()
	interval := schedule.intervalFor(tag, nodeID)
	return !now.Before(lastCheck.Add(interval))
}

func (m *Manager) probeScheduleSnapshot() probeScheduleSnapshot {
	m.healthMu.Lock()
	snapshot := probeScheduleSnapshot{started: m.healthStarted, base: m.healthInterval}
	m.healthMu.Unlock()
	if snapshot.base <= 0 {
		snapshot.base = 2 * time.Hour
	}
	snapshot.byTag = make(map[string]time.Duration)
	snapshot.byNodeID = make(map[int64]time.Duration)
	m.groupScheduleMu.RLock()
	for _, schedule := range m.groupSchedules {
		if schedule.interval <= 0 {
			continue
		}
		for tag := range schedule.tags {
			if current := snapshot.byTag[tag]; current == 0 || schedule.interval < current {
				snapshot.byTag[tag] = schedule.interval
			}
		}
		for nodeID := range schedule.nodeIDs {
			if current := snapshot.byNodeID[nodeID]; current == 0 || schedule.interval < current {
				snapshot.byNodeID[nodeID] = schedule.interval
			}
		}
	}
	m.groupScheduleMu.RUnlock()
	return snapshot
}

func (s probeScheduleSnapshot) intervalFor(tag string, nodeID int64) time.Duration {
	interval := s.base
	if candidate := s.byTag[tag]; candidate > 0 && candidate < interval {
		interval = candidate
	}
	if candidate := s.byNodeID[nodeID]; candidate > 0 && candidate < interval {
		interval = candidate
	}
	return interval
}

func (m *Manager) nextProbeDelay(now time.Time) (time.Duration, bool) {
	if _, ready := m.TargetForProbe(); !ready {
		return 0, false
	}
	schedule := m.probeScheduleSnapshot()
	if !schedule.started {
		return 0, false
	}
	found := false
	var earliest time.Time
	for _, item := range m.registry.entries() {
		item.mu.RLock()
		probe := item.probe
		tag := item.info.Tag
		nodeID := item.info.NodeID
		lastCheck := item.lastHealthCheck
		item.mu.RUnlock()
		if probe == nil {
			continue
		}
		due := now
		if !lastCheck.IsZero() {
			due = lastCheck.Add(schedule.intervalFor(tag, nodeID))
		}
		if !found || due.Before(earliest) {
			earliest = due
			found = true
		}
	}
	if !found {
		return 0, false
	}
	delay := earliest.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

// RequestStartupProbeAllOnce triggers the one fast full round used after all
// nodes have been registered at startup or reload.
func (m *Manager) RequestStartupProbeAllOnce() {
	if _, ready := m.TargetForProbe(); !ready {
		tags := m.registry.tags()
		for _, tag := range tags {
			_ = m.MarkAvailableWithoutProbe(tag)
		}
		return
	}
	tags := m.registry.tags()
	m.probeScheduler.enqueueInitial(tags)
}

// RequestRoutineProbeAllOnce triggers a periodic-policy full round.
func (m *Manager) RequestRoutineProbeAllOnce() {
	m.probeScheduler.requestRoutine(false)
}

// RequestDueProbesOnce checks only nodes whose effective management/group
// interval has elapsed. Batch rounds never overlap.
func (m *Manager) RequestDueProbesOnce() {
	m.probeScheduler.requestRoutine(true)
}

// RunProbeBatch submits one mutually exclusive round to the scheduler actor.
// Result callbacks are serialized before the method returns, which is safe for
// the SSE handler's single response writer.
func (m *Manager) RunProbeBatch(ctx context.Context, kind ProbeRoundKind, dueOnly bool,
	onStart func(total int), onResult func(ProbeBatchResult)) (ProbeBatchSummary, error) {
	return m.probeScheduler.runBatch(ctx, kind, dueOnly, onStart, onResult)
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
	return m.probeScheduler.roundStatus()
}

func (m *Manager) ProbeNodeCount() int {
	return m.registry.count()
}

func (m *Manager) InitialProbeStatus() InitialProbeStatus {
	pending := 0
	for _, item := range m.registry.entries() {
		item.mu.RLock()
		if !item.initialCheckDone {
			pending++
		}
		item.mu.RUnlock()
	}
	return m.probeScheduler.initialStatus(pending)
}

// Stop stops the periodic health check.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.healthEvents != nil {
		m.healthEvents.close()
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
	for _, e := range m.registry.entries() {
		e.updateTrafficSpeed(now)
	}
}

// BeginReload bumps the generation counter. Nodes registered after this call
// will be marked with the new generation. Call SweepStaleNodes after reload
// to remove nodes that were not re-registered (disabled/deleted nodes).
func (m *Manager) BeginReload() {
	m.registry.beginReload()
}

// SweepStaleNodes removes nodes that were not re-registered during the current
// reload cycle. This preserves monitoring data (latency, success/failure counts)
// for nodes that are still active, while cleaning up disabled/removed nodes.
func (m *Manager) SweepStaleNodes() {
	m.registry.sweepStale()
	m.probeScheduler.signal()
}

// ClearNodes removes all registered nodes. Use BeginReload + SweepStaleNodes
// for reload scenarios to preserve data for active nodes.
func (m *Manager) ClearNodes() {
	m.registry.clear()
}

// Register ensures a node is tracked and returns its entry.
// If the node already exists, its info is updated but monitoring stats
// (latency, success/failure counts, etc.) are preserved.
func (m *Manager) Register(info NodeInfo) *EntryHandle {
	return &EntryHandle{ref: m.registry.register(info)}
}

// MigrateRuntimeTag atomically moves the monitor identity of a stable node to
// a new concrete runtime tag. The entry object is retained, so latency,
// counters, traffic and timeline history survive an outbound replacement.
// If no entry with nodeID exists this behaves like Register.
func (m *Manager) MigrateRuntimeTag(nodeID int64, info NodeInfo) *EntryHandle {
	return &EntryHandle{ref: m.registry.migrate(nodeID, info)}
}

// UnregisterRuntimeTag removes a retired runtime identity if it is still the
// map owner. A migrated entry is keyed by its new tag and is left untouched.
func (m *Manager) UnregisterRuntimeTag(tag string) {
	m.registry.unregister(tag)
	m.probeScheduler.signal()
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
	m.probeScheduler.enqueueInitial(tags)
}

// HandleForTag returns an existing monitor entry without changing its reload
// generation, callbacks, or node metadata. Independent group runtimes use this
// to observe the base runtime's health state without becoming probe owners.
func (m *Manager) HandleForTag(tag string) *EntryHandle {
	entry := m.registry.byTagEntry(tag)
	if entry == nil {
		return nil
	}
	return &EntryHandle{ref: entry}
}

// HandleForNodeID resolves the current monitor owner for a stable node.
func (m *Manager) HandleForNodeID(nodeID int64) *EntryHandle {
	item := m.registry.byNodeID(nodeID)
	if item == nil {
		return nil
	}
	return &EntryHandle{ref: item}
}

// DestinationForProbe exposes the configured destination for health checks.
func (m *Manager) DestinationForProbe() (M.Socksaddr, bool) {
	m.targetMu.RLock()
	defer m.targetMu.RUnlock()
	if !m.probeReady {
		return M.Socksaddr{}, false
	}
	return m.probeTarget.Destination, true
}

// TargetForProbe returns the full endpoint needed for a scheme-aware HTTP
// health check. Callers should prefer this over DestinationForProbe.
func (m *Manager) TargetForProbe() (ProbeTarget, bool) {
	m.targetMu.RLock()
	defer m.targetMu.RUnlock()
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
		m.targetMu.Lock()
		m.probeTarget = ProbeTarget{}
		m.probeReady = false
		m.cfg.ProbeTarget = ""
		m.targetMu.Unlock()
		m.probeScheduler.reschedulePeriodic()
		return nil
	}
	parsed, err := parseProbeTarget(target)
	if err != nil {
		return err
	}

	m.targetMu.Lock()
	m.probeTarget = parsed
	m.probeReady = true
	m.cfg.ProbeTarget = target
	m.targetMu.Unlock()
	m.probeScheduler.reschedulePeriodic()
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
	for _, item := range m.registry.entries() {
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
	e := m.registry.byTagEntry(tag)
	if e == nil {
		return nil
	}
	snap := m.decorateSnapshot(e.snapshot())
	return &snap
}

// SnapshotForNodeID returns health for the current runtime generation of a
// stable node, independent of a group box's concrete tag.
func (m *Manager) SnapshotForNodeID(nodeID int64) *Snapshot {
	handle := m.HandleForNodeID(nodeID)
	if handle == nil {
		return nil
	}
	snapshot := m.decorateSnapshot(handle.ref.snapshot())
	return &snapshot
}

// SnapshotFiltered returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
// Nodes that haven't been checked yet are also included (they will be checked on first use).
func (m *Manager) SnapshotFiltered(onlyAvailable bool) []Snapshot {
	list := m.registry.entries()
	snapshots := make([]Snapshot, 0, len(list))
	for _, e := range list {
		snap := m.decorateSnapshot(e.snapshot())
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

func (m *Manager) decorateSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Provisional = !snapshot.InitialCheckDone && m.StartupAvailabilityPolicy() == StartupAvailabilityOptimistic && !snapshot.Blacklisted
	snapshot.RoutingEligible = !snapshot.Blacklisted && ((snapshot.InitialCheckDone && snapshot.Available) || snapshot.Provisional)
	if snapshot.HealthSource == "" {
		snapshot.HealthSource = "none"
	}
	return snapshot
}

// RestorePersistedHealth restores display/history fields by stable node ID.
// Runtime eligibility remains provisional and startup convergence still probes
// every restored entry because InitialCheckDone is deliberately left false.
func (m *Manager) RestorePersistedHealth(states map[int64]PersistedHealthState) int {
	restored := 0
	now := time.Now()
	for nodeID, state := range states {
		entry := m.registry.byNodeID(nodeID)
		if entry == nil {
			continue
		}
		entry.mu.Lock()
		entry.failure = state.FailureCount
		entry.success = state.SuccessCount
		entry.blacklist = state.Blacklisted && (state.BlacklistedUntil.IsZero() || state.BlacklistedUntil.After(now))
		entry.until = state.BlacklistedUntil
		entry.lastError = state.LastError
		entry.lastFail = state.LastFailure
		entry.lastOK = state.LastSuccess
		if state.LastLatencyMs > 0 {
			entry.lastProbe = time.Duration(state.LastLatencyMs) * time.Millisecond
		}
		entry.available = state.Available
		entry.initialCheckDone = false
		entry.healthSource = "persisted"
		entry.healthUpdatedAt = state.UpdatedAt
		entry.totalUpload.Store(state.TotalUpload)
		entry.totalDownload.Store(state.TotalDownload)
		entry.mu.Unlock()
		restored++
	}
	if restored > 0 && m.healthEvents != nil {
		m.healthEvents.broadcast(HealthResultEvent{SnapshotOnly: true, CheckedAt: time.Now()})
	}
	return restored
}

// TrafficSummary returns aggregated traffic totals/speeds and per-node speeds.
// includeNodes controls whether per-node details are returned.
func (m *Manager) TrafficSummary(includeNodes bool) TrafficSummary {
	list := m.registry.entries()

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
	return m.probeScheduler.beginTagProbe(tag)
}

func (m *Manager) endTagProbe(tag string) {
	m.probeScheduler.endTagProbe(tag)
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
		e.healthSource = "runtime"
		e.healthUpdatedAt = checkedAt
		e.mu.Unlock()
	} else {
		e.recordSuccessWithLatency(latency)
		e.mu.Lock()
		e.lastHealthCheck = checkedAt
		e.healthSource = "runtime"
		e.healthUpdatedAt = checkedAt
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
	e := m.registry.byTagEntry(tag)
	if e == nil {
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
		HealthSource:      e.healthSource,
		HealthUpdatedAt:   e.healthUpdatedAt,
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
	now := time.Now()
	e.success++
	e.lastOK = now
	e.lastProbe = latency
	e.available = true
	e.initialCheckDone = true
	e.healthSource = "runtime"
	e.healthUpdatedAt = now
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
	return m.healthEvents.subscribe(nil, nil, true, callback)
}

// SubscribeHealthResultsFor installs a targeted observer. A subscription
// indexed by both tag and Node ID is invoked at most once for each event.
func (m *Manager) SubscribeHealthResultsFor(tags []string, nodeIDs []int64, callback func(HealthResultEvent)) func() {
	return m.healthEvents.subscribe(tags, nodeIDs, false, callback)
}

func (m *Manager) publishHealthResult(event HealthResultEvent) {
	m.healthEvents.publish(event)
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
