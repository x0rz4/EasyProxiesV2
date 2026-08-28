package monitor

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"easy_proxies/internal/config"
)

func TestServeMuxMethodAndCanonicalPathSemantics(t *testing.T) {
	server, _ := newTagAPITestServer(t)
	handler := server.routes()

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/tags/schema", nil))
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200; body=%s", head.Code, head.Body.String())
	}
	for _, path := range []string{
		"/api/debug/stream",
		"/api/nodes/traffic/stream",
		"/api/nodes/example/speedtest",
		"/api/node-check/tasks/task-1/events",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodHead, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("HEAD %s status=%d content-type=%q", path, response.Code, response.Header().Get("Content-Type"))
		}
	}

	wrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPost, "/api/tags/schema", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong-method status = %d, want 405", wrongMethod.Code)
	}
	if allow := wrongMethod.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodHead) {
		t.Fatalf("Allow = %q, want GET and HEAD", allow)
	}

	redirect := httptest.NewRecorder()
	handler.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/api//tags/./schema", nil))
	if redirect.Code != http.StatusMovedPermanently || redirect.Header().Get("Location") != "/api/tags/schema" {
		t.Fatalf("canonical redirect status=%d location=%q", redirect.Code, redirect.Header().Get("Location"))
	}

	for _, path := range []string{"/api/not-registered", "/sub/not-registered/extra"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
	}
}

func TestServeMuxDynamicRoutesAndLiteralPrecedence(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	server := &Server{mgr: mgr, logger: log.New(io.Discard, "", 0)}
	handler := server.routes()

	tests := []struct {
		method  string
		path    string
		pattern string
	}{
		{http.MethodGet, "/api/node-check/tasks/task-1", "GET /api/node-check/tasks/{taskID}"},
		{http.MethodDelete, "/api/node-check/tasks/task-1", "DELETE /api/node-check/tasks/{taskID}"},
		{http.MethodGet, "/api/node-check/tasks/task-1/events", "GET /api/node-check/tasks/{taskID}/events"},
		{http.MethodPut, "/api/nodes/config/node-a", "PUT /api/nodes/config/{name}"},
		{http.MethodPatch, "/api/nodes/config/node-a", "PATCH /api/nodes/config/{name}"},
		{http.MethodDelete, "/api/nodes/config/node-a", "DELETE /api/nodes/config/{name}"},
		{http.MethodPost, "/api/nodes/config/batch-toggle", "POST /api/nodes/config/batch-toggle"},
		{http.MethodPost, "/api/nodes/missing/probe", "POST /api/nodes/{tag}/probe"},
		{http.MethodPost, "/api/nodes/missing/release", "POST /api/nodes/{tag}/release"},
		{http.MethodGet, "/api/nodes/missing/speedtest", "GET /api/nodes/{tag}/speedtest"},
		{http.MethodPost, "/api/nodes/missing/unlock", "POST /api/nodes/{tag}/unlock"},
		{http.MethodGet, "/api/subscriptions/1", "GET /api/subscriptions/{subscriptionID}"},
		{http.MethodPut, "/api/subscriptions/1", "PUT /api/subscriptions/{subscriptionID}"},
		{http.MethodDelete, "/api/subscriptions/1", "DELETE /api/subscriptions/{subscriptionID}"},
		{http.MethodPatch, "/api/subscriptions/1/enabled", "PATCH /api/subscriptions/{subscriptionID}/enabled"},
		{http.MethodPost, "/api/subscriptions/1/activate", "POST /api/subscriptions/{subscriptionID}/activate"},
		{http.MethodPost, "/api/subscriptions/1/refresh", "POST /api/subscriptions/{subscriptionID}/refresh"},
		{http.MethodGet, "/api/subscriptions/1/nodes", "GET /api/subscriptions/{subscriptionID}/nodes"},
		{http.MethodPut, "/api/groups/1", "PUT /api/groups/{groupID}"},
		{http.MethodDelete, "/api/groups/1", "DELETE /api/groups/{groupID}"},
		{http.MethodPost, "/api/groups/1/members/2/activate", "POST /api/groups/{groupID}/members/{nodeID}/activate"},
		{http.MethodPost, "/api/groups/1/members/2/restore", "POST /api/groups/{groupID}/members/{nodeID}/restore"},
		{http.MethodDelete, "/api/groups/1/members/2", "DELETE /api/groups/{groupID}/members/{nodeID}"},
		{http.MethodDelete, "/api/groups/1/exclusions/2", "DELETE /api/groups/{groupID}/exclusions/{nodeID}"},
		{http.MethodPost, "/api/groups/1/subscription/reset-token", "POST /api/groups/{groupID}/subscription/reset-token"},
		{http.MethodGet, "/api/tags/schema", "GET /api/tags/schema"},
		{http.MethodPost, "/api/tags/nodes/batch", "POST /api/tags/nodes/batch"},
		{http.MethodPut, "/api/tags/nodes/2", "PUT /api/tags/nodes/{nodeID}"},
		{http.MethodGet, "/api/tags/mutex-groups/3", "GET /api/tags/mutex-groups/{mutexGroupID}"},
		{http.MethodPut, "/api/tags/mutex-groups/3", "PUT /api/tags/mutex-groups/{mutexGroupID}"},
		{http.MethodDelete, "/api/tags/mutex-groups/3", "DELETE /api/tags/mutex-groups/{mutexGroupID}"},
		{http.MethodGet, "/api/tags/4", "GET /api/tags/{tagID}"},
		{http.MethodPut, "/api/tags/4", "PUT /api/tags/{tagID}"},
		{http.MethodDelete, "/api/tags/4", "DELETE /api/tags/{tagID}"},
		{http.MethodPatch, "/api/tags/4/auto", "PATCH /api/tags/{tagID}/auto"},
		{http.MethodGet, "/sub/1", "GET /sub/{groupID}"},
		{http.MethodGet, "/sub/1/entry", "GET /sub/{groupID}/entry"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			handler.ServeHTTP(response, request)
			if request.Pattern != test.pattern {
				t.Fatalf("matched pattern = %q, want %q", request.Pattern, test.pattern)
			}
		})
	}

	extra := httptest.NewRecorder()
	handler.ServeHTTP(extra, httptest.NewRequest(http.MethodPost, "/api/groups/1/members/2/activate/extra", nil))
	if extra.Code != http.StatusNotFound {
		t.Fatalf("extra path status = %d, want 404", extra.Code)
	}
}

type pathValueNodeManager struct {
	mu          sync.Mutex
	updatedName string
}

func (*pathValueNodeManager) ListConfigNodes(context.Context, *int64) ([]ManagedNodeConfig, error) {
	return nil, nil
}
func (*pathValueNodeManager) CreateNode(_ context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	return node, nil
}
func (m *pathValueNodeManager) UpdateNode(_ context.Context, name string, node config.NodeConfig) (config.NodeConfig, error) {
	m.mu.Lock()
	m.updatedName = name
	m.mu.Unlock()
	return node, nil
}
func (*pathValueNodeManager) DeleteNode(context.Context, string) error           { return nil }
func (*pathValueNodeManager) SetNodeEnabled(context.Context, string, bool) error { return nil }
func (*pathValueNodeManager) TriggerReload(context.Context) error                { return nil }

func TestServeMuxPathValueUsesDecodedSegment(t *testing.T) {
	nodeManager := &pathValueNodeManager{}
	server := &Server{nodeMgr: nodeManager, logger: log.New(io.Discard, "", 0)}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/nodes/config/name%2Fwith%20space", strings.NewReader(`{"name":"updated","uri":"socks5://127.0.0.1:1080"}`))
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	nodeManager.mu.Lock()
	name := nodeManager.updatedName
	nodeManager.mu.Unlock()
	if name != "name/with space" {
		t.Fatalf("decoded PathValue = %q", name)
	}
}
