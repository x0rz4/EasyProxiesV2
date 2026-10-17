package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
)

// This file is Phase 0 of plane/monitor-server-domain-refactor.md: freeze the
// response shapes that the planned internal/httpapi split has to keep, for the
// domains the route-contract table does not already pin.
//
// routes_contract_test.go covers which routes exist and who may call them, and
// operations_test.go covers the probe/unlock SSE shape. What was still
// unpinned — and therefore what a cross-package move could silently change —
// is three things:
//
//   - the GeoIP management JSON (only route-level 401/405 was asserted),
//   - the traffic stream's initial event,
//   - the node-check task event stream's initial "task" event.
//
// These assert the wire shape, not the implementation, so they survive the
// handlers moving to another package unchanged. Where a handler reads through
// manager state, the fixture drives that state directly rather than sleeping.

// newGeoipContractServer returns a server bound to an explicit GeoIP database
// path. It builds the config struct directly rather than loading a file:
// config.Load fills an empty database_path with a default and rewrites relative
// paths against the config's directory, so the "unconfigured" case — the one
// that must answer 400 without reaching the network — is unreachable through
// the loader.
func newGeoipContractServer(t *testing.T, dbPath string) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.GeoIP.DatabasePath = dbPath
	return &Server{cfgSrc: cfg, logger: testDiscardLogger()}
}

func testDiscardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// TestGeoipStatusKeepsResponseShape pins the JSON the WebUI reads to render the
// GeoIP card. The refactor moves this handler across packages, so every key and
// its type is a contract.
func TestGeoipStatusKeepsResponseShape(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.mmdb")
	server := newGeoipContractServer(t, dbPath)

	response := httptest.NewRecorder()
	server.handleGeoipStatus(response, httptest.NewRequest(http.MethodGet, "/api/geoip/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Enabled  bool `json:"enabled"`
		Database struct {
			DatabasePath string `json:"database_path"`
			Exists       bool   `json:"exists"`
			SizeBytes    int64  `json:"size_bytes"`
			ModifiedAt   string `json:"modified_at"`
			DownloadURL  string `json:"download_url"`
			FallbackURL  string `json:"fallback_url"`
			SourceURL    string `json:"source_url"`
		} `json:"database"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, response.Body.String())
	}
	if payload.Enabled {
		t.Fatal("enabled should mirror the configured false")
	}
	if payload.Database.DatabasePath != dbPath {
		t.Fatalf("database_path = %q, want %q", payload.Database.DatabasePath, dbPath)
	}
	if payload.Database.Exists {
		t.Fatal("a missing db file must report exists=false")
	}
	if payload.Database.DownloadURL == "" {
		t.Fatal("download_url must be populated so the UI can offer a download")
	}
	// A missing file must not fabricate a size or mtime.
	if payload.Database.SizeBytes != 0 || payload.Database.ModifiedAt != "" {
		t.Fatalf("missing db reported size=%d modified=%q", payload.Database.SizeBytes, payload.Database.ModifiedAt)
	}
}

// TestGeoipStatusReportsExistingFile checks the other branch: an existing file
// yields exists=true with a real size and an RFC3339 UTC mtime.
func TestGeoipStatusReportsExistingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(dbPath, []byte("not-really-an-mmdb"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newGeoipContractServer(t, dbPath)

	response := httptest.NewRecorder()
	server.handleGeoipStatus(response, httptest.NewRequest(http.MethodGet, "/api/geoip/status", nil))
	var payload struct {
		Database struct {
			Exists     bool   `json:"exists"`
			SizeBytes  int64  `json:"size_bytes"`
			ModifiedAt string `json:"modified_at"`
		} `json:"database"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, response.Body.String())
	}
	if !payload.Database.Exists || payload.Database.SizeBytes <= 0 || payload.Database.ModifiedAt == "" {
		t.Fatalf("existing db reported %+v", payload.Database)
	}
	if !strings.HasSuffix(payload.Database.ModifiedAt, "Z") {
		t.Fatalf("modified_at = %q, want an RFC3339 UTC timestamp", payload.Database.ModifiedAt)
	}
}

// TestGeoipMutationRejectsMissingPathBeforeTouchingTheNetwork pins the 400 that
// guards download/update when no path is configured. It must answer before any
// HTTP fetch, so this test would hang rather than fail if that ordering broke.
func TestGeoipMutationRejectsMissingPathBeforeTouchingTheNetwork(t *testing.T) {
	server := newGeoipContractServer(t, "")

	for _, route := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"download", server.handleGeoipDownload},
		{"update", server.handleGeoipUpdate},
	} {
		t.Run(route.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			route.handler(response, httptest.NewRequest(http.MethodPost, "/api/geoip/"+route.name, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "GeoIP 数据库路径未配置") {
				t.Fatalf("body=%s", response.Body.String())
			}
		})
	}
}

// TestTrafficStreamKeepsInitialEventShape pins the traffic stream's opening
// frame. The stream ticks once a second forever, so the request carries an
// already-cancelled context: the handler emits the snapshot and then returns on
// the first select, leaving exactly one deterministic event in the recorder.
func TestTrafficStreamKeepsInitialEventShape(t *testing.T) {
	manager, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.Register(monitor.NodeInfo{Tag: "node-a", Name: "Node A"})

	server := &Server{mgr: manager, done: make(chan struct{}), logger: testDiscardLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/nodes/traffic/stream", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	server.handleTrafficStream(response, request)

	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if !response.Flushed {
		t.Fatal("traffic stream did not flush its initial event")
	}
	body := response.Body.String()
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("body does not start with an SSE data frame: %q", body)
	}
	payload := decodeSSEPayload(t, body)
	for _, key := range []string{"type", "node_count", "total_upload", "total_download", "upload_speed", "download_speed", "sampled_at", "nodes"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("initial traffic event missing %q: %s", key, body)
		}
	}
	if payload["type"] != "traffic" {
		t.Fatalf("type = %v, want traffic", payload["type"])
	}
	if payload["node_count"] != float64(1) {
		t.Fatalf("node_count = %v, want 1", payload["node_count"])
	}
}

// TestNodeCheckTaskEventsKeepsInitialEventShape pins the node-check stream's
// opening frame and its early return for an already-terminal task. A task that
// is terminal emits its snapshot and closes immediately, which is what makes
// this deterministic without a running detection.
func TestNodeCheckTaskEventsKeepsInitialEventShape(t *testing.T) {
	server := &Server{mgr: mustNewManager(t), logger: testDiscardLogger()}
	manager := newNodeCheckManager(server)
	server.nodeChecks = manager

	task := &nodeCheckTask{
		snapshot: nodeCheckTaskSnapshot{
			ID: "task-1", Status: nodeCheckTaskCompleted,
			Stages: nodeCheckStages{Latency: true}, Stats: map[string]nodeCheckStageStats{},
			TotalNodes: 2, CompletedNodes: 2,
		},
		subs:      map[chan nodeCheckEvent]struct{}{},
		latencyOK: map[int64]bool{},
	}
	manager.mu.Lock()
	manager.tasks["task-1"] = task
	manager.active = task
	manager.mu.Unlock()

	request := httptest.NewRequest(http.MethodGet, "/api/node-check/tasks/task-1/events", nil)
	request.SetPathValue("taskID", "task-1")
	response := httptest.NewRecorder()
	server.handleNodeCheckTaskEvents(response, request)

	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := response.Body.String()
	payload := decodeSSEPayload(t, body)
	if payload["type"] != "task" {
		t.Fatalf("initial event type = %v, want task (body=%s)", payload["type"], body)
	}
	task_, ok := payload["task"].(map[string]any)
	if !ok {
		t.Fatalf("initial event has no task object: %s", body)
	}
	if task_["id"] != "task-1" || task_["status"] != nodeCheckTaskCompleted {
		t.Fatalf("task snapshot = %v", task_)
	}
	// A terminal task must not leave the stream open.
	if !strings.Contains(body, `"total_nodes":2`) {
		t.Fatalf("task snapshot lost total_nodes: %s", body)
	}
}

// TestNodeCheckTaskEventsUnknownTaskIsNotFound covers the other early exit: a
// task that is not in memory answers 404 rather than opening a stream that will
// never produce an event.
func TestNodeCheckTaskEventsUnknownTaskIsNotFound(t *testing.T) {
	server := &Server{mgr: mustNewManager(t), logger: testDiscardLogger()}
	server.nodeChecks = newNodeCheckManager(server)

	request := httptest.NewRequest(http.MethodGet, "/api/node-check/tasks/missing/events", nil)
	request.SetPathValue("taskID", "missing")
	response := httptest.NewRecorder()
	server.handleNodeCheckTaskEvents(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", response.Code, response.Body.String())
	}
}

func mustNewManager(t *testing.T) *monitor.Manager {
	t.Helper()
	manager, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	return manager
}

// decodeSSEPayload extracts the first data: frame's JSON object.
func decodeSSEPayload(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		payload, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			t.Fatalf("decode SSE frame %q: %v", payload, err)
		}
		return value
	}
	t.Fatalf("no data frame in body: %q", body)
	return nil
}
