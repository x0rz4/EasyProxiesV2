package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodedetect"
	"easy_proxies/internal/noderesult"
	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"
)

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	// 返回所有注册节点，让前端根据状态过滤展示
	allNodes := s.mgr.Snapshot()
	totalNodes := len(allNodes)

	// Calculate region statistics and traffic totals
	regionStats := make(map[string]int)
	regionHealthy := make(map[string]int)
	for _, snap := range allNodes {
		region := snap.Region
		if region == "" {
			region = "other"
		}
		regionStats[region]++
		// Count healthy nodes per region
		if snap.InitialCheckDone && snap.Available && !snap.Blacklisted {
			regionHealthy[region]++
		}
	}

	traffic := s.mgr.TrafficSummary(false)

	payload := map[string]any{
		"nodes":           allNodes,
		"total_nodes":     totalNodes,
		"total_upload":    traffic.TotalUpload,
		"total_download":  traffic.TotalDownload,
		"upload_speed":    traffic.UploadSpeed,
		"download_speed":  traffic.DownloadSpeed,
		"traffic_sampled": traffic.SampledAt,
		"region_stats":    regionStats,
		"region_healthy":  regionHealthy,
	}
	writeJSON(w, payload)
}

func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	snapshots := s.mgr.Snapshot()
	var totalCalls, totalSuccess int64
	debugNodes := make([]map[string]any, 0, len(snapshots))
	for _, snap := range snapshots {
		totalCalls += snap.SuccessCount + int64(snap.FailureCount)
		totalSuccess += snap.SuccessCount
		debugNodes = append(debugNodes, map[string]any{
			"tag":                snap.Tag,
			"name":               snap.Name,
			"mode":               snap.Mode,
			"port":               snap.Port,
			"failure_count":      snap.FailureCount,
			"success_count":      snap.SuccessCount,
			"active_connections": snap.ActiveConnections,
			"last_latency_ms":    snap.LastLatencyMs,
			"last_success":       snap.LastSuccess,
			"last_failure":       snap.LastFailure,
			"last_error":         snap.LastError,
			"blacklisted":        snap.Blacklisted,
			"total_upload":       snap.TotalUpload,
			"total_download":     snap.TotalDownload,
			"timeline":           snap.Timeline,
		})
	}
	var successRate float64
	if totalCalls > 0 {
		successRate = float64(totalSuccess) / float64(totalCalls) * 100
	}
	writeJSON(w, map[string]any{
		"nodes":         debugNodes,
		"total_calls":   totalCalls,
		"total_success": totalSuccess,
		"success_rate":  successRate,
	})
}

func (s *Server) handleDebugStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := s.mgr.SubscribeDebugLogs()
	defer unsubscribe()
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case event := <-events:
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err = fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// nodeActionTag resolves the {tag} path segment for the per-node action
// routes. An empty tag is a malformed route match, not a missing node.
func (s *Server) nodeActionTag(w http.ResponseWriter, r *http.Request) (string, bool) {
	tag := r.PathValue("tag")
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		return "", false
	}
	return tag, true
}

func (s *Server) handleNodeProbe(w http.ResponseWriter, r *http.Request) {
	tag, ok := s.nodeActionTag(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	latency, err := s.mgr.Probe(ctx, tag)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	latencyMs := latency.Milliseconds()
	if latencyMs == 0 && latency > 0 {
		latencyMs = 1 // Round up sub-millisecond latencies to 1ms
	}
	writeJSON(w, map[string]any{"message": "探测成功", "latency_ms": latencyMs})
}

func (s *Server) handleNodeRelease(w http.ResponseWriter, r *http.Request) {
	tag, ok := s.nodeActionTag(w, r)
	if !ok {
		return
	}
	if err := s.mgr.Release(tag); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"message": "已解除拉黑"})
}

func (s *Server) handleNodeSpeedtest(w http.ResponseWriter, r *http.Request) {
	tag, ok := s.nodeActionTag(w, r)
	if !ok {
		return
	}
	release, acquireErr := s.acquireDiagnostic(r.Context(), probe.Speed)
	if acquireErr != nil {
		writeAPIError(w, http.StatusServiceUnavailable, acquireErr.Error())
		return
	}
	defer release()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	dialer, err := s.mgr.DialerFor(tag)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}

	options := nodedetect.SpeedOptions{}
	s.cfgMu.RLock()
	if s.cfgSrc != nil {
		settings := s.cfgSrc.Snapshot().Management.NodeCheck
		options = nodedetect.SpeedOptions{URL: settings.SpeedURL, Duration: settings.SpeedDuration, RequestTimeout: settings.SpeedRequestTimeout, MaxBytes: settings.MaxDownloadBytes, PeakSampleInterval: settings.PeakSampleInterval}
	}
	s.cfgMu.RUnlock()
	event, beginErr := s.beginMeasuredResult(r.Context(), s.mgr.SnapshotForTag(tag), probe.Speed, probe.ManualDiagnostic)
	if beginErr != nil {
		payload, _ := json.Marshal(map[string]string{"error": beginErr.Error()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		flusher.Flush()
		return
	}
	runner := &monitor.SpeedtestRunner{Options: options}
	result, runErr := runner.Run(r.Context(), dialer, func(mbps float64, isDone bool) {
		if isDone {
			return
		}
		fmt.Fprintf(w, "event: progress\ndata: {\"mbps\": %.2f}\n\n", mbps)
		flusher.Flush()
	})
	now := time.Now().UTC()
	stored := &store.NodeDetectionResult{NodeID: event.NodeID, TaskID: "legacy-speedtest", LatencyStatus: "untested", SpeedStatus: "success", BytesDownloaded: result.BytesDownloaded, SpeedDurationMs: result.DurationMs, SpeedCheckedAt: now, ExitIPStatus: "untested"}
	if runErr != nil {
		stored.SpeedStatus = "failed"
		stored.SpeedError = runErr.Error()
	}
	if result.BytesDownloaded > 0 {
		stored.AverageBytesPerSecond = &result.AverageBytesPerSecond
		stored.PeakBytesPerSecond = &result.PeakBytesPerSecond
	}
	event.Detection = stored
	if err := measurementError(runErr, s.applyMeasuredResult(event)); err != nil {
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		flusher.Flush()
		return
	}
	fmt.Fprintf(w, "event: done\ndata: {\"mbps\": %.2f}\n\n", float64(result.AverageBytesPerSecond)*8/1_000_000)
	flusher.Flush()
}

func (s *Server) handleNodeUnlock(w http.ResponseWriter, r *http.Request) {
	tag, ok := s.nodeActionTag(w, r)
	if !ok {
		return
	}
	// Unlock checks issue several HTTP requests through the node, so allow
	// more time than a single latency probe.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	release, acquireErr := s.acquireDiagnostic(ctx, probe.Unlock)
	if acquireErr != nil {
		writeAPIError(w, http.StatusServiceUnavailable, acquireErr.Error())
		return
	}
	defer release()
	dialer, err := s.mgr.DialerFor(tag)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	name := tag
	snap := s.mgr.SnapshotForTag(tag)
	if snap != nil && snap.Name != "" {
		name = snap.Name
	}
	event, beginErr := s.beginMeasuredResult(ctx, snap, probe.Unlock, probe.UnlockCheck)
	if beginErr != nil {
		writeAPIError(w, http.StatusConflict, beginErr.Error())
		return
	}
	result, err := unlock.Check(ctx, unlock.DialFunc(dialer), tag, name, s.geoipLookupForCheck(), 25*time.Second)
	if result == nil {
		result = &unlock.Result{Tag: tag, Name: name}
	}
	if err != nil {
		result.Error = err.Error()
	}
	if persistErr := s.persistUnlockResult(event, result); persistErr != nil {
		writeAPIError(w, http.StatusInternalServerError, persistErr.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, result)
}

// handleProbeAll probes all nodes in batches and returns results via SSE.
// The write side runs in its own goroutine: the result callback is invoked on
// the probe scheduler's actor goroutine, and a synchronous write to a slow or
// stalled SSE client would freeze the whole scheduler (event and command
// buffers back up, periodic probing and reload enqueueing stall with it).
// Progress frames are expendable and dropped when the client cannot keep up;
// the per-write deadline bounds how long a wedged connection can hold the
// writer goroutine.
func (s *Server) handleProbeAll(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	// Headers are set before any write; the start event no longer doubles as
	// the header setter because the writer goroutine owns all writes now.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	type sseFrame struct{ data []byte }
	frames := make(chan sseFrame, 128)
	var writeFailed atomic.Bool
	responseController := http.NewResponseController(w)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for frame := range frames {
			_ = responseController.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintf(w, "data: %s\n\n", frame.data); err != nil {
				writeFailed.Store(true)
				for range frames { // drain so producers never block on a dead client
				}
				return
			}
			flusher.Flush()
		}
	}()
	send := func(payload map[string]any) {
		if writeFailed.Load() {
			return
		}
		data, _ := json.Marshal(payload)
		select {
		case frames <- sseFrame{data: data}:
		default: // the client cannot keep up; progress frames are expendable
		}
	}

	started := false
	completed := 0
	total := 0
	summary, err := s.mgr.RunProbeBatch(r.Context(), monitor.ProbeRoundManual, false, func(count int) {
		started, total = true, count
		send(map[string]any{"type": "start", "total": total})
	}, func(result monitor.ProbeBatchResult) {
		completed++
		status, errorMessage := "success", ""
		latency := result.Latency.Milliseconds()
		if result.Err != nil {
			status, errorMessage, latency = "error", result.Err.Error(), -1
		}
		progress := float64(100)
		if total > 0 {
			progress = float64(completed) / float64(total) * 100
		}
		send(map[string]any{
			"type": "progress", "tag": result.Tag, "name": result.Name,
			"latency": latency, "status": status, "error": errorMessage,
			"current": completed, "total": total, "progress": progress,
		})
	})
	// The completion frame carries the totals the UI needs; give it a short
	// blocking window instead of the drop-on-full path used for progress. It
	// must be sent before the channel is closed below.
	if err == nil {
		data, _ := json.Marshal(map[string]any{
			"type": "complete", "total": summary.Total,
			"success": summary.Success, "failed": summary.Failed,
		})
		select {
		case frames <- sseFrame{data: data}:
		case <-time.After(1 * time.Second):
		}
	}
	close(frames)
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
	}
	if errors.Is(err, monitor.ErrProbeRoundInProgress) && !started {
		writeAPIError(w, http.StatusConflict, "已有全量探测正在运行")
		return
	}
	if err != nil {
		if !started {
			writeAPIError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
}

// handleUnlockAll runs unlock checks for all nodes and streams results via SSE.
// The event sequence mirrors handleProbeAll: {"type":"start","total":N},
// repeated {"type":"progress",...} (one per node, carrying the full Result),
// and a final {"type":"complete",...}. Each progress event contains the
// node's unlock.Result serialized as the "result" field.
func (s *Server) handleUnlockAll(w http.ResponseWriter, r *http.Request) {

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Get all nodes
	snapshots := s.mgr.Snapshot()
	total := len(snapshots)
	if total == 0 {
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"complete","total":0,"success":0,"failed":0}`)
		flusher.Flush()
		return
	}

	// Send start event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"start","total":%d}`, total))
	flusher.Flush()

	// Unlock checks are heavier than latency probes, so allow a generous
	// overall deadline (each node can take up to ~25s).
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	// The per-batch errgroup limit prevents this request from launching an
	// unbounded goroutine set. The shared weighted semaphore remains the
	// cross-request limit for all concurrent unlock batches.
	results := collectUnlockBatch(ctx, batchOperationConcurrency(), s.diagnosticSemaphore(probe.Unlock), snapshots, func(ctx context.Context, snap monitor.Snapshot) unlockBatchResult {
		// Execute unlock check
		checkCtx, checkCancel := context.WithTimeout(ctx, 60*time.Second)
		defer checkCancel()

		dialer, err := s.mgr.DialerFor(snap.Tag)
		if err != nil {
			return unlockBatchResult{err: err.Error()}
		}
		event, beginErr := s.beginMeasuredResult(checkCtx, &snap, probe.Unlock, probe.UnlockCheck)
		if beginErr != nil {
			return unlockBatchResult{err: beginErr.Error()}
		}
		res, err := unlock.Check(checkCtx, unlock.DialFunc(dialer), snap.Tag, snap.Name, s.geoipLookupForCheck(), 25*time.Second)
		if res == nil {
			res = &unlock.Result{Tag: snap.Tag, Name: snap.Name}
		}
		if err != nil {
			res.Error = err.Error()
		}
		if combined := measurementError(err, s.persistUnlockResult(event, res)); combined != nil {
			return unlockBatchResult{err: combined.Error()}
		}
		return unlockBatchResult{result: res}
	})

	// Collect and stream results
	successCount := 0
	failedCount := 0
	count := 0

	// progressEvent is the SSE payload for one completed node.
	type progressEvent struct {
		Type     string         `json:"type"`
		Tag      string         `json:"tag"`
		Name     string         `json:"name"`
		Status   string         `json:"status"`
		Error    string         `json:"error,omitempty"`
		Result   *unlock.Result `json:"result,omitempty"`
		Current  int            `json:"current"`
		Total    int            `json:"total"`
		Progress float64        `json:"progress"`
	}

	for result := range results {
		count++
		hasError := result.err != ""
		if hasError {
			failedCount++
		} else {
			successCount++
		}

		progress := float64(count) / float64(total) * 100

		ev := progressEvent{
			Type:     "progress",
			Current:  count,
			Total:    total,
			Progress: progress,
		}
		if hasError {
			ev.Status = "error"
			ev.Error = result.err
		} else {
			ev.Tag = result.result.Tag
			ev.Name = result.result.Name
			ev.Status = "success"
			ev.Result = result.result
		}

		data, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Send complete event
	fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"type":"complete","total":%d,"success":%d,"failed":%d}`, total, successCount, failedCount))
	flusher.Flush()
}

// handleUnlockResults returns the latest persisted unlock detection result for
// every node that has one, keyed by the stable node ID. Runtime tags carry a
// generation suffix and are regenerated whenever a node is rebuilt by a reload,
// so keying the response by tag would strand every stored result behind a tag
// the WebUI no longer knows about. It lets the WebUI show previously saved
// detections without re-running the checks. It is read-only and never touches
// the live probes.
func (s *Server) handleUnlockResults(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, map[string]any{"results": map[string]any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := s.store.ListUnlockResults(ctx)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	// unlockResultView is the JSON shape served to the WebUI. It mirrors the
	// live unlock.Result plus the owning node ID and a checked_at timestamp,
	// omitting the redundant result_json blob (Services are already
	// reconstructed server-side from that blob). Tag is retained for display
	// only; it is the tag recorded at check time, not necessarily the current
	// runtime tag.
	type unlockResultView struct {
		NodeID int64 `json:"node_id"`
		noderesult.Unlock
		CheckedAt time.Time `json:"checked_at"`
	}

	out := make(map[string]unlockResultView, len(stored))
	for nodeID, res := range stored {
		if res == nil || nodeID <= 0 {
			continue
		}
		out[strconv.FormatInt(nodeID, 10)] = unlockResultView{
			NodeID:    nodeID,
			Unlock:    res.Unlock,
			CheckedAt: res.CheckedAt,
		}
	}
	writeJSON(w, map[string]any{"results": out})
}

// handleUnlockMeta exposes detector and status metadata independently from a
// live node check so clients can render new registered modules dynamically.
func (s *Server) handleUnlockMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"providers": unlock.ListProviderMetas(),
		"statuses":  unlock.ListStatusMetas(),
	})
}

func (s *Server) handleTrafficStream(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	send := func(payload map[string]any) bool {
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Initial snapshot
	initial := s.mgr.TrafficSummary(true)
	if !send(map[string]any{
		"type":           "traffic",
		"node_count":     initial.NodeCount,
		"total_upload":   initial.TotalUpload,
		"total_download": initial.TotalDownload,
		"upload_speed":   initial.UploadSpeed,
		"download_speed": initial.DownloadSpeed,
		"sampled_at":     initial.SampledAt,
		"nodes":          initial.Nodes,
	}) {
		return
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			summary := s.mgr.TrafficSummary(true)
			ok := send(map[string]any{
				"type":           "traffic",
				"node_count":     summary.NodeCount,
				"total_upload":   summary.TotalUpload,
				"total_download": summary.TotalDownload,
				"upload_speed":   summary.UploadSpeed,
				"download_speed": summary.DownloadSpeed,
				"sampled_at":     summary.SampledAt,
				"nodes":          summary.Nodes,
			})
			if !ok {
				return
			}
		}
	}
}

// persistUnlockResult applies a report using the identity captured before dialing.
// Service regions remain unlock facts; they are not exit-location observations.
func (s *Server) persistUnlockResult(event probe.Result, result *unlock.Result) error {
	if s == nil || s.store == nil || result == nil {
		return fmt.Errorf("解锁结果或存储不可用")
	}
	nodeID := event.NodeID

	// Serialize the full result once and store it verbatim so the WebUI can
	// reconstruct per-service detail/region exactly as reported.
	resultJSON := ""
	if data, err := json.Marshal(result); err == nil {
		resultJSON = string(data)
	}

	event.Unlock = &store.UnlockResult{
		NodeID:     nodeID,
		Unlock:     *result,
		CheckedAt:  time.Now().UTC(),
		ResultJSON: resultJSON,
	}
	return s.applyMeasuredResult(event)
}
