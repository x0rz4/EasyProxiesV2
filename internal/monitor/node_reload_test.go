package monitor

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"easy_proxies/internal/config"
)

type reloadNodeManager struct{ reloads atomic.Int32 }

func (m *reloadNodeManager) ListConfigNodes(context.Context, *int64) ([]ManagedNodeConfig, error) {
	return nil, nil
}
func (m *reloadNodeManager) CreateNode(_ context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	return node, nil
}
func (m *reloadNodeManager) UpdateNode(_ context.Context, _ string, node config.NodeConfig) (config.NodeConfig, error) {
	return node, nil
}
func (m *reloadNodeManager) DeleteNode(context.Context, string) error { return nil }
func (m *reloadNodeManager) SetNodeEnabled(context.Context, string, bool) error {
	return nil
}
func (m *reloadNodeManager) TriggerReload(context.Context) error { m.reloads.Add(1); return nil }

func TestNodeCRUDTriggersReloadAndReportsResult(t *testing.T) {
	nodeManager := &reloadNodeManager{}
	server := &Server{nodeMgr: nodeManager, logger: log.Default()}
	payload := `{"name":"node-a","uri":"socks5://127.0.0.1:1080","port":1080}`

	createReq := httptest.NewRequest(http.MethodPost, "/api/nodes/config", strings.NewReader(payload))
	createResp := httptest.NewRecorder()
	server.routes().ServeHTTP(createResp, createReq)
	assertReloadResponse(t, createResp)
	if nodeManager.reloads.Load() != 1 {
		t.Fatalf("reloads after create = %d", nodeManager.reloads.Load())
	}

	updateReq := httptest.NewRequest(http.MethodPut, "/api/nodes/config/node-a", strings.NewReader(payload))
	updateResp := httptest.NewRecorder()
	server.routes().ServeHTTP(updateResp, updateReq)
	assertReloadResponse(t, updateResp)
	if nodeManager.reloads.Load() != 2 {
		t.Fatalf("reloads after update = %d", nodeManager.reloads.Load())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/nodes/config/node-a", nil)
	deleteResp := httptest.NewRecorder()
	server.routes().ServeHTTP(deleteResp, deleteReq)
	assertReloadResponse(t, deleteResp)
	if nodeManager.reloads.Load() != 3 {
		t.Fatalf("reloads after delete = %d", nodeManager.reloads.Load())
	}
}

func assertReloadResponse(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Reloaded    bool   `json:"reloaded"`
		ReloadError string `json:"reload_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Reloaded || body.ReloadError != "" {
		t.Fatalf("reload response = %+v", body)
	}
}
