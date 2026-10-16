package monitor

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

func TestNodeMaintenanceClassifiesStatusAndOfflineDays(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	create := func(name, uri string, enabled bool) *store.Node {
		t.Helper()
		node := &store.Node{URI: uri, Name: name, Source: store.NodeSourceManual, Enabled: enabled}
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
		return node
	}

	online := create("online-node", "http://127.0.0.1:10001", true)
	stale := create("stale-node", "http://127.0.0.1:10002", true)
	freshFailure := create("fresh-failure", "http://127.0.0.1:10003", true)
	never := create("never-worked", "http://127.0.0.1:10004", true)
	pending := create("pending-node", "http://127.0.0.1:10005", true)
	disabled := create("disabled-node", "http://127.0.0.1:10006", false)

	if err := db.BatchUpdateStats(ctx, []store.StatsUpdate{
		{NodeID: online.ID, Available: true, InitialCheckDone: true, SuccessCount: 5, LastSuccessAt: now},
		{NodeID: stale.ID, Available: false, InitialCheckDone: true, FailureCount: 12,
			LastSuccessAt: now.Add(-10 * 24 * time.Hour), LastFailureAt: now.Add(-time.Hour), LastError: "dial tcp: i/o timeout"},
		{NodeID: freshFailure.ID, Available: false, InitialCheckDone: true, FailureCount: 2,
			LastSuccessAt: now.Add(-2 * time.Hour), LastFailureAt: now.Add(-time.Minute)},
		{NodeID: never.ID, Available: false, InitialCheckDone: true, FailureCount: 40, LastFailureAt: now},
		{NodeID: pending.ID, InitialCheckDone: false},
		// A disabled node keeps its frozen last-known state; it must never be
		// reported as a live outage.
		{NodeID: disabled.ID, Available: false, InitialCheckDone: true, LastSuccessAt: now.Add(-90 * 24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	server := &Server{store: db, logger: log.New(io.Discard, "", 0)}
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/nodes/maintenance", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Nodes   []NodeMaintenanceEntry `json:"nodes"`
		Summary map[string]int         `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Nodes) != 6 {
		t.Fatalf("listed %d nodes, want 6", len(payload.Nodes))
	}

	byName := make(map[string]NodeMaintenanceEntry, len(payload.Nodes))
	for _, entry := range payload.Nodes {
		byName[entry.Name] = entry
	}

	if got := byName["online-node"]; got.Status != maintenanceOnline || got.OfflineDays != 0 {
		t.Fatalf("online node=%+v", got)
	}
	// 10 days offline, and the streak starts at the last successful check.
	if got := byName["stale-node"]; got.Status != maintenanceOffline || got.OfflineDays < 9.9 || got.OfflineDays > 10.1 {
		t.Fatalf("stale node=%+v", got)
	} else if got.FailureCount != 12 || got.LastError != "dial tcp: i/o timeout" || got.SuccessCount != 0 {
		t.Fatalf("stale node lost failure detail: %+v", got)
	}
	if got := byName["fresh-failure"]; got.Status != maintenanceOffline || got.OfflineDays < 0.08 || got.OfflineDays > 0.09 {
		t.Fatalf("two-hour outage=%+v", got)
	}
	// Never succeeded: the streak falls back to the creation time, so a fresh
	// import reports roughly zero days rather than a bogus number.
	if got := byName["never-worked"]; got.Status != maintenanceOffline || got.OfflineDays > 0.01 {
		t.Fatalf("never-succeeded node=%+v", got)
	}
	if got := byName["pending-node"]; got.Status != maintenancePending || got.OfflineDays != 0 {
		t.Fatalf("pending node=%+v", got)
	}
	if got := byName["disabled-node"]; got.Status != maintenanceDisabled || got.OfflineDays != 0 {
		t.Fatalf("disabled node must not report an outage: %+v", got)
	}

	want := map[string]int{"total": 6, "online": 1, "offline": 3, "pending": 1, "disabled": 1}
	for key, value := range want {
		if payload.Summary[key] != value {
			t.Fatalf("summary=%v, want %v", payload.Summary, want)
		}
	}

	// Longest outage first, so the page leads with what most needs removing.
	if payload.Nodes[0].Name != "stale-node" {
		t.Fatalf("first row=%s, want stale-node", payload.Nodes[0].Name)
	}
	if payload.Nodes[1].Name != "fresh-failure" {
		t.Fatalf("second row=%s, want fresh-failure", payload.Nodes[1].Name)
	}
}

func TestBuildNodeMaintenanceEntryOfflineSinceFallback(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-45 * 24 * time.Hour)

	node := store.ManagedNode{Node: store.Node{ID: 7, Name: "dead", Enabled: true, CreatedAt: created}}
	entry := buildNodeMaintenanceEntry(node, &store.NodeStats{InitialCheckDone: true, Available: false}, now)
	if entry.Status != maintenanceOffline {
		t.Fatalf("status=%s, want offline", entry.Status)
	}
	if !entry.OfflineSince.Equal(created) {
		t.Fatalf("offline since=%v, want the creation time %v", entry.OfflineSince, created)
	}
	if entry.OfflineDays < 44.9 || entry.OfflineDays > 45.1 {
		t.Fatalf("offline days=%v, want ~45", entry.OfflineDays)
	}

	// A successful node carries no outage duration even if it succeeded long ago.
	healthy := buildNodeMaintenanceEntry(node, &store.NodeStats{InitialCheckDone: true, Available: true, LastSuccessAt: created}, now)
	if healthy.Status != maintenanceOnline || healthy.OfflineDays != 0 || !healthy.OfflineSince.IsZero() {
		t.Fatalf("online node=%+v", healthy)
	}

	// No stats row at all: mirror the disabled flag, never claim an outage.
	if got := buildNodeMaintenanceEntry(node, nil, now); got.Status != maintenancePending {
		t.Fatalf("statless node=%+v", got)
	}
	node.Enabled = false
	if got := buildNodeMaintenanceEntry(node, nil, now); got.Status != maintenanceDisabled {
		t.Fatalf("statless disabled node=%+v", got)
	}
}
