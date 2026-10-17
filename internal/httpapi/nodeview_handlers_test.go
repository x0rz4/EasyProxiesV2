package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

func TestNodeQueryAndDraftPreviewShareRegionRules(t *testing.T) {
	s, _, _ := newOperationsTestServer(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "view.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s.SetStore(db)
	for _, node := range []store.Node{{URI: "http://alice:very-secret@127.0.0.1:8080", Name: "known", Region: "sg", Country: "Singapore", Enabled: true, Source: store.NodeSourceManual},
		{URI: "http://127.0.0.2:8080", Name: "pending", Enabled: true, Source: store.NodeSourceManual}} {
		if err := db.CreateNode(context.Background(), &node); err != nil {
			t.Fatal(err)
		}
	}
	query := httptest.NewRecorder()
	s.routes().ServeHTTP(query, httptest.NewRequest(http.MethodPost, "/api/nodes/query", strings.NewReader(`{"spec":{"version":2,"regions":["other"]},"page":{"limit":10}}`)))
	if query.Code != 200 {
		t.Fatalf("query: %d %s", query.Code, query.Body.String())
	}
	if strings.Contains(query.Body.String(), "very-secret") || strings.Contains(query.Body.String(), "http://") {
		t.Fatal("query exposed URI credentials")
	}
	var result nodeview.Result
	if err := json.Unmarshal(query.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Items[0].Name != "known" {
		t.Fatalf("query result: %+v", result)
	}
	preview := httptest.NewRecorder()
	s.routes().ServeHTTP(preview, httptest.NewRequest(http.MethodPost, "/api/groups/preview", strings.NewReader(`{"regions":["other"],"region_rule_version":2}`)))
	if preview.Code != 200 {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	var response struct {
		MatchedIDs []int64 `json:"matched_ids"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.MatchedIDs) != 1 || response.MatchedIDs[0] != result.Items[0].ID {
		t.Fatalf("preview/query disagree: %+v %+v", response, result)
	}
	if strings.Contains(preview.Body.String(), "very-secret") {
		t.Fatal("preview exposed credentials")
	}
	bulk := httptest.NewRecorder()
	s.routes().ServeHTTP(bulk, httptest.NewRequest(http.MethodPost, "/api/nodes/query-ids", strings.NewReader(`{"spec":{"version":2,"regions":["other"]}}`)))
	var targets struct {
		IDs      []int64 `json:"ids"`
		Revision string  `json:"snapshot_revision"`
	}
	if err := json.Unmarshal(bulk.Body.Bytes(), &targets); err != nil || bulk.Code != 200 || len(targets.IDs) != 1 || targets.IDs[0] != result.Items[0].ID || targets.Revision == "" {
		t.Fatalf("bulk query disagrees: %d %s %v", bulk.Code, bulk.Body.String(), err)
	}
	if strings.Contains(bulk.Body.String(), "very-secret") {
		t.Fatal("bulk query exposed credentials")
	}
	bad := httptest.NewRecorder()
	s.routes().ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/nodes/query", strings.NewReader(`{"spec":{},"page":{"limit":501}}`)))
	if bad.Code != 400 {
		t.Fatalf("unbounded page accepted: %d", bad.Code)
	}
}
