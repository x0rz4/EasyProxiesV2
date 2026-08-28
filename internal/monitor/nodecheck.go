package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/ipquality"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodedetect"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"
)

const (
	nodeCheckTaskPending     = "pending"
	nodeCheckTaskRunning     = "running"
	nodeCheckTaskCompleted   = "completed"
	nodeCheckTaskFailed      = "failed"
	nodeCheckTaskCancelled   = "cancelled"
	nodeCheckTaskInterrupted = "interrupted"
)

type nodeCheckStages struct {
	Latency bool `json:"latency"`
	Speed   bool `json:"speed"`
	Quality bool `json:"quality"`
	Unlock  bool `json:"unlock"`
}

type nodeCheckStageStats struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Success   int `json:"success"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type nodeCheckTaskSnapshot struct {
	ID              string                         `json:"id"`
	Status          string                         `json:"status"`
	Stages          nodeCheckStages                `json:"stages"`
	Settings        map[string]any                 `json:"settings"`
	Stats           map[string]nodeCheckStageStats `json:"stats"`
	TotalNodes      int                            `json:"total_nodes"`
	CompletedNodes  int                            `json:"completed_nodes"`
	DownloadedBytes int64                          `json:"downloaded_bytes"`
	Error           string                         `json:"error,omitempty"`
	CreatedAt       time.Time                      `json:"created_at"`
	StartedAt       time.Time                      `json:"started_at,omitempty"`
	FinishedAt      time.Time                      `json:"finished_at,omitempty"`
}

type nodeCheckEvent struct {
	Sequence        int64                      `json:"sequence"`
	Type            string                     `json:"type"`
	Task            *nodeCheckTaskSnapshot     `json:"task,omitempty"`
	Phase           string                     `json:"phase,omitempty"`
	NodeID          int64                      `json:"node_id,omitempty"`
	Tag             string                     `json:"tag,omitempty"`
	Name            string                     `json:"name,omitempty"`
	Status          string                     `json:"status,omitempty"`
	Error           string                     `json:"error,omitempty"`
	LatencyMs       *int64                     `json:"latency_ms,omitempty"`
	Speed           *nodedetect.SpeedResult    `json:"speed,omitempty"`
	SpeedProgress   *nodedetect.SpeedProgress  `json:"speed_progress,omitempty"`
	Quality         *store.NodeIPQualityResult `json:"quality,omitempty"`
	DownloadedBytes int64                      `json:"downloaded_bytes,omitempty"`
}

type nodeCheckTask struct {
	mu         sync.Mutex
	snapshot   nodeCheckTaskSnapshot
	settings   config.NodeCheckConfig
	nodes      []Snapshot
	ctx        context.Context
	cancel     context.CancelFunc
	sequence   int64
	subs       map[chan nodeCheckEvent]struct{}
	latencyOK  map[int64]bool
	landingIPs map[int64]string
}

// nodeIDs returns the store IDs of the task's nodes. task.nodes is fixed at
// construction, so no lock is needed.
func (t *nodeCheckTask) nodeIDs() []int64 {
	ids := make([]int64, 0, len(t.nodes))
	for _, node := range t.nodes {
		if node.NodeID > 0 {
			ids = append(ids, node.NodeID)
		}
	}
	return ids
}

func (t *nodeCheckTask) copySnapshot() nodeCheckTaskSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.copySnapshotLocked()
}

func (t *nodeCheckTask) copySnapshotLocked() nodeCheckTaskSnapshot {
	copyValue := t.snapshot
	copyValue.Stats = make(map[string]nodeCheckStageStats, len(t.snapshot.Stats))
	for key, value := range t.snapshot.Stats {
		copyValue.Stats[key] = value
	}
	copyValue.Settings = make(map[string]any, len(t.snapshot.Settings))
	for key, value := range t.snapshot.Settings {
		copyValue.Settings[key] = value
	}
	return copyValue
}

func (t *nodeCheckTask) publish(event nodeCheckEvent) {
	if event.Task == nil && event.Type != "progress" {
		snapshot := t.copySnapshot()
		event.Task = &snapshot
	}
	t.mu.Lock()
	t.sequence++
	event.Sequence = t.sequence
	for sub := range t.subs {
		select {
		case sub <- event:
		default:
		}
	}
	t.mu.Unlock()
}

func (t *nodeCheckTask) updateStage(phase, status string) {
	t.mu.Lock()
	stats := t.snapshot.Stats[phase]
	stats.Completed++
	switch status {
	case "success":
		stats.Success++
	case "skipped":
		stats.Skipped++
	default:
		stats.Failed++
	}
	t.snapshot.Stats[phase] = stats
	t.mu.Unlock()
}

type nodeCheckManager struct {
	server *Server
	mu     sync.Mutex
	active *nodeCheckTask
	tasks  map[string]*nodeCheckTask
	ipapi  *ipquality.IPAPIClient
}

func newNodeCheckManager(server *Server) *nodeCheckManager {
	return &nodeCheckManager{server: server, tasks: make(map[string]*nodeCheckTask), ipapi: &ipquality.IPAPIClient{}}
}

type nodeCheckCreateRequest struct {
	NodeIDs  []int64                    `json:"node_ids"`
	Stages   nodeCheckStages            `json:"stages"`
	Settings *nodeCheckSettingsResponse `json:"settings,omitempty"`
}

func (m *nodeCheckManager) create(request nodeCheckCreateRequest) (*nodeCheckTask, error) {
	if !request.Stages.Latency && !request.Stages.Speed && !request.Stages.Quality && !request.Stages.Unlock {
		return nil, errors.New("至少选择一个检测项目")
	}
	wanted := make(map[int64]struct{}, len(request.NodeIDs))
	for _, id := range request.NodeIDs {
		if id > 0 {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil, errors.New("请选择至少一个运行节点")
	}
	var nodes []Snapshot
	for _, snapshot := range m.server.mgr.Snapshot() {
		if _, ok := wanted[snapshot.NodeID]; ok {
			nodes = append(nodes, snapshot)
		}
	}
	if len(nodes) != len(wanted) {
		return nil, errors.New("部分节点当前没有运行时拨号器，请刷新后重试")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	m.server.cfgMu.RLock()
	cfgSrc := m.server.cfgSrc
	m.server.cfgMu.RUnlock()
	if cfgSrc == nil {
		return nil, errors.New("配置存储未初始化")
	}
	settings := cfgSrc.Snapshot().Management.NodeCheck
	if request.Settings != nil {
		override, err := request.Settings.toConfig()
		if err != nil {
			return nil, err
		}
		settings = override
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	stats := make(map[string]nodeCheckStageStats)
	for phase, enabled := range map[string]bool{"latency": request.Stages.Latency, "speed": request.Stages.Speed, "quality": request.Stages.Quality, "unlock": request.Stages.Unlock} {
		if enabled {
			stats[phase] = nodeCheckStageStats{Total: len(nodes)}
		}
	}
	task := &nodeCheckTask{
		settings: settings, nodes: nodes, ctx: ctx, cancel: cancel,
		subs: make(map[chan nodeCheckEvent]struct{}), latencyOK: make(map[int64]bool), landingIPs: make(map[int64]string),
		snapshot: nodeCheckTaskSnapshot{ID: newNodeCheckID(), Status: nodeCheckTaskPending, Stages: request.Stages, Settings: nodeCheckPublicSettings(settings), Stats: stats, TotalNodes: len(nodes), CreatedAt: now},
	}
	m.mu.Lock()
	if m.active != nil {
		active := m.active.copySnapshot()
		if active.Status == nodeCheckTaskPending || active.Status == nodeCheckTaskRunning {
			m.mu.Unlock()
			cancel()
			return nil, fmt.Errorf("已有综合检测任务正在运行: %s", active.ID)
		}
	}
	m.active, m.tasks[task.snapshot.ID] = task, task
	m.mu.Unlock()
	m.persist(task)
	go m.run(task)
	return task, nil
}

func newNodeCheckID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("check-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func (m *nodeCheckManager) run(task *nodeCheckTask) {
	task.mu.Lock()
	task.snapshot.Status = nodeCheckTaskRunning
	task.snapshot.StartedAt = time.Now().UTC()
	task.mu.Unlock()
	task.publish(nodeCheckEvent{Type: "task", Task: snapshotPointer(task.copySnapshot())})
	m.persist(task)

	stages := task.snapshot.Stages
	if stages.Latency {
		m.runLatency(task)
	}
	if task.ctx.Err() == nil && stages.Speed {
		m.runSpeed(task)
	}
	if task.ctx.Err() == nil && stages.Quality {
		m.runQuality(task)
	}
	if task.ctx.Err() == nil && stages.Unlock {
		m.runUnlock(task)
	}

	task.mu.Lock()
	if task.ctx.Err() != nil {
		task.snapshot.Status = nodeCheckTaskCancelled
	} else {
		task.snapshot.Status = nodeCheckTaskCompleted
	}
	task.snapshot.CompletedNodes = task.snapshot.TotalNodes
	task.snapshot.FinishedAt = time.Now().UTC()
	final := task.copySnapshotLocked()
	task.mu.Unlock()
	m.persist(task)
	// Per-stage enqueues cover the facts each stage wrote; this is the backstop for
	// a node whose stage failed early, and it costs nothing extra because the queue
	// coalesces it with everything already pending.
	m.server.enqueueRetag(task.nodeIDs()...)
	task.publish(nodeCheckEvent{Type: "done", Task: &final})
	m.mu.Lock()
	if m.active == task {
		m.active = nil
	}
	m.mu.Unlock()
	m.pruneMemory(20)
}

func (m *nodeCheckManager) runLatency(task *nodeCheckTask) {
	runLimited(task.settings.LatencyConcurrency, task.nodes, func(node Snapshot) {
		if task.ctx.Err() != nil {
			return
		}
		dial, err := m.server.mgr.DialerFor(node.Tag)
		var latency time.Duration
		if err == nil {
			latency, err = nodedetect.MeasureLatency(task.ctx, nodedetect.DialFunc(dial), task.settings.LatencyURL, task.settings.LatencyTimeout, task.settings.IncludeHandshake)
		}
		now := time.Now().UTC()
		result := &store.NodeDetectionResult{NodeID: node.NodeID, TaskID: task.snapshot.ID, LatencyStatus: "success", SpeedStatus: "untested", ExitIPStatus: "untested", LatencyCheckedAt: now}
		status, errText := "success", ""
		if err != nil {
			status, errText, result.LatencyStatus, result.LatencyError = "failed", err.Error(), "failed", err.Error()
		} else {
			value := latency.Milliseconds()
			if value < 1 {
				value = 1
			}
			result.LatencyMs = &value
			task.mu.Lock()
			task.latencyOK[node.NodeID] = true
			task.mu.Unlock()
		}
		_ = m.server.store.UpsertNodeDetectionResult(context.Background(), result)
		m.server.enqueueRetag(node.NodeID)
		task.updateStage("latency", status)
		event := nodeCheckEvent{Type: "result", Phase: "latency", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: status, Error: errText, LatencyMs: result.LatencyMs}
		task.publish(event)
		m.persist(task)
	})
}

func (m *nodeCheckManager) runSpeed(task *nodeCheckTask) {
	nodes := make([]Snapshot, 0, len(task.nodes))
	for _, node := range task.nodes {
		if task.ctx.Err() != nil {
			break
		}
		if task.snapshot.Stages.Latency {
			task.mu.Lock()
			passed := task.latencyOK[node.NodeID]
			task.mu.Unlock()
			if !passed {
				now := time.Now().UTC()
				_ = m.server.store.UpsertNodeDetectionResult(context.Background(), &store.NodeDetectionResult{NodeID: node.NodeID, TaskID: task.snapshot.ID, LatencyStatus: "untested", SpeedStatus: "skipped", SpeedError: "latency test failed", SpeedCheckedAt: now, ExitIPStatus: "untested"})
				task.updateStage("speed", "skipped")
				task.publish(nodeCheckEvent{Type: "result", Phase: "speed", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: "skipped", Error: "latency test failed"})
				continue
			}
		}
		nodes = append(nodes, node)
	}
	runLimited(task.settings.SpeedConcurrency, nodes, func(node Snapshot) {
		if task.ctx.Err() != nil {
			return
		}
		dial, err := m.server.mgr.DialerFor(node.Tag)
		var speed nodedetect.SpeedResult
		if err == nil {
			speed, err = nodedetect.MeasureSpeed(task.ctx, nodedetect.DialFunc(dial), nodedetect.SpeedOptions{URL: task.settings.SpeedURL, Duration: task.settings.SpeedDuration, RequestTimeout: task.settings.SpeedRequestTimeout, MaxBytes: task.settings.MaxDownloadBytes, PeakSampleInterval: task.settings.PeakSampleInterval}, func(progress nodedetect.SpeedProgress) {
				task.publish(nodeCheckEvent{Type: "progress", Phase: "speed", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: "running", SpeedProgress: &progress})
			})
		}
		now := time.Now().UTC()
		result := &store.NodeDetectionResult{NodeID: node.NodeID, TaskID: task.snapshot.ID, LatencyStatus: "untested", SpeedStatus: "success", SpeedCheckedAt: now, ExitIPStatus: "untested"}
		status, errText := "success", ""
		if speed.BytesDownloaded > 0 {
			result.AverageBytesPerSecond, result.PeakBytesPerSecond = &speed.AverageBytesPerSecond, &speed.PeakBytesPerSecond
			result.BytesDownloaded, result.SpeedDurationMs = speed.BytesDownloaded, speed.DurationMs
			task.mu.Lock()
			task.snapshot.DownloadedBytes += speed.BytesDownloaded
			task.mu.Unlock()
		}
		if err != nil {
			status, errText, result.SpeedStatus, result.SpeedError = "failed", err.Error(), "failed", err.Error()
		}
		_ = m.server.store.UpsertNodeDetectionResult(context.Background(), result)
		m.server.enqueueRetag(node.NodeID)
		task.updateStage("speed", status)
		task.publish(nodeCheckEvent{Type: "result", Phase: "speed", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: status, Error: errText, Speed: &speed, DownloadedBytes: speed.BytesDownloaded})
		m.persist(task)
	})
}

func (m *nodeCheckManager) runQuality(task *nodeCheckTask) {
	outcomes := make(map[int64][]string)
	var outcomesMu sync.Mutex
	var locationMu sync.Mutex
	// relocatedNodeIDs, not just a boolean: a region change moves group
	// membership, and the incremental path needs to know which nodes moved.
	relocatedNodeIDs := make([]int64, 0)
	markLocation := func(node Snapshot, region, country string) {
		region = strings.ToLower(strings.TrimSpace(region))
		country = strings.TrimSpace(country)
		if region == "" {
			return
		}
		if err := m.server.store.UpdateNodeLocation(context.Background(), node.NodeID, region, country); err != nil {
			return
		}
		m.server.mgr.UpdateNodeLocation(node.NodeID, region, country)
		if node.Region != region || node.Country != country {
			locationMu.Lock()
			relocatedNodeIDs = append(relocatedNodeIDs, node.NodeID)
			locationMu.Unlock()
		}
	}
	recordOutcome := func(nodeID int64, status string) {
		outcomesMu.Lock()
		outcomes[nodeID] = append(outcomes[nodeID], status)
		outcomesMu.Unlock()
	}
	runLimited(task.settings.QualityConcurrency, task.nodes, func(node Snapshot) {
		if task.ctx.Err() != nil {
			return
		}
		dial, err := m.server.mgr.DialerFor(node.Tag)
		var ip string
		if err == nil {
			ip, err = nodedetect.DiscoverExitIP(task.ctx, nodedetect.DialFunc(dial), task.settings.LandingIPURL, task.settings.QualityTimeout)
		}
		now := time.Now().UTC()
		result := &store.NodeDetectionResult{NodeID: node.NodeID, TaskID: task.snapshot.ID, LatencyStatus: "untested", SpeedStatus: "untested", ExitIPStatus: "success", ExitIP: ip, ExitIPFamily: detectIPFamily(ip), ExitIPCheckedAt: now}
		if err != nil {
			result.ExitIPStatus, result.ExitIPError = "failed", err.Error()
		} else {
			if lookup := m.server.geoipLookupForCheck(); lookup != nil {
				region := lookup.LookupIP(ip)
				result.ExitCountry = region.Country
				result.ExitCountryCode = strings.ToUpper(strings.TrimSpace(region.ISOCode))
			}
			task.mu.Lock()
			task.landingIPs[node.NodeID] = ip
			task.mu.Unlock()
		}
		if saveErr := m.server.store.UpsertNodeDetectionResult(context.Background(), result); saveErr == nil && result.ExitIPStatus == "success" {
			markLocation(node, result.ExitCountryCode, result.ExitCountry)
		}
		m.server.enqueueRetag(node.NodeID)
	})

	if task.settings.IPPureEnabled {
		provider := ipquality.IPPureProvider{URL: task.settings.IPPureURL, Timeout: task.settings.QualityTimeout}
		runLimited(task.settings.QualityConcurrency, task.nodes, func(node Snapshot) {
			if task.ctx.Err() != nil {
				return
			}
			dial, err := m.server.mgr.DialerFor(node.Tag)
			quality := ipquality.Result{Provider: "ippure", Status: ipquality.StatusFailed, Reason: "dialer unavailable", CheckedAt: time.Now().UTC()}
			if err == nil {
				quality = provider.Check(task.ctx, nodedetect.DialFunc(dial))
			}
			m.saveQuality(task, node, quality)
			recordOutcome(node.NodeID, quality.Status)
		})
	} else {
		for _, node := range task.nodes {
			quality := ipquality.Result{Provider: "ippure", Status: ipquality.StatusDisabled, Reason: "provider disabled", CheckedAt: time.Now().UTC()}
			m.saveQuality(task, node, quality)
			recordOutcome(node.NodeID, quality.Status)
		}
	}

	if task.settings.IPAPIEnabled {
		m.ipapi.BaseURL = task.settings.IPAPIBaseURL
		unique := make(map[string]struct{})
		var ips []string
		task.mu.Lock()
		for _, ip := range task.landingIPs {
			if _, ok := unique[ip]; !ok {
				unique[ip] = struct{}{}
				ips = append(ips, ip)
			}
		}
		task.mu.Unlock()
		batch := m.ipapi.CheckBatch(task.ctx, ips)
		for _, node := range task.nodes {
			task.mu.Lock()
			ip := task.landingIPs[node.NodeID]
			task.mu.Unlock()
			quality, ok := batch[ip]
			if !ok {
				quality = ipquality.Result{Provider: "ip-api", Status: ipquality.StatusFailed, IP: ip, Reason: "exit IP unavailable", CheckedAt: time.Now().UTC()}
			}
			if ip != "" && quality.Status == ipquality.StatusSuccess && quality.CountryCode != "" && sameDetectedIP(ip, quality.IP) {
				now := time.Now().UTC()
				saveErr := m.server.store.UpsertNodeDetectionResult(context.Background(), &store.NodeDetectionResult{
					NodeID: node.NodeID, TaskID: task.snapshot.ID, LatencyStatus: "untested", SpeedStatus: "untested",
					ExitIP: ip, ExitIPFamily: detectIPFamily(ip), ExitCountry: quality.Country,
					ExitCountryCode: strings.ToUpper(strings.TrimSpace(quality.CountryCode)), ExitIPStatus: "success", ExitIPCheckedAt: now,
				})
				if saveErr == nil {
					markLocation(node, quality.CountryCode, quality.Country)
				}
			}
			m.saveQuality(task, node, quality)
			recordOutcome(node.NodeID, quality.Status)
		}
	} else {
		for _, node := range task.nodes {
			quality := ipquality.Result{Provider: "ip-api", Status: ipquality.StatusDisabled, Reason: "provider disabled", CheckedAt: time.Now().UTC()}
			m.saveQuality(task, node, quality)
			recordOutcome(node.NodeID, quality.Status)
		}
	}
	for _, node := range task.nodes {
		statuses := outcomes[node.NodeID]
		stageStatus := "failed"
		if !task.settings.IPPureEnabled && !task.settings.IPAPIEnabled {
			stageStatus = "skipped"
		} else {
			for _, status := range statuses {
				if status == ipquality.StatusSuccess || status == ipquality.StatusPartial {
					stageStatus = "success"
					break
				}
			}
		}
		task.updateStage("quality", stageStatus)
		task.publish(nodeCheckEvent{Type: "task", Phase: "quality", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: stageStatus})
	}
	locationMu.Lock()
	relocated := append([]int64(nil), relocatedNodeIDs...)
	locationMu.Unlock()
	if err := m.server.refreshGroupMembership(relocated); err != nil {
		task.publish(nodeCheckEvent{Type: "task", Phase: "quality", Status: "warning", Error: "落地地区已保存，但分组刷新失败: " + err.Error()})
	}
	m.persist(task)
}

// saveQuality persists one provider's IP-quality result. Every quality write in
// this file funnels through here, so this is also the one place the tag queue has
// to hear about a new ipq.* fact.
func (m *nodeCheckManager) saveQuality(task *nodeCheckTask, node Snapshot, quality ipquality.Result) {
	result := store.NodeIPQualityResult{NodeID: node.NodeID, TaskID: task.snapshot.ID, Provider: quality.Provider, Status: quality.Status, IP: quality.IP, Family: quality.Family, Country: quality.Country, CountryCode: quality.CountryCode, ASN: quality.ASN, Org: quality.Org, ISP: quality.ISP, IsBroadcast: quality.IsBroadcast, IsResidential: quality.IsResidential, FraudScore: quality.FraudScore, Proxy: quality.Proxy, Hosting: quality.Hosting, Mobile: quality.Mobile, Reason: quality.Reason, CheckedAt: quality.CheckedAt}
	_ = m.server.store.UpsertNodeIPQualityResult(context.Background(), &result)
	m.server.enqueueRetag(node.NodeID)
	task.publish(nodeCheckEvent{Type: "result", Phase: "quality", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: quality.Status, Error: quality.Reason, Quality: &result})
}

func (m *nodeCheckManager) runUnlock(task *nodeCheckTask) {
	runLimited(task.settings.QualityConcurrency, task.nodes, func(node Snapshot) {
		if task.ctx.Err() != nil {
			return
		}
		dial, err := m.server.mgr.DialerFor(node.Tag)
		var result *unlock.Result
		if err == nil {
			result, err = unlock.Check(task.ctx, unlock.DialFunc(dial), node.Tag, node.Name, m.server.geoipLookupForCheck(), 25*time.Second)
		}
		status, errText := "success", ""
		if err != nil {
			status, errText = "failed", err.Error()
		} else {
			m.server.persistUnlockResult(&node, result)
		}
		task.updateStage("unlock", status)
		task.publish(nodeCheckEvent{Type: "result", Phase: "unlock", NodeID: node.NodeID, Tag: node.Tag, Name: node.Name, Status: status, Error: errText})
		m.persist(task)
	})
}

func detectIPFamily(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return "ipv4"
	}
	return "ipv6"
}

func snapshotPointer(value nodeCheckTaskSnapshot) *nodeCheckTaskSnapshot { return &value }

func (m *nodeCheckManager) persist(task *nodeCheckTask) {
	if m.server.store == nil {
		return
	}
	snapshot := task.copySnapshot()
	stagesJSON, _ := json.Marshal(snapshot.Stages)
	statsJSON, _ := json.Marshal(snapshot.Stats)
	settingsJSON, _ := json.Marshal(snapshot.Settings)
	_ = m.server.store.UpsertNodeDetectionTask(context.Background(), &store.NodeDetectionTask{ID: snapshot.ID, Status: snapshot.Status, StagesJSON: string(stagesJSON), SettingsJSON: string(settingsJSON), StatsJSON: string(statsJSON), TotalNodes: snapshot.TotalNodes, CompletedNodes: snapshot.CompletedNodes, DownloadedBytes: snapshot.DownloadedBytes, Error: snapshot.Error, CreatedAt: snapshot.CreatedAt, StartedAt: snapshot.StartedAt, FinishedAt: snapshot.FinishedAt})
	if snapshot.Status == nodeCheckTaskCompleted || snapshot.Status == nodeCheckTaskCancelled || snapshot.Status == nodeCheckTaskFailed {
		_ = m.server.store.PruneNodeDetectionTasks(context.Background(), 20)
	}
}

func (m *nodeCheckManager) get(id string) *nodeCheckTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

func (m *nodeCheckManager) subscribe(task *nodeCheckTask) (<-chan nodeCheckEvent, func()) {
	channel := make(chan nodeCheckEvent, 64)
	task.mu.Lock()
	task.subs[channel] = struct{}{}
	task.mu.Unlock()
	return channel, func() {
		task.mu.Lock()
		if _, ok := task.subs[channel]; ok {
			delete(task.subs, channel)
			close(channel)
		}
		task.mu.Unlock()
	}
}

func (m *nodeCheckManager) cancel(id string) bool {
	task := m.get(id)
	if task == nil {
		return false
	}
	task.cancel()
	return true
}

func (m *nodeCheckManager) shutdown() {
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active != nil {
		active.cancel()
	}
}

func (m *nodeCheckManager) pruneMemory(keep int) {
	if keep < 1 {
		keep = 20
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tasks) <= keep {
		return
	}
	tasks := make([]*nodeCheckTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].copySnapshot().CreatedAt.After(tasks[j].copySnapshot().CreatedAt)
	})
	for _, task := range tasks[keep:] {
		delete(m.tasks, task.copySnapshot().ID)
	}
}

func taskSnapshotFromStore(task store.NodeDetectionTask) nodeCheckTaskSnapshot {
	result := nodeCheckTaskSnapshot{ID: task.ID, Status: task.Status, TotalNodes: task.TotalNodes, CompletedNodes: task.CompletedNodes, DownloadedBytes: task.DownloadedBytes, Error: task.Error, CreatedAt: task.CreatedAt, StartedAt: task.StartedAt, FinishedAt: task.FinishedAt, Settings: map[string]any{}, Stats: map[string]nodeCheckStageStats{}}
	_ = json.Unmarshal([]byte(task.StagesJSON), &result.Stages)
	_ = json.Unmarshal([]byte(task.SettingsJSON), &result.Settings)
	_ = json.Unmarshal([]byte(task.StatsJSON), &result.Stats)
	return result
}

func nodeCheckPublicSettings(settings config.NodeCheckConfig) map[string]any {
	return map[string]any{"latency_timeout": settings.LatencyTimeout.String(), "speed_duration": settings.SpeedDuration.String(), "speed_request_timeout": settings.SpeedRequestTimeout.String(), "quality_timeout": settings.QualityTimeout.String(), "max_download_bytes": settings.MaxDownloadBytes, "peak_sample_interval": settings.PeakSampleInterval.String(), "latency_concurrency": settings.LatencyConcurrency, "speed_concurrency": settings.SpeedConcurrency, "quality_concurrency": settings.QualityConcurrency, "include_handshake": settings.IncludeHandshake, "ippure_enabled": settings.IPPureEnabled, "ip_api_enabled": settings.IPAPIEnabled}
}

func nodeCheckTerminal(status string) bool {
	return status == nodeCheckTaskCompleted || status == nodeCheckTaskFailed || status == nodeCheckTaskCancelled || status == nodeCheckTaskInterrupted
}
