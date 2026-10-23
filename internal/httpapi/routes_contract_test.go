package httpapi

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

// The route table is a contract, not an implementation detail. Every HTTP
// surface the panel exposes is enumerated here once, with the access class that
// guards it. A handler accidentally re-bound to the wrong pattern, an action
// dropped while splitting a switch-dispatching handler, or a route silently
// moved from session-authenticated to public all fail this file.
//
// Access classes map to the wrappers in routes(): "public" is registered bare,
// "session" runs behind withAuth, "apikey" behind withAPIKey, and
// "subscription"/"static" live on their own child muxes outside /api/.
const (
	classPublic       = "public"
	classSession      = "session"
	classAPIKey       = "apikey"
	classSubscription = "subscription"
	classStatic       = "static"
)

type routeContract struct {
	class    string
	method   string
	pattern  string // the ServeMux pattern as registered
	concrete string // the same pattern with every {wildcard} filled in
}

// routeContracts enumerates every registered route. Order mirrors routes() so a
// diff against that file stays readable.
var routeContracts = []routeContract{
	{classPublic, "GET", "/api/auth", "/api/auth"},
	{classPublic, "POST", "/api/auth", "/api/auth"},
	{classSession, "GET", "/api/settings", "/api/settings"},
	{classSession, "PUT", "/api/settings", "/api/settings"},
	{classSession, "GET", "/api/operations/probe-settings", "/api/operations/probe-settings"},
	{classSession, "PUT", "/api/operations/probe-settings", "/api/operations/probe-settings"},
	{classSession, "GET", "/api/operations/probe-status", "/api/operations/probe-status"},
	{classSession, "GET", "/api/operations/health-events", "/api/operations/health-events"},
	{classSession, "GET", "/api/operations/node-check-settings", "/api/operations/node-check-settings"},
	{classSession, "PUT", "/api/operations/node-check-settings", "/api/operations/node-check-settings"},
	{classSession, "GET", "/api/node-check/results", "/api/node-check/results"},
	{classSession, "GET", "/api/node-check/tasks", "/api/node-check/tasks"},
	{classSession, "POST", "/api/node-check/tasks", "/api/node-check/tasks"},
	{classSession, "GET", "/api/node-check/tasks/{taskID}", "/api/node-check/tasks/task-1"},
	{classSession, "DELETE", "/api/node-check/tasks/{taskID}", "/api/node-check/tasks/task-1"},
	{classSession, "GET", "/api/node-check/tasks/{taskID}/events", "/api/node-check/tasks/task-1/events"},
	{classSession, "HEAD", "/api/node-check/tasks/{taskID}/events", "/api/node-check/tasks/task-1/events"},
	{classSession, "GET", "/api/nodes", "/api/nodes"},
	{classSession, "POST", "/api/nodes/query", "/api/nodes/query"},
	{classSession, "POST", "/api/nodes/query-ids", "/api/nodes/query-ids"},
	{classSession, "GET", "/api/nodes/facets", "/api/nodes/facets"},
	{classSession, "POST", "/api/groups/preview", "/api/groups/preview"},
	{classSession, "POST", "/api/groups/{groupID}/preview", "/api/groups/1/preview"},
	{classSession, "GET", "/api/node-filters", "/api/node-filters"},
	{classSession, "POST", "/api/node-filters", "/api/node-filters"},
	{classSession, "PUT", "/api/node-filters/{filterID}", "/api/node-filters/5"},
	{classSession, "DELETE", "/api/node-filters/{filterID}", "/api/node-filters/5"},
	{classSession, "GET", "/api/nodes/config", "/api/nodes/config"},
	{classSession, "POST", "/api/nodes/config", "/api/nodes/config"},
	{classSession, "POST", "/api/nodes/config/batch", "/api/nodes/config/batch"},
	{classSession, "POST", "/api/nodes/config/batch-toggle", "/api/nodes/config/batch-toggle"},
	{classSession, "POST", "/api/nodes/config/batch-delete", "/api/nodes/config/batch-delete"},
	{classSession, "PUT", "/api/nodes/config/id/{id}", "/api/nodes/config/id/7"},
	{classSession, "PATCH", "/api/nodes/config/id/{id}", "/api/nodes/config/id/7"},
	{classSession, "DELETE", "/api/nodes/config/id/{id}", "/api/nodes/config/id/7"},
	{classSession, "PUT", "/api/nodes/config/{name}", "/api/nodes/config/node-a"},
	{classSession, "PATCH", "/api/nodes/config/{name}", "/api/nodes/config/node-a"},
	{classSession, "DELETE", "/api/nodes/config/{name}", "/api/nodes/config/node-a"},
	{classSession, "GET", "/api/nodes/maintenance", "/api/nodes/maintenance"},
	{classSession, "POST", "/api/nodes/probe-all", "/api/nodes/probe-all"},
	{classSession, "POST", "/api/nodes/unlock-all", "/api/nodes/unlock-all"},
	{classSession, "GET", "/api/nodes/unlock-meta", "/api/nodes/unlock-meta"},
	{classSession, "GET", "/api/nodes/unlock-results", "/api/nodes/unlock-results"},
	{classSession, "GET", "/api/nodes/traffic/stream", "/api/nodes/traffic/stream"},
	{classSession, "HEAD", "/api/nodes/traffic/stream", "/api/nodes/traffic/stream"},
	{classSession, "POST", "/api/nodes/{tag}/probe", "/api/nodes/example/probe"},
	{classSession, "POST", "/api/nodes/{tag}/release", "/api/nodes/example/release"},
	{classSession, "GET", "/api/nodes/{tag}/speedtest", "/api/nodes/example/speedtest"},
	{classSession, "HEAD", "/api/nodes/{tag}/speedtest", "/api/nodes/example/speedtest"},
	{classSession, "POST", "/api/nodes/{tag}/unlock", "/api/nodes/example/unlock"},
	{classSession, "GET", "/api/debug", "/api/debug"},
	{classSession, "GET", "/api/debug/stream", "/api/debug/stream"},
	{classSession, "HEAD", "/api/debug/stream", "/api/debug/stream"},
	{classSession, "GET", "/api/export", "/api/export"},
	{classSession, "POST", "/api/import", "/api/import"},
	{classSession, "GET", "/api/subscription/status", "/api/subscription/status"},
	{classSession, "POST", "/api/subscription/refresh", "/api/subscription/refresh"},
	{classSession, "GET", "/api/subscription/refresh-settings", "/api/subscription/refresh-settings"},
	{classSession, "PUT", "/api/subscription/refresh-settings", "/api/subscription/refresh-settings"},
	{classSession, "GET", "/api/subscriptions", "/api/subscriptions"},
	{classSession, "POST", "/api/subscriptions", "/api/subscriptions"},
	{classSession, "GET", "/api/subscriptions/{subscriptionID}", "/api/subscriptions/1"},
	{classSession, "PUT", "/api/subscriptions/{subscriptionID}", "/api/subscriptions/1"},
	{classSession, "DELETE", "/api/subscriptions/{subscriptionID}", "/api/subscriptions/1"},
	{classSession, "PATCH", "/api/subscriptions/{subscriptionID}/enabled", "/api/subscriptions/1/enabled"},
	{classSession, "POST", "/api/subscriptions/{subscriptionID}/activate", "/api/subscriptions/1/activate"},
	{classSession, "POST", "/api/subscriptions/{subscriptionID}/refresh", "/api/subscriptions/1/refresh"},
	{classSession, "GET", "/api/subscriptions/{subscriptionID}/nodes", "/api/subscriptions/1/nodes"},
	{classSession, "POST", "/api/reload", "/api/reload"},
	{classSession, "GET", "/api/api-keys", "/api/api-keys"},
	{classSession, "POST", "/api/api-keys", "/api/api-keys"},
	{classSession, "DELETE", "/api/api-keys/{apiKeyID}", "/api/api-keys/6"},
	{classAPIKey, "POST", "/api/public/subscriptions", "/api/public/subscriptions"},
	{classAPIKey, "POST", "/api/public/import", "/api/public/import"},
	{classSession, "GET", "/api/groups", "/api/groups"},
	{classSession, "GET", "/api/groups/{groupID}/healing-events", "/api/groups/1/healing-events"},
	{classSession, "POST", "/api/groups", "/api/groups"},
	{classSession, "PUT", "/api/groups/{groupID}", "/api/groups/1"},
	{classSession, "DELETE", "/api/groups/{groupID}", "/api/groups/1"},
	{classSession, "POST", "/api/groups/{groupID}/members/{nodeID}/activate", "/api/groups/1/members/2/activate"},
	{classSession, "POST", "/api/groups/{groupID}/members/{nodeID}/restore", "/api/groups/1/members/2/restore"},
	{classSession, "DELETE", "/api/groups/{groupID}/members/{nodeID}", "/api/groups/1/members/2"},
	{classSession, "DELETE", "/api/groups/{groupID}/exclusions/{nodeID}", "/api/groups/1/exclusions/2"},
	{classSession, "POST", "/api/groups/{groupID}/subscription/reset-token", "/api/groups/1/subscription/reset-token"},
	{classSession, "GET", "/api/tags", "/api/tags"},
	{classSession, "POST", "/api/tags", "/api/tags"},
	{classSession, "GET", "/api/tags/schema", "/api/tags/schema"},
	{classSession, "POST", "/api/tags/preview", "/api/tags/preview"},
	{classSession, "POST", "/api/tags/recompute", "/api/tags/recompute"},
	{classSession, "POST", "/api/tags/templates", "/api/tags/templates"},
	{classSession, "GET", "/api/tags/assignments", "/api/tags/assignments"},
	{classSession, "POST", "/api/tags/nodes/batch", "/api/tags/nodes/batch"},
	{classSession, "PUT", "/api/tags/nodes/{nodeID}", "/api/tags/nodes/2"},
	{classSession, "GET", "/api/tags/mutex-groups", "/api/tags/mutex-groups"},
	{classSession, "POST", "/api/tags/mutex-groups", "/api/tags/mutex-groups"},
	{classSession, "GET", "/api/tags/mutex-groups/{mutexGroupID}", "/api/tags/mutex-groups/3"},
	{classSession, "PUT", "/api/tags/mutex-groups/{mutexGroupID}", "/api/tags/mutex-groups/3"},
	{classSession, "DELETE", "/api/tags/mutex-groups/{mutexGroupID}", "/api/tags/mutex-groups/3"},
	{classSession, "GET", "/api/tags/{tagID}", "/api/tags/4"},
	{classSession, "PUT", "/api/tags/{tagID}", "/api/tags/4"},
	{classSession, "DELETE", "/api/tags/{tagID}", "/api/tags/4"},
	{classSession, "PATCH", "/api/tags/{tagID}/auto", "/api/tags/4/auto"},
	{classSession, "GET", "/api/geoip/status", "/api/geoip/status"},
	{classSession, "POST", "/api/geoip/download", "/api/geoip/download"},
	{classSession, "POST", "/api/geoip/update", "/api/geoip/update"},
	{classSubscription, "GET", "/sub/{groupID}", "/sub/1"},
	{classSubscription, "GET", "/sub/{groupID}/entry", "/sub/1/entry"},
	{classStatic, "GET", "/", "/"},
}

// sessionRejection is the body withAuth writes. It is the fingerprint that
// proves a route really sits behind the session middleware rather than merely
// happening to answer 401 for its own reasons.
const sessionRejection = "未授权，请先登录"

// apiKeyRejection is the fingerprint of withAPIKey.
const apiKeyRejection = "缺少 API Key"

// contractServer wires the minimum a request needs to reach the router: a real
// store so the API-key middleware gets past its "store unavailable" guard, and
// a configured password so the session middleware is actually armed.
func contractServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "routes-contract.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Server{
		cfg:    monitor.Config{Password: "secret"},
		store:  db,
		logger: log.New(io.Discard, "", 0),
	}
}

// newContractSession issues a session the way a successful login would, so the
// tests below can reach handlers sitting behind withAuth.
func newContractSession(t *testing.T, server *Server) (*controlplane.Session, string) {
	t.Helper()
	session, err := server.authService().CreateSession(context.Background())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session, session.Token
}

// TestRouteTableIsFullyRegistered pins the registered pattern for every route.
// A dropped action turns a wildcard match into a 404 (or into a neighbouring
// pattern) and fails here rather than in production.
func TestRouteTableIsFullyRegistered(t *testing.T) {
	server := contractServer(t)
	handler := server.routes()

	for _, route := range routeContracts {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.concrete, nil)
			handler.ServeHTTP(httptest.NewRecorder(), request)
			if request.Pattern != route.method+" "+route.pattern {
				t.Fatalf("matched pattern = %q, want %q", request.Pattern, route.method+" "+route.pattern)
			}
		})
	}
}

// TestRouteTableAccessClass fixes which middleware wraps each route. Requests
// carry no credential at all, so the guard answers before the handler runs and
// the response body names the guard that stopped it.
func TestRouteTableAccessClass(t *testing.T) {
	server := contractServer(t)
	handler := server.routes()

	for _, route := range routeContracts {
		t.Run(route.class+" "+route.method+" "+route.pattern, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.concrete, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			body := response.Body.String()

			switch route.class {
			case classSession:
				if response.Code != http.StatusUnauthorized || !strings.Contains(body, sessionRejection) {
					t.Fatalf("status=%d body=%q, want the session guard", response.Code, body)
				}
			case classAPIKey:
				if response.Code != http.StatusUnauthorized || !strings.Contains(body, apiKeyRejection) {
					t.Fatalf("status=%d body=%q, want the API-key guard", response.Code, body)
				}
			default:
				// Public surfaces must not be reachable through either guard.
				if strings.Contains(body, sessionRejection) || strings.Contains(body, apiKeyRejection) {
					t.Fatalf("status=%d body=%q, want no guard", response.Code, body)
				}
			}
		})
	}
}

// TestSSERoutesAnswerHEAD pins the Content-Type contract the browser
// EventSource handshake depends on. These HEAD routes are registered
// separately from their GET counterparts, so losing one is a silent freeze in
// the UI rather than an error.
func TestSSERoutesAnswerHEAD(t *testing.T) {
	server := contractServer(t)
	_, session := newContractSession(t, server)
	handler := server.routes()

	for _, path := range []string{
		"/api/debug/stream",
		"/api/nodes/traffic/stream",
		"/api/nodes/example/speedtest",
		"/api/node-check/tasks/task-1/events",
	} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodHead, path, nil)
			request.Header.Set("Authorization", "Bearer "+session)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
			}
		})
	}
}

// TestUnknownRoutesAreNotFound guards the split between the API mux and the SPA
// fallback: an unrecognised endpoint must stay a real 404 instead of quietly
// returning index.html.
func TestUnknownRoutesAreNotFound(t *testing.T) {
	server := contractServer(t)
	handler := server.routes()

	for _, path := range []string{
		"/api/not-registered",
		"/api/nodes/extra/segment",
		"/sub/not-registered/extra",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, response.Code)
		}
	}
}

// TestWrongMethodIsRejectedByTheMux pins that method multiplexing stays with
// ServeMux. The handlers used to answer 405 from a default switch branch, which
// meant the Allow header depended on hand-written code.
func TestWrongMethodIsRejectedByTheMux(t *testing.T) {
	server := contractServer(t)
	_, session := newContractSession(t, server)
	handler := server.routes()

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/tags/schema"},
		{http.MethodDelete, "/api/settings"},
		{http.MethodPut, "/api/groups"},
		{http.MethodGet, "/api/nodes/probe-all"},
		{http.MethodPost, "/api/subscriptions/1"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set("Authorization", "Bearer "+session)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405; body=%s", response.Code, response.Body.String())
			}
			if allow := response.Header().Get("Allow"); allow == "" {
				t.Fatal("405 response carries no Allow header")
			}
		})
	}
}
