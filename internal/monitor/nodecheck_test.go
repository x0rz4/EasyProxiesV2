package monitor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

func TestNodeCheckSettingsAPIRejectsEnabledIPPureWithoutURL(t *testing.T) {
	server, _, _ := newOperationsTestServer(t)
	get := httptest.NewRecorder()
	server.handleNodeCheckSettings(get, httptest.NewRequest(http.MethodGet, "/api/operations/node-check-settings", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	var settings nodeCheckSettingsResponse
	if err := json.Unmarshal(get.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	settings.IPPureEnabled = true
	settings.IPPureURL = ""
	body, _ := json.Marshal(settings)
	put := httptest.NewRecorder()
	server.handleNodeCheckSettings(put, httptest.NewRequest(http.MethodPut, "/api/operations/node-check-settings", strings.NewReader(string(body))))
	if put.Code != http.StatusBadRequest {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
}

func TestNodeCheckLatencyDoesNotMutateHealth(t *testing.T) {
	server, mgr, _ := newOperationsTestServer(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "node-check.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	node := &store.Node{URI: "http://user:pass@127.0.0.1:8080", Name: "one", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	server.cfgSrc.Lock()
	server.cfgSrc.Management.NodeCheck.LatencyURL = target.URL
	server.cfgSrc.Management.NodeCheck.LatencyTimeout = time.Second
	server.cfgSrc.Management.NodeCheck.IncludeHandshake = false
	server.cfgSrc.Unlock()
	server.SetStore(db)
	handle := mgr.Register(NodeInfo{NodeID: node.ID, Tag: "node-one", Name: "one", URI: node.URI})
	handle.SetDialer(func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})
	before := mgr.SnapshotForTag("node-one")
	task, err := server.nodeChecks.create(nodeCheckCreateRequest{NodeIDs: []int64{node.ID}, Stages: nodeCheckStages{Latency: true}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !nodeCheckTerminal(task.copySnapshot().Status) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	after := mgr.SnapshotForTag("node-one")
	if before.Available != after.Available || before.InitialCheckDone != after.InitialCheckDone || before.FailureCount != after.FailureCount || before.LastLatencyMs != after.LastLatencyMs {
		t.Fatalf("health mutated: before=%+v after=%+v", before, after)
	}
	results, err := db.ListNodeDetectionResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if results[node.ID] == nil || results[node.ID].LatencyStatus != "success" || results[node.ID].LatencyMs == nil {
		t.Fatalf("diagnostic result missing: %+v", results[node.ID])
	}
}
