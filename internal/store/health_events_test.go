package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openHealthEventJournal(t *testing.T) Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "health-events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func appendHealthEvent(t *testing.T, db Store, nodeID int64, probeID string, success bool) int64 {
	t.Helper()
	seq, err := db.AppendHealthEvent(context.Background(), HealthEvent{
		NodeID: nodeID, IdentityToken: "id-token", IdentityRevision: 3, Tag: "node-a",
		ProbeID: probeID, Success: success, LatencyMs: 12, Error: "",
		CheckedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("append health event: %v", err)
	}
	return seq
}

func TestHealthEventJournalKeepsCommitOrder(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	first := appendHealthEvent(t, db, 7, "probe-1", true)
	second := appendHealthEvent(t, db, 7, "probe-2", false)
	if second <= first {
		t.Fatalf("sequence not monotonic: %d then %d", first, second)
	}
	events, err := db.ListHealthEventsAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != first || events[1].ProbeID != "probe-2" {
		t.Fatalf("unexpected journal: %+v", events)
	}
	if events[0].IdentityToken != "id-token" || events[0].IdentityRevision != 3 || events[0].Tag != "node-a" {
		t.Fatalf("identity evidence lost: %+v", events[0])
	}
	// Reading after a cursor never re-delivers the consumed prefix.
	rest, err := db.ListHealthEventsAfter(ctx, first, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Seq != second {
		t.Fatalf("unexpected tail: %+v", rest)
	}
}

func TestHealthEventJournalRejectsWhenFull(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	sqlDB := db.(*sqliteStore)
	// Seed the capacity in one transaction: the guard is what this test pins,
	// not the per-row insert cost of filling 8192 slots.
	if _, err := sqlDB.writerDB.ExecContext(ctx, `INSERT INTO health_events(node_id,tag,probe_id,success)
SELECT 7,'node-a','seed',1 FROM (WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?) SELECT i FROM n)`, HealthEventJournalLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendHealthEvent(ctx, HealthEvent{NodeID: 7, ProbeID: "overflow"}); !errors.Is(err, ErrHealthEventJournalFull) {
		t.Fatalf("expected journal full, got %v", err)
	}
	// Backpressure is visible but does not destroy the facts already queued.
	var count int64
	if err := sqlDB.readerDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM health_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != HealthEventJournalLimit {
		t.Fatalf("journal size changed under backpressure: %d", count)
	}
}

func TestHealthEventCursorPersistsForwardOnly(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	if seq, found, err := db.GetHealthEventCursor(ctx, "pool:1"); err != nil || found || seq != 0 {
		t.Fatalf("unexpected empty cursor: %d %v %v", seq, found, err)
	}
	if err := db.PutHealthEventCursor(ctx, "pool:1", 5); err != nil {
		t.Fatal(err)
	}
	// A retried write of an older batch must not rewind a resumed consumer.
	if err := db.PutHealthEventCursor(ctx, "pool:1", 3); err != nil {
		t.Fatal(err)
	}
	seq, found, err := db.GetHealthEventCursor(ctx, "pool:1")
	if err != nil || !found || seq != 5 {
		t.Fatalf("cursor = %d %v %v", seq, found, err)
	}
}

func TestPruneHealthEventsKeepsSlowestConsumerBacklog(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		if _, err := db.AppendHealthEvent(ctx, HealthEvent{NodeID: 7, ProbeID: "probe", RecordedAt: stale}); err != nil {
			t.Fatal(err)
		}
	}
	// Both consumers are stale by time, but the slowest one still owes 3..4.
	for consumer, seq := range map[string]int64{"fast": 4, "slow": 2} {
		if err := db.PutHealthEventCursor(ctx, consumer, seq); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := db.PruneHealthEvents(ctx, cutoff, []string{"fast", "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("pruned %d rows, want 2", removed)
	}
	remaining, err := db.ListHealthEventsAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Seq != 3 {
		t.Fatalf("slow consumer backlog was pruned: %+v", remaining)
	}
}

// A cursor row outlives its consumer (deleted group, disabled group, stopped
// box). Pruning must be bounded by the consumers that are registered now, not by
// every row in health_event_cursors: otherwise one inactive consumer pins the
// slowest position, the journal fills, and every other group's measurements are
// rejected.
func TestPruneHealthEventsIgnoresInactiveConsumerCursor(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	for i := 0; i < 10; i++ {
		if _, err := db.AppendHealthEvent(ctx, HealthEvent{NodeID: 7, ProbeID: "probe", RecordedAt: stale}); err != nil {
			t.Fatal(err)
		}
	}
	// The live consumer is caught up; the departed one stopped at 3.
	for consumer, seq := range map[string]int64{"pool:1": 10, "pool:9": 3} {
		if err := db.PutHealthEventCursor(ctx, consumer, seq); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := db.PruneHealthEvents(ctx, time.Now().UTC().Add(-time.Hour), []string{"pool:1"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 10 {
		t.Fatalf("pruned %d rows, want all 10: an inactive consumer pinned the journal", removed)
	}
	// The departed consumer's cursor row is untouched; it resumes from 3 and
	// counts what it missed as a gap when it comes back.
	if seq, found, err := db.GetHealthEventCursor(ctx, "pool:9"); err != nil || !found || seq != 3 {
		t.Fatalf("inactive cursor was altered: %d %v %v", seq, found, err)
	}
}

func TestPruneHealthEventsWithoutConsumersUsesAgeOnly(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := db.AppendHealthEvent(ctx, HealthEvent{NodeID: 7, ProbeID: "old", RecordedAt: old}); err != nil {
		t.Fatal(err)
	}
	appendHealthEvent(t, db, 7, "fresh", true)
	removed, err := db.PruneHealthEvents(ctx, time.Now().UTC().Add(-time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("pruned %d rows, want 1", removed)
	}
	stats, err := db.HealthEventJournalStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Count != 1 || stats.MaxSeq == 0 || time.Since(stats.OldestRecordedAt) > time.Minute {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
