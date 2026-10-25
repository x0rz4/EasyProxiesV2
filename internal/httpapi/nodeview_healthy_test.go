package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

func TestNodeViewHealthyScopeUsesCurrentRuntimeHealth(t *testing.T) {
	s, mgr, _ := newOperationsTestServer(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "healthy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s.SetStore(db)
	var healthyID int64
	var healthyHandle *monitor.EntryHandle
	for _, name := range []string{"healthy", "pending", "unavailable", "blacklisted", "disabled", "unloaded"} {
		node := createGroupTestNode(t, t.Context(), db, name, store.NodeSourceManual, name != "disabled")
		if name == "unloaded" {
			continue
		}
		handle := mgr.Register(monitor.NodeInfo{NodeID: node.ID, Tag: name, Name: name, URI: node.URI})
		switch name {
		case "pending":
		case "unavailable":
			handle.MarkInitialCheckDone(false)
		default:
			handle.MarkInitialCheckDone(true)
		}
		if name == "blacklisted" {
			handle.Blacklist(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
		}
		if name == "healthy" {
			healthyID, healthyHandle = node.ID, handle
		}
	}
	handler := s.routes()
	assertHealthy := func(want int) {
		t.Helper()
		for _, path := range []string{"/api/nodes/query", "/api/nodes/query-ids"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"spec":{"version":2,"healthy_only":true},"page":{"limit":50}}`)))
			if response.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
			}
			if path == "/api/nodes/query" {
				var result nodeview.Result
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Total != want || len(result.Items) != want {
					t.Fatalf("page=%+v err=%v", result, err)
				}
				if want > 0 && result.Items[0].ID != healthyID {
					t.Fatal("non-healthy node returned")
				}
			} else {
				var result struct {
					IDs []int64 `json:"ids"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.IDs) != want {
					t.Fatalf("targets=%v err=%v", result.IDs, err)
				}
				if want > 0 && result.IDs[0] != healthyID {
					t.Fatal("non-healthy target returned")
				}
			}
		}
		for _, suffix := range []string{"?healthy_only=true", "?healthy_only=true&runtime_only=true"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/nodes/facets"+suffix, nil))
			var facets nodeview.Facets
			if err := json.Unmarshal(response.Body.Bytes(), &facets); err != nil || response.Code != 200 || facets.Total != want {
				t.Fatalf("facets=%+v status=%d err=%v", facets, response.Code, err)
			}
		}
	}
	assertHealthy(1)
	healthyHandle.MarkInitialCheckDone(false)
	assertHealthy(0)
	for suffix, want := range map[string]int{"": 6, "?runtime_only=true": 5} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/nodes/facets"+suffix, nil))
		var facets nodeview.Facets
		if err := json.Unmarshal(response.Body.Bytes(), &facets); err != nil || facets.Total != want {
			t.Fatalf("existing scope %q changed: %+v %v", suffix, facets, err)
		}
	}
}
