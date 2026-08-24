package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
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
	Enabled        bool
	Listen         string
	ProbeTarget    string
	Password       string
	ProxyUsername  string // 代理池的用户名（用于导出）
	ProxyPassword  string // 代理池的密码（用于导出）
	ExternalIP     string // 外部 IP 地址，用于导出时替换 0.0.0.0
	SkipCertVerify bool   // 全局跳过 SSL 证书验证
}

// NodeInfo is static metadata about a proxy entry.
type NodeInfo struct {
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
	initialCheckDone bool
	available        bool
	reloadGen        uint64 // generation counter to track active registrations
	mu               sync.RWMutex
	onTimeline       func(DebugLogEvent)
}

// Manager aggregates all node states for the UI/API.
type Manager struct {
	cfg        Config
	reloadGen  uint64 // current reload generation
	probeDst   M.Socksaddr
	probeReady bool
	mu         sync.RWMutex
	nodes      map[string]*entry
	ctx        context.Context
	cancel     context.CancelFunc
	logger     Logger

	// periodic health check control
	healthMu         sync.Mutex
	healthInterval   time.Duration
	healthTimeout    time.Duration
	healthTicker     *time.Ticker
	healthIntervalC  chan time.Duration
	probeAllInFlight atomic.Bool
	debugSubMu       sync.RWMutex
	debugSubscribers map[chan DebugLogEvent]struct{}
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
		nodes:            make(map[string]*entry),
		ctx:              ctx,
		cancel:           cancel,
		debugSubscribers: make(map[chan DebugLogEvent]struct{}),
	}
	if cfg.ProbeTarget != "" {
		target := cfg.ProbeTarget
		// Strip URL scheme if present (e.g., "https://www.google.com:443" -> "www.google.com:443")
		if strings.HasPrefix(target, "https://") {
			target = strings.TrimPrefix(target, "https://")
		} else if strings.HasPrefix(target, "http://") {
			target = strings.TrimPrefix(target, "http://")
		}
		// Remove trailing path if present
		if idx := strings.Index(target, "/"); idx != -1 {
			target = target[:idx]
		}
		host, port, err := net.SplitHostPort(target)
		if err != nil {
			// If no port specified, use default based on original scheme
			if strings.HasPrefix(cfg.ProbeTarget, "https://") {
				host = target
				port = "443"
			} else {
				host = target
				port = "80"
			}
		}
		parsed := M.ParseSocksaddrHostPort(host, parsePort(port))
		m.probeDst = parsed
		m.probeReady = true
	}
	go m.startTrafficSpeedSampler()
	return m, nil
}

// SetLogger sets the logger for the manager.
func (m *Manager) SetLogger(logger Logger) {
	m.logger = logger
}

// StartPeriodicHealthCheck starts a background goroutine that periodically checks all nodes.
// interval: how often to check (e.g., 30 * time.Second)
// timeout: timeout for each probe (e.g., 10 * time.Second)
func (m *Manager) StartPeriodicHealthCheck(interval, timeout time.Duration) {
	if !m.probeReady {
		if m.logger != nil {
			m.logger.Warn("probe target not configured, periodic health check disabled")
		}
		return
	}
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	m.healthMu.Lock()
	if m.healthIntervalC == nil {
		m.healthIntervalC = make(chan time.Duration, 1)
	}
	m.healthInterval = interval
	m.healthTimeout = timeout
	if m.healthTicker != nil {
		m.healthTicker.Stop()
	}
	m.healthTicker = time.NewTicker(interval)
	ticker := m.healthTicker
	intervalC := m.healthIntervalC
	m.healthMu.Unlock()

	go func() {
		// 启动后立即进行一次检查（走去重）
		m.RequestProbeAllOnce(timeout)

		for {
			select {
			case <-m.ctx.Done():
				return
			case newInterval := <-intervalC:
				if newInterval <= 0 {
					newInterval = 2 * time.Hour
				}
				// 重置 ticker
				m.healthMu.Lock()
				m.healthInterval = newInterval
				if m.healthTicker != nil {
					m.healthTicker.Stop()
				}
				m.healthTicker = time.NewTicker(newInterval)
				ticker = m.healthTicker
				m.healthMu.Unlock()
				if m.logger != nil {
					m.logger.Info("periodic health check interval updated: ", newInterval)
				}
			case <-ticker.C:
				m.RequestProbeAllOnce(timeout)
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

// RequestProbeAllOnce triggers a full probe round at most once concurrently.
// If another full probe is already running, it returns immediately.
func (m *Manager) RequestProbeAllOnce(timeout time.Duration) {
	if !m.probeReady {
		return
	}
	if m.probeAllInFlight.Swap(true) {
		return
	}
	go func() {
		defer m.probeAllInFlight.Store(false)
		m.probeAllNodes(timeout)
	}()
}

// probeAllNodes checks all registered nodes concurrently.
func (m *Manager) probeAllNodes(timeout time.Duration) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.nodes))
	for _, e := range m.nodes {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	if len(entries) == 0 {
		return
	}

	if m.logger != nil {
		m.logger.Info("starting health check for ", len(entries), " nodes")
	}

	workerLimit := runtime.NumCPU() * 2
	if workerLimit < 8 {
		workerLimit = 8
	}
	sem := make(chan struct{}, workerLimit)
	var wg sync.WaitGroup
	var availableCount atomic.Int32
	var failedCount atomic.Int32

	for _, e := range entries {
		e.mu.RLock()
		probeFn := e.probe
		tag := e.info.Tag
		e.mu.RUnlock()

		if probeFn == nil {
			continue
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(entry *entry, probe probeFunc, tag string) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(m.ctx, timeout)
			latency, err := probe(ctx)
			cancel()

			entry.mu.Lock()
			if err != nil {
				failedCount.Add(1)
				entry.lastError = err.Error()
				entry.lastFail = time.Now()
				entry.lastProbe = 0
				entry.available = false
				entry.initialCheckDone = true
			} else {
				availableCount.Add(1)
				entry.lastOK = time.Now()
				entry.lastProbe = latency
				entry.available = true
				entry.initialCheckDone = true
			}
			entry.mu.Unlock()

			if err != nil && m.logger != nil {
				m.logger.Warn("probe failed for ", tag, ": ", err)
			}
		}(e, probeFn, tag)
	}
	wg.Wait()

	if m.logger != nil {
		m.logger.Info("health check completed: ", availableCount.Load(), " available, ", failedCount.Load(), " failed")
	}
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

func parsePort(value string) uint16 {
	p, err := strconv.Atoi(value)
	if err != nil || p <= 0 || p > 65535 {
		return 80
	}
	return uint16(p)
}

// BeginReload bumps the generation counter. Nodes registered after this call
// will be marked with the new generation. Call SweepStaleNodes after reload
// to remove nodes that were not re-registered (disabled/deleted nodes).
func (m *Manager) BeginReload() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reloadGen++
}

// SweepStaleNodes removes nodes that were not re-registered during the current
// reload cycle. This preserves monitoring data (latency, success/failure counts)
// for nodes that are still active, while cleaning up disabled/removed nodes.
func (m *Manager) SweepStaleNodes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tag, e := range m.nodes {
		if e.reloadGen != m.reloadGen {
			delete(m.nodes, tag)
		}
	}
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
		e.info = info
		e.reloadGen = m.reloadGen
		e.onTimeline = m.publishDebugLog
	}
	return &EntryHandle{ref: e}
}

// DestinationForProbe exposes the configured destination for health checks.
func (m *Manager) DestinationForProbe() (M.Socksaddr, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.probeReady {
		return M.Socksaddr{}, false
	}
	return m.probeDst, true
}

// UpdateProbeTarget dynamically updates the probe destination at runtime.
func (m *Manager) UpdateProbeTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		m.mu.Lock()
		m.probeDst = M.Socksaddr{}
		m.probeReady = false
		m.cfg.ProbeTarget = ""
		m.mu.Unlock()
		return nil
	}
	defaultPort := "80"
	if strings.HasPrefix(target, "https://") {
		defaultPort = "443"
		target = strings.TrimPrefix(target, "https://")
	} else if strings.HasPrefix(target, "http://") {
		target = strings.TrimPrefix(target, "http://")
	}
	// Remove trailing path if present
	if idx := strings.Index(target, "/"); idx != -1 {
		target = target[:idx]
	}

	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// If no port specified, use the scheme's default port.
		host = target
		port = defaultPort
	}

	parsed := M.ParseSocksaddrHostPort(host, parsePort(port))

	m.mu.Lock()
	m.probeDst = parsed
	m.probeReady = true
	m.cfg.ProbeTarget = target
	m.mu.Unlock()

	return nil
}

// Snapshot returns a sorted copy of current node states.
// If onlyAvailable is true, only returns nodes that passed initial health check.
func (m *Manager) Snapshot() []Snapshot {
	return m.SnapshotFiltered(false)
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
	if e.probe == nil {
		return 0, errors.New("probe not available for this node")
	}
	latency, err := e.probe(ctx)
	if err != nil {
		// 探测失败：标记节点为不可用，清除旧延迟数据
		e.mu.Lock()
		e.available = false
		e.initialCheckDone = true
		e.lastProbe = 0 // 清除延迟，前端显示"未测试"
		e.lastError = err.Error()
		e.lastFail = time.Now()
		e.mu.Unlock()
		return 0, err
	}
	// 探测成功：recordSuccessWithLatency 已在 probe 函数内更新 available=true
	e.recordProbeLatency(latency)
	return latency, nil
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
