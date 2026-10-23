package probe

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

func fixture(t *testing.T) (store.Store, store.Node, Result) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	node := store.Node{Name: "node", URI: "http://127.0.0.1:8080", Enabled: true, Source: store.NodeSourceManual}
	if err := db.CreateNode(context.Background(), &node); err != nil {
		t.Fatal(err)
	}
	event, err := New(db).Begin(context.Background(), node.ID, node.URI, ManualDiagnostic, Latency)
	if err != nil {
		t.Fatal(err)
	}
	event.Observation.StartedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	event.Observation.CheckedAt = event.Observation.StartedAt.Add(time.Second)
	event.Observation.ProbeID = "first"
	latency := int64(42)
	event.Detection = &store.NodeDetectionResult{NodeID: node.ID, LatencyStatus: "success", LatencyMs: &latency, LatencyCheckedAt: event.Observation.CheckedAt, SpeedStatus: "untested", ExitIPStatus: "untested"}
	return db, node, event
}

func TestReceiptRejectsReplayOldAndReplacedIdentity(t *testing.T) {
	db, node, event := fixture(t)
	ctx := context.Background()
	a := New(db)
	out, err := a.Apply(ctx, event)
	if err != nil || !out.Applied {
		t.Fatalf("first: %+v %v", out, err)
	}
	// A new Applier shares the persisted order after restart.
	replay, err := New(db).Apply(ctx, event)
	if err != nil || replay.Applied || replay.Reason != "duplicate" {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	older := event
	older.Observation.ProbeID = "older"
	older.Observation.StartedAt = event.Observation.StartedAt.Add(-time.Second)
	if got, err := a.Apply(ctx, older); err != nil || got.Applied || got.Reason != "older_probe" {
		t.Fatalf("older: %+v %v", got, err)
	}
	node.URI = "http://127.0.0.2:8080"
	if err := db.UpdateNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	event.Observation.ProbeID = "obsolete"
	if got, err := a.Apply(ctx, event); err != nil || got.Applied || got.Reason != "obsolete_identity" {
		t.Fatalf("identity: %+v %v", got, err)
	}
	pending, err := db.ListPendingProbeReceipts(ctx, 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("old receipts survived replacement: %+v %v", pending, err)
	}
}

type rejectReceipt struct{ store.Store }

func (r rejectReceipt) PutProbeReceipt(context.Context, store.ProbeReceipt) error {
	return errors.New("receipt disk failure")
}
func (r rejectReceipt) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return r.Store.WithTx(ctx, func(tx store.Store) error { return fn(rejectReceipt{tx}) })
}

func TestReceiptAndMeasurementRollbackTogether(t *testing.T) {
	db, _, event := fixture(t)
	out, err := New(rejectReceipt{db}).Apply(context.Background(), event)
	if err == nil || out.Applied {
		t.Fatalf("failed receipt committed: %+v %v", out, err)
	}
	rows, err := db.ListNodeDetectionResults(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("measurement escaped transaction: %+v %v", rows, err)
	}
}

func TestProjectionRetryAndConditionalAcknowledgment(t *testing.T) {
	db, _, event := fixture(t)
	a := New(db)
	ctx := context.Background()
	old, err := a.Apply(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reconcile(ctx, func(context.Context, Outcome) error { return errors.New("projection failed") }); err == nil {
		t.Fatal("projection failure lost")
	}
	pending, err := db.ListPendingProbeReceipts(ctx, 100)
	if err != nil || len(pending) != 1 {
		t.Fatal("pending receipt lost")
	}
	event.Observation.ProbeID = "newer"
	event.Observation.StartedAt = event.Observation.StartedAt.Add(time.Minute)
	event.Observation.CheckedAt = event.Observation.StartedAt.Add(time.Second)
	if _, err := a.Apply(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := a.Acknowledge(ctx, old); err != nil {
		t.Fatal(err)
	}
	pending, err = db.ListPendingProbeReceipts(ctx, 100)
	if err != nil || len(pending) != 1 {
		t.Fatal("old ack cleared new event")
	}
	calls := 0
	if err := New(db).Reconcile(ctx, func(_ context.Context, out Outcome) error {
		calls++
		if out.Event.Observation.ProbeID != "newer" {
			t.Fatal("old event projected")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("projection calls: %d", calls)
	}
	pending, err = db.ListPendingProbeReceipts(ctx, 100)
	if err != nil || len(pending) != 0 {
		t.Fatal("projection not acknowledged")
	}
}

func TestManualResultCannotPublishHealth(t *testing.T) {
	db, _, event := fixture(t)
	event.Kind = Health
	event.Detection = nil
	event.Health = &HealthResult{Tag: "runtime", Success: true}
	if _, err := New(db).Apply(context.Background(), event); err == nil {
		t.Fatal("manual event accepted health payload")
	}
}

// healthEvent builds a valid health-origin measurement for the journal tests.
func healthEvent(t *testing.T, db store.Store, node store.Node, tag string) Result {
	t.Helper()
	event, err := New(db).Begin(context.Background(), node.ID, node.URI, HealthProbe, Health)
	if err != nil {
		t.Fatal(err)
	}
	event.Observation.StartedAt = time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	event.Observation.CheckedAt = event.Observation.StartedAt.Add(time.Second)
	event.Observation.ProbeID = store.NewProbeID("health")
	event.Health = &HealthResult{Tag: tag, Success: true, Latency: 12 * time.Millisecond}
	return event
}

func TestHealthMeasurementAppendsJournalRowInSameTransaction(t *testing.T) {
	db, node, latencyEvent := fixture(t)
	ctx := context.Background()
	// A manual latency diagnostic is a fact, not a health transition: it must
	// stay out of the journal the group state machine consumes.
	if out, err := New(db).Apply(ctx, latencyEvent); err != nil || !out.Applied {
		t.Fatalf("latency apply: %+v %v", out, err)
	}
	if events, err := db.ListHealthEventsAfter(ctx, 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("diagnostic entered health journal: %+v %v", events, err)
	}

	event := healthEvent(t, db, node, "node")
	out, err := New(db).Apply(ctx, event)
	if err != nil || !out.Applied || out.Seq == 0 {
		t.Fatalf("health apply: %+v %v", out, err)
	}
	events, err := db.ListHealthEventsAfter(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("journal rows = %d, want 1", len(events))
	}
	row := events[0]
	if row.Seq != out.Seq || row.ProbeID != event.Observation.ProbeID || row.Tag != "node" || !row.Success {
		t.Fatalf("journal row lost evidence: %+v", row)
	}
	if row.IdentityToken == "" || row.IdentityRevision == 0 {
		t.Fatalf("journal row lost identity token: %+v", row)
	}
}

func TestHealthMeasurementRejectedWhenJournalFull(t *testing.T) {
	db, node, _ := fixture(t)
	ctx := context.Background()
	err := db.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < store.HealthEventJournalLimit; i++ {
			if _, err := tx.AppendHealthEvent(ctx, store.HealthEvent{NodeID: node.ID, ProbeID: "seed", Tag: "node"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	event := healthEvent(t, db, node, "node")
	out, err := New(db).Apply(ctx, event)
	if !errors.Is(err, store.ErrHealthEventJournalFull) {
		t.Fatalf("expected journal full, got %+v %v", out, err)
	}
	if out.Applied || out.Seq != 0 {
		t.Fatalf("rejected measurement left a result: %+v", out)
	}
	// Backpressure must not leave the fact behind without its journal row.
	if receipt, err := db.GetProbeReceipt(ctx, node.ID, "health"); err != nil || receipt != nil {
		t.Fatalf("receipt written under backpressure: %+v %v", receipt, err)
	}
	stats, err := db.GetNodeStats(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats != nil && stats.InitialCheckDone {
		t.Fatalf("node health advanced under backpressure: %+v", stats)
	}
}
