package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupTimelineDropsExpiredAndCapsPerNode covers both pruning rules of
// the time-window cleanup: rows older than the retention window go regardless
// of rank, and nodes that burst inside the window are trimmed to keepPerNode.
func TestCleanupTimelineDropsExpiredAndCapsPerNode(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "timeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sqlDB := db.(*sqliteStore)

	now := time.Now().UTC().Format(time.RFC3339)
	stale := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)

	// node_timeline rows carry a foreign key into nodes, so the fixture nodes
	// must exist first.
	for _, name := range []string{"n1", "n2"} {
		node := &Node{URI: "http://" + name + ".example.test:80", Name: name, Source: NodeSourceManual, Enabled: true}
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(nodeID int64, createdAt string) {
		t.Helper()
		if _, err := sqlDB.writerDB.Exec(
			`INSERT INTO node_timeline (node_id, success, latency_ms, error, created_at) VALUES (?, 1, 0, '', ?)`,
			nodeID, createdAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Node 1: 25 fresh rows (above the cap) + 5 stale rows.
	for i := 0; i < 25; i++ {
		insert(1, now)
	}
	for i := 0; i < 5; i++ {
		insert(1, stale)
	}
	// Node 2: 5 fresh rows (under the cap) + 3 stale rows.
	for i := 0; i < 5; i++ {
		insert(2, now)
	}
	for i := 0; i < 3; i++ {
		insert(2, stale)
	}

	if err := db.CleanupTimeline(ctx, 20); err != nil {
		t.Fatal(err)
	}

	count := func(nodeID int64) int {
		t.Helper()
		var n int
		if err := sqlDB.writerDB.QueryRow(
			`SELECT COUNT(*) FROM node_timeline WHERE node_id = ?`, nodeID,
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Node 1 keeps exactly the per-node cap; all stale rows are gone.
	if got := count(1); got != 20 {
		t.Fatalf("node 1 kept %d rows, want 20 (cap)", got)
	}
	// Node 2 keeps only its fresh rows; the stale ones expired.
	if got := count(2); got != 5 {
		t.Fatalf("node 2 kept %d rows, want 5 (fresh only)", got)
	}

	// Newest rows must be the survivors: the cap trims from the oldest side.
	events, err := db.GetTimeline(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 20 {
		t.Fatalf("GetTimeline returned %d events, want 20", len(events))
	}
	for _, event := range events {
		if event.CreatedAt.Before(time.Now().UTC().Add(-timelineRetentionWindow)) {
			t.Fatalf("expired row survived: %+v", event)
		}
	}
}
