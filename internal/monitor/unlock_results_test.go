package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/store"
)

// TestHandleUnlockResultsKeysByNodeIDAcrossTagGenerations is the regression test
// for nodes rendering as 未检测 after a reload. Runtime tags carry a generation
// suffix ("<digest>@v<n>") and every rebuilt node gets a fresh one, so a
// tag-keyed response stranded stored results behind tags the WebUI had already
// replaced — an incremental reload re-tags only the changed nodes, which is why
// only part of the list went blank.
func TestHandleUnlockResultsKeysByNodeIDAcrossTagGenerations(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "unlock-results.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	node := &store.Node{URI: "ss://node@127.0.0.1:1080", Name: "tokyo", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	// Recorded while the node was running as generation 1.
	stored := &store.UnlockResult{
		NodeID:    node.ID,
		Tag:       "0123456789abcdef@v1",
		Name:      "tokyo",
		Services:  []store.UnlockServiceResult{{Name: "netflix", Status: "unlocked"}},
		IP:        store.UnlockIPInfo{IP: "203.0.113.7", ISOCode: "JP"},
		Duration:  1234,
		CheckedAt: time.Now().UTC(),
	}
	if err := db.UpsertUnlockResult(ctx, stored); err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db}
	request := httptest.NewRequest(http.MethodGet, "/api/nodes/unlock-results", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	var payload struct {
		Results map[string]struct {
			NodeID   int64  `json:"node_id"`
			Tag      string `json:"tag"`
			Name     string `json:"name"`
			Services []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"services"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}

	// The node is now running as generation 2; the WebUI only knows that tag.
	if _, strandedByTag := payload.Results["0123456789abcdef@v2"]; strandedByTag {
		t.Fatal("results must not be keyed by runtime tag")
	}
	key := strconv.FormatInt(node.ID, 10)
	view, ok := payload.Results[key]
	if !ok {
		t.Fatalf("result for node %d missing, got keys %v", node.ID, payload.Results)
	}
	if view.NodeID != node.ID {
		t.Fatalf("node_id=%d, want %d", view.NodeID, node.ID)
	}
	if view.Name != "tokyo" {
		t.Fatalf("result payload lost detail: %+v", view)
	}
	netflix := ""
	for _, service := range view.Services {
		if service.Name == "netflix" {
			netflix = service.Status
		}
	}
	if netflix != "unlocked" {
		t.Fatalf("netflix status=%q, want unlocked: %+v", netflix, view)
	}
	if view.Tag != "0123456789abcdef@v1" {
		t.Fatalf("tag=%q, want the tag recorded at check time", view.Tag)
	}
}
