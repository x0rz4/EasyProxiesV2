package httpapi

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
	"easy_proxies/internal/controlplane"
)

type reloadNodeManager struct{ reloads atomic.Int32 }

func (m *reloadNodeManager) ListConfigNodes(context.Context, *int64) ([]controlplane.ManagedNodeConfig, error) {
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

// idNodeManager records ID-addressed calls so the ID routes can be verified
// without a live runtime.
type idNodeManager struct {
	reloadNodeManager
	updated []int64
	toggled []int64
	deleted []int64
}

func (m *idNodeManager) UpdateNodeByID(_ context.Context, id int64, node config.NodeConfig) (config.NodeConfig, error) {
	m.updated = append(m.updated, id)
	node.ID = id
	return node, nil
}

func (m *idNodeManager) SetNodeEnabledByID(_ context.Context, id int64, _ bool) error {
	m.toggled = append(m.toggled, id)
	return nil
}

func (m *idNodeManager) DeleteNodeByID(_ context.Context, id int64) error {
	m.deleted = append(m.deleted, id)
	return nil
}

func TestNodeIDRoutesAddressByID(t *testing.T) {
	manager := &idNodeManager{}
	server := &Server{nodeMgr: manager, logger: log.Default()}

	update := httptest.NewRequest(http.MethodPut, "/api/nodes/config/id/42",
		strings.NewReader(`{"name":"renamed","uri":"socks5://127.0.0.1:1080"}`))
	updateResp := httptest.NewRecorder()
	server.routes().ServeHTTP(updateResp, update)
	assertReloadResponse(t, updateResp)
	if len(manager.updated) != 1 || manager.updated[0] != 42 {
		t.Fatalf("updated=%v", manager.updated)
	}

	toggle := httptest.NewRequest(http.MethodPatch, "/api/nodes/config/id/42", strings.NewReader(`{"enabled":false}`))
	toggleResp := httptest.NewRecorder()
	server.routes().ServeHTTP(toggleResp, toggle)
	if toggleResp.Code != http.StatusOK || len(manager.toggled) != 1 || manager.toggled[0] != 42 {
		t.Fatalf("toggle status=%d toggled=%v", toggleResp.Code, manager.toggled)
	}

	remove := httptest.NewRequest(http.MethodDelete, "/api/nodes/config/id/42", nil)
	removeResp := httptest.NewRecorder()
	server.routes().ServeHTTP(removeResp, remove)
	assertReloadResponse(t, removeResp)
	if len(manager.deleted) != 1 || manager.deleted[0] != 42 {
		t.Fatalf("deleted=%v", manager.deleted)
	}

	// A malformed ID is a 400, not a name lookup.
	bad := httptest.NewRequest(http.MethodDelete, "/api/nodes/config/id/abc", nil)
	badResp := httptest.NewRecorder()
	server.routes().ServeHTTP(badResp, bad)
	if badResp.Code != http.StatusBadRequest {
		t.Fatalf("malformed id status=%d", badResp.Code)
	}
}

func TestBatchToggleAcceptsIDs(t *testing.T) {
	manager := &idNodeManager{}
	server := &Server{nodeMgr: manager, logger: log.Default()}
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/config/batch-toggle",
		strings.NewReader(`{"ids":[3,4],"enabled":false}`))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(manager.toggled) != 2 || manager.toggled[0] != 3 || manager.toggled[1] != 4 {
		t.Fatalf("toggled=%v", manager.toggled)
	}
}
