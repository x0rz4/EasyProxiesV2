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

// createGroupStateFixture makes a real group+node pair so group_node_states rows
// satisfy their foreign keys.
func createGroupStateFixture(t *testing.T, db Store) (groupID, nodeID int64) {
	t.Helper()
	ctx := context.Background()
	node := &Node{URI: "http://atomicity.example:80", Name: "atomic", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	group := &GroupPool{Name: "atomic", BindAddress: "0.0.0.0", BindPort: 10011, Protocol: "mixed",
		DispatchMode: "fixed", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{node.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, group); err != nil {
		t.Fatal(err)
	}
	return group.ID, node.ID
}

// The cursor says "this transition was applied"; group_node_states is the
// transition. A crash between the two writes would either lose an eviction (the
// bad node returns healthy after restart) or replay an applied one. They must
// commit together, so a failed state write has to take the cursor down with it.
func TestAdvanceHealthCursorWithStateIsAtomic(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	groupID, nodeID := createGroupStateFixture(t, db)
	seq := appendHealthEvent(t, db, nodeID, "probe-1", false)

	// A trigger raises a non-foreign-key error for the next state write, which
	// is a genuine write failure the caller must see and retry — unlike a
	// deleted group, which is handled separately below.
	if _, err := db.(*sqliteStore).writerDB.ExecContext(ctx, `CREATE TRIGGER fail_state_write
BEFORE INSERT ON group_node_states WHEN NEW.last_error = 'boom'
BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.AdvanceHealthCursorWithState(ctx, "pool:1", seq, []GroupNodeState{{GroupID: groupID,
		NodeID: nodeID, Evicted: true, LastError: "boom"}}); err == nil {
		t.Fatal("expected the injected state write to fail")
	}
	if got, found, err := db.GetHealthEventCursor(ctx, "pool:1"); err != nil || found || got != 0 {
		t.Fatalf("cursor advanced despite a failed state write: seq=%d found=%v err=%v", got, found, err)
	}

	// The same call with a valid state commits both.
	states := []GroupNodeState{{GroupID: groupID, NodeID: nodeID, FailureHistory: []int64{1, 2, 3}, Evicted: true, LastError: "down"}}
	if err := db.AdvanceHealthCursorWithState(ctx, "pool:1", seq, states); err != nil {
		t.Fatal(err)
	}
	if got, found, err := db.GetHealthEventCursor(ctx, "pool:1"); err != nil || !found || got != seq {
		t.Fatalf("cursor = %d %v %v, want %d", got, found, err, seq)
	}
	group, err := db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || !group.NodeStates[0].Evicted || len(group.NodeStates[0].FailureHistory) != 3 {
		t.Fatalf("state not committed with the cursor: %+v", group.NodeStates)
	}
}

// A state row whose group or node was deleted between the transition and the
// commit can never be written. Treating that as retryable would park the
// consumer forever on one dead reference, stalling delivery for every other
// group, so the row is dropped and the cursor still advances.
func TestAdvanceHealthCursorSkipsStateForDeletedGroup(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	groupID, nodeID := createGroupStateFixture(t, db)
	seq := appendHealthEvent(t, db, nodeID, "probe-1", false)

	if err := db.AdvanceHealthCursorWithState(ctx, "pool:2", seq, []GroupNodeState{
		{GroupID: groupID + 5000, NodeID: nodeID, Evicted: true},
		{GroupID: groupID, NodeID: nodeID, Evicted: true, LastError: "down"},
	}); err != nil {
		t.Fatalf("a deleted group must not fail the commit: %v", err)
	}
	if got, found, err := db.GetHealthEventCursor(ctx, "pool:2"); err != nil || !found || got != seq {
		t.Fatalf("cursor = %d %v %v, want %d", got, found, err, seq)
	}
	group, err := db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || !group.NodeStates[0].Evicted {
		t.Fatalf("the live group's state was not written: %+v", group.NodeStates)
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

// group_node_states has two writers (the synchronous health-delivery
// transaction and the asynchronous persister). Their snapshots can commit out of
// order, so a write carrying an older revision must not overwrite a newer one:
// otherwise a slow eviction write could undo a recovery that already landed.
func TestUpsertGroupNodeStateRejectsStaleRevision(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	groupID, nodeID := createGroupStateFixture(t, db)

	recovered := GroupNodeState{GroupID: groupID, NodeID: nodeID, Evicted: false, LastError: "", Revision: 9}
	if err := db.UpsertGroupNodeState(ctx, &recovered); err != nil {
		t.Fatal(err)
	}
	stale := GroupNodeState{GroupID: groupID, NodeID: nodeID, FailureHistory: []int64{1, 2, 3},
		Evicted: true, LastError: "down", Revision: 4}
	if err := db.UpsertGroupNodeState(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	group, err := db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || group.NodeStates[0].Evicted {
		t.Fatalf("stale eviction overwrote a newer recovery: %+v", group.NodeStates)
	}

	// A newer write still applies, and so does an unversioned one: existing
	// callers that never set a revision keep working.
	newer := GroupNodeState{GroupID: groupID, NodeID: nodeID, Evicted: true, LastError: "down again", Revision: 12}
	if err := db.UpsertGroupNodeState(ctx, &newer); err != nil {
		t.Fatal(err)
	}
	unversioned := GroupNodeState{GroupID: groupID, NodeID: nodeID, Evicted: false}
	if err := db.UpsertGroupNodeState(ctx, &unversioned); err != nil {
		t.Fatal(err)
	}
	group, err = db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || group.NodeStates[0].Evicted {
		t.Fatalf("unversioned write did not apply: %+v", group.NodeStates)
	}
}

// A revision is only useful if it survives a restart. If a reload re-seeded the
// clock at zero, every write after startup would be rejected as stale and
// evictions would silently stop persisting — a failure mode with no error, only
// a bad node coming back healthy.
func TestGroupNodeStateRevisionRoundTripsThroughStore(t *testing.T) {
	db := openHealthEventJournal(t)
	ctx := context.Background()
	groupID, nodeID := createGroupStateFixture(t, db)

	if err := db.UpsertGroupNodeState(ctx, &GroupNodeState{GroupID: groupID, NodeID: nodeID,
		Evicted: true, LastError: "down", Revision: 77}); err != nil {
		t.Fatal(err)
	}
	group, err := db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || group.NodeStates[0].Revision != 77 {
		t.Fatalf("revision did not round-trip: %+v", group.NodeStates)
	}
	// A post-restart write carries a higher revision and must apply.
	if err := db.UpsertGroupNodeState(ctx, &GroupNodeState{GroupID: groupID, NodeID: nodeID,
		Evicted: false, Revision: 78}); err != nil {
		t.Fatal(err)
	}
	group, err = db.GetGroupPool(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(group.NodeStates) != 1 || group.NodeStates[0].Evicted {
		t.Fatalf("a post-restart recovery was rejected as stale: %+v", group.NodeStates)
	}
}
