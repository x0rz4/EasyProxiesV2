package monitor

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"easy_proxies/internal/config"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/store"
)

type nodeCheckSettingsResponse struct {
	LatencyURL          string `json:"latency_url"`
	SpeedURL            string `json:"speed_url"`
	LandingIPURL        string `json:"landing_ip_url"`
	LatencyTimeout      string `json:"latency_timeout"`
	SpeedDuration       string `json:"speed_duration"`
	SpeedRequestTimeout string `json:"speed_request_timeout"`
	QualityTimeout      string `json:"quality_timeout"`
	MaxDownloadBytes    int64  `json:"max_download_bytes"`
	PeakSampleInterval  string `json:"peak_sample_interval"`
	LatencyConcurrency  int    `json:"latency_concurrency"`
	SpeedConcurrency    int    `json:"speed_concurrency"`
	QualityConcurrency  int    `json:"quality_concurrency"`
	IncludeHandshake    bool   `json:"include_handshake"`
	IPPureEnabled       bool   `json:"ippure_enabled"`
	IPPureURL           string `json:"ippure_url"`
	IPAPIEnabled        bool   `json:"ip_api_enabled"`
	IPAPIBaseURL        string `json:"ip_api_base_url"`
}

func nodeCheckSettingsFromConfig(value config.NodeCheckConfig) nodeCheckSettingsResponse {
	return nodeCheckSettingsResponse{
		LatencyURL: value.LatencyURL, SpeedURL: value.SpeedURL, LandingIPURL: value.LandingIPURL,
		LatencyTimeout: value.LatencyTimeout.String(), SpeedDuration: value.SpeedDuration.String(),
		SpeedRequestTimeout: value.SpeedRequestTimeout.String(), QualityTimeout: value.QualityTimeout.String(),
		MaxDownloadBytes: value.MaxDownloadBytes, PeakSampleInterval: value.PeakSampleInterval.String(),
		LatencyConcurrency: value.LatencyConcurrency, SpeedConcurrency: value.SpeedConcurrency,
		QualityConcurrency: value.QualityConcurrency, IncludeHandshake: value.IncludeHandshake,
		IPPureEnabled: value.IPPureEnabled, IPPureURL: value.IPPureURL,
		IPAPIEnabled: value.IPAPIEnabled, IPAPIBaseURL: value.IPAPIBaseURL,
	}
}

func (r nodeCheckSettingsResponse) toConfig() (config.NodeCheckConfig, error) {
	parse := func(name, raw string) (time.Duration, error) { return parsePositiveDuration(name, raw) }
	latencyTimeout, err := parse("检测延迟超时", r.LatencyTimeout)
	if err != nil {
		return config.NodeCheckConfig{}, err
	}
	speedDuration, err := parse("测速持续时间", r.SpeedDuration)
	if err != nil {
		return config.NodeCheckConfig{}, err
	}
	speedRequestTimeout, err := parse("测速请求超时", r.SpeedRequestTimeout)
	if err != nil {
		return config.NodeCheckConfig{}, err
	}
	qualityTimeout, err := parse("质量检测超时", r.QualityTimeout)
	if err != nil {
		return config.NodeCheckConfig{}, err
	}
	peakInterval, err := parse("峰值采样间隔", r.PeakSampleInterval)
	if err != nil {
		return config.NodeCheckConfig{}, err
	}
	value := config.NodeCheckConfig{LatencyURL: strings.TrimSpace(r.LatencyURL), SpeedURL: strings.TrimSpace(r.SpeedURL), LandingIPURL: strings.TrimSpace(r.LandingIPURL), LatencyTimeout: latencyTimeout, SpeedDuration: speedDuration, SpeedRequestTimeout: speedRequestTimeout, QualityTimeout: qualityTimeout, MaxDownloadBytes: r.MaxDownloadBytes, PeakSampleInterval: peakInterval, LatencyConcurrency: r.LatencyConcurrency, SpeedConcurrency: r.SpeedConcurrency, QualityConcurrency: r.QualityConcurrency, IncludeHandshake: r.IncludeHandshake, IPPureEnabled: r.IPPureEnabled, IPPureURL: strings.TrimSpace(r.IPPureURL), IPAPIEnabled: r.IPAPIEnabled, IPAPIBaseURL: strings.TrimSpace(r.IPAPIBaseURL)}
	if err := config.ValidateNodeCheckConfig(value); err != nil {
		return config.NodeCheckConfig{}, err
	}
	return value, nil
}

func (s *Server) handleNodeCheckSettings(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	source := s.cfgSrc
	s.cfgMu.RUnlock()
	if source == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "配置存储未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, nodeCheckSettingsFromConfig(source.Snapshot().Management.NodeCheck))
	case http.MethodPut:
		var request nodeCheckSettingsResponse
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		value, err := request.toConfig()
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.configMutationMu.Lock()
		defer s.configMutationMu.Unlock()
		snapshot := source.Snapshot()
		old := snapshot.Management.NodeCheck
		source.Lock()
		source.Management.NodeCheck = value
		source.Unlock()
		if err := source.SaveSettings(); err != nil {
			source.Lock()
			source.Management.NodeCheck = old
			source.Unlock()
			writeAPIError(w, http.StatusInternalServerError, "保存检测设置失败: "+err.Error())
			return
		}
		writeJSON(w, nodeCheckSettingsFromConfig(value))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNodeCheckTasks(w http.ResponseWriter, r *http.Request) {
	if s.nodeChecks == nil || s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "综合检测服务未初始化")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var request nodeCheckCreateRequest
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		task, err := s.nodeChecks.create(request)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		writeNodeCheckJSONStatus(w, http.StatusAccepted, map[string]any{"task": task.copySnapshot()})
	case http.MethodGet:
		tasks, err := s.store.ListNodeDetectionTasks(r.Context(), 20)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		result := make([]nodeCheckTaskSnapshot, 0, len(tasks))
		for _, task := range tasks {
			result = append(result, taskSnapshotFromStore(task))
		}
		writeJSON(w, map[string]any{"tasks": result})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNodeCheckTaskItem(w http.ResponseWriter, r *http.Request) {
	if s.nodeChecks == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "综合检测服务未初始化")
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/node-check/tasks/"), "/")
	parts := strings.Split(path, "/")
	if path == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	id := parts[0]
	if len(parts) == 2 && parts[1] == "events" {
		s.streamNodeCheckTask(w, r, id)
		return
	}
	if len(parts) != 1 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if task := s.nodeChecks.get(id); task != nil {
			writeJSON(w, map[string]any{"task": task.copySnapshot()})
			return
		}
		tasks, err := s.store.ListNodeDetectionTasks(r.Context(), 100)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, task := range tasks {
			if task.ID == id {
				writeJSON(w, map[string]any{"task": taskSnapshotFromStore(task)})
				return
			}
		}
		writeAPIError(w, http.StatusNotFound, "检测任务不存在")
	case http.MethodDelete:
		if !s.nodeChecks.cancel(id) {
			writeAPIError(w, http.StatusNotFound, "检测任务不存在或已结束")
			return
		}
		writeJSON(w, map[string]any{"message": "已请求取消检测任务"})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) streamNodeCheckTask(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	task := s.nodeChecks.get(id)
	if task == nil {
		writeAPIError(w, http.StatusNotFound, "检测任务不存在或已归档")
		return
	}
	initial := nodeCheckEvent{Type: "task", Task: snapshotPointer(task.copySnapshot())}
	writeSSEJSON(w, initial)
	flusher.Flush()
	if nodeCheckTerminal(initial.Task.Status) {
		return
	}
	events, unsubscribe := s.nodeChecks.subscribe(task)
	defer unsubscribe()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			writeSSEJSON(w, event)
			flusher.Flush()
			if event.Type == "done" {
				return
			}
		}
	}
}

func writeSSEJSON(w http.ResponseWriter, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
}

func writeNodeCheckJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) handleNodeCheckResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储未初始化")
		return
	}
	detections, err := s.store.ListNodeDetectionResults(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	quality, err := s.store.ListNodeIPQualityResults(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		NodeID      int64                       `json:"node_id"`
		Detection   *store.NodeDetectionResult  `json:"detection,omitempty"`
		Quality     []store.NodeIPQualityResult `json:"quality"`
		ExitIPDrift bool                        `json:"exit_ip_drift"`
	}
	ids := make(map[int64]struct{})
	for _, snapshot := range s.mgr.Snapshot() {
		if snapshot.NodeID > 0 {
			ids[snapshot.NodeID] = struct{}{}
		}
	}
	for id := range detections {
		ids[id] = struct{}{}
	}
	for id := range quality {
		ids[id] = struct{}{}
	}
	result := make([]item, 0, len(ids))
	s.cfgMu.RLock()
	var checkConfig config.NodeCheckConfig
	if s.cfgSrc != nil {
		checkConfig = s.cfgSrc.Snapshot().Management.NodeCheck
	}
	s.cfgMu.RUnlock()
	for id := range ids {
		drift := false
		seen := ""
		providerResults := quality[id]
		if providerResults == nil {
			providerResults = []store.NodeIPQualityResult{}
		}
		seenProviders := make(map[string]struct{}, len(providerResults))
		for _, provider := range providerResults {
			seenProviders[provider.Provider] = struct{}{}
		}
		for _, provider := range []struct {
			name    string
			enabled bool
		}{{"ippure", checkConfig.IPPureEnabled}, {"ip-api", checkConfig.IPAPIEnabled}} {
			if _, ok := seenProviders[provider.name]; ok {
				continue
			}
			status := "untested"
			if !provider.enabled {
				status = "disabled"
			}
			providerResults = append(providerResults, store.NodeIPQualityResult{NodeID: id, Provider: provider.name, Status: status})
		}
		sort.Slice(providerResults, func(i, j int) bool { return providerResults[i].Provider < providerResults[j].Provider })
		if detection := detections[id]; detection != nil {
			seen = detection.ExitIP
		}
		for _, provider := range providerResults {
			if provider.IP != "" && seen != "" && !sameDetectedIP(provider.IP, seen) {
				drift = true
			}
			if seen == "" {
				seen = provider.IP
			}
		}
		result = append(result, item{NodeID: id, Detection: detections[id], Quality: providerResults, ExitIPDrift: drift})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NodeID < result[j].NodeID })
	writeJSON(w, map[string]any{"results": result})
}

func sameDetectedIP(left, right string) bool {
	leftIP, rightIP := net.ParseIP(left), net.ParseIP(right)
	if leftIP != nil && rightIP != nil {
		return leftIP.Equal(rightIP)
	}
	return left == right
}
