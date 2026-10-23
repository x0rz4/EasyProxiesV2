package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

// healthJournalFixture opens a store-backed hub. The journal lives in the same
// SQLite file as the production data, so these are integration-level checks of
// the delivery contract rather than of a stand-in queue.
func healthJournalFixture(t *testing.T) (*healthEventHub, store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "health-journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hub := newHealthEventHub(func() store.Store { return db })
	t.Cleanup(hub.close)
	return hub, db
}

// subscribeReadOnly registers a test observer. Observers do not own group state,
// so they contribute no rows to the cursor transaction.
func (h *healthEventHub) subscribeReadOnly(consumer string, tags []string, nodeIDs []int64, global bool, callback func(HealthResultEvent)) func() {
	return h.subscribeAs(consumer, tags, nodeIDs, global, func(event HealthResultEvent) []store.GroupNodeState {
		callback(event)
		return nil
	})
}

// appendJournal writes rows the way probe.Applier does: the fact and its
// journal row commit together, then the probe path publishes the wakeup.
func appendJournal(t *testing.T, db store.Store, count int) []int64 {
	t.Helper()
	seqs := make([]int64, 0, count)
	at := time.Now().UTC()
	err := db.WithTx(context.Background(), func(tx store.Store) error {
		for i := 0; i < count; i++ {
			seq, err := tx.AppendHealthEvent(context.Background(), store.HealthEvent{
				NodeID: 1, IdentityToken: "identity", IdentityRevision: 1, Tag: "node-a",
				ProbeID: store.NewProbeID("probe"),
				Success: i%2 == 0, CheckedAt: at,
			})
			if err != nil {
				return err
			}
			seqs = append(seqs, seq)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seqs
}

func TestHealthJournalDeliversBeyondLegacyQueueLimit(t *testing.T) {
	hub, db := healthJournalFixture(t)
	const total = 5000 // more than the 4096-entry queue this replaced

	entered, release := make(chan struct{}), make(chan struct{})
	delivered := make(chan HealthResultEvent, total)
	var once sync.Once
	unsubscribe := hub.subscribeReadOnly("pool:1", []string{"node-a"}, nil, false, func(event HealthResultEvent) {
		once.Do(func() { close(entered); <-release })
		delivered <- event
	})
	defer unsubscribe()

	seqs := appendJournal(t, db, total)
	for _, seq := range seqs {
		hub.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seq})
	}
	// The consumer is now parked inside the first callback while the whole
	// batch is committed: this is the backlog that used to overflow.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer never started")
	}
	select {
	case event := <-delivered:
		t.Fatalf("event delivered before release: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	last := int64(0)
	for i := 0; i < total; i++ {
		select {
		case event := <-delivered:
			if event.Seq <= last {
				t.Fatalf("delivery out of order at %d: %d after %d", i, event.Seq, last)
			}
			last = event.Seq
		case <-time.After(60 * time.Second):
			t.Fatalf("delivered %d of %d events", i, total)
		}
	}
	if last != seqs[len(seqs)-1] {
		t.Fatalf("last delivered seq = %d, want %d", last, seqs[len(seqs)-1])
	}
	stats, err := hub.stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.RejectedMeasurements != 0 || len(stats.Consumers) != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if consumer := stats.Consumers[0]; consumer.Delivered < total || consumer.Dropped != 0 || consumer.Gaps != 0 {
		t.Fatalf("delivery reported loss: %+v", consumer)
	}
	if stats.Backlog != 0 {
		t.Fatalf("backlog after drain = %d", stats.Backlog)
	}
}

func TestHealthEventCursorResumesAfterRestart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	journal := func() store.Store { return db }

	first := make(chan HealthResultEvent, 8)
	hubOne := newHealthEventHub(journal)
	unsubscribe := hubOne.subscribeReadOnly("pool:7", []string{"node-a"}, nil, false, func(event HealthResultEvent) { first <- event })
	seqs := appendJournal(t, db, 3)
	for _, seq := range seqs {
		hubOne.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seq})
	}
	for i := 0; i < 3; i++ {
		select {
		case <-first:
		case <-time.After(10 * time.Second):
			t.Fatalf("first run delivered %d of 3", i)
		}
	}
	// The cursor is persisted after the batch; wait for it so the assertion
	// below checks the durable value, not a lucky ordering.
	deadline := time.Now().Add(10 * time.Second)
	for {
		seq, found, err := db.GetHealthEventCursor(ctx, "pool:7")
		if err != nil {
			t.Fatal(err)
		}
		if found && seq == seqs[len(seqs)-1] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor not persisted: %d %v", seq, found)
		}
		time.Sleep(5 * time.Millisecond)
	}
	unsubscribe()
	hubOne.close()

	// Restart: a new hub with the same consumer key must resume, not replay.
	second := make(chan HealthResultEvent, 8)
	hubTwo := newHealthEventHub(journal)
	defer hubTwo.close()
	hubTwo.subscribeReadOnly("pool:7", []string{"node-a"}, nil, false, func(event HealthResultEvent) { second <- event })
	more := appendJournal(t, db, 2)
	for _, seq := range more {
		hubTwo.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seq})
	}
	for i := 0; i < 2; i++ {
		select {
		case event := <-second:
			if event.Seq != more[i] {
				t.Fatalf("resumed at %d, want %d", event.Seq, more[i])
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("resumed run delivered %d of 2", i)
		}
	}
	select {
	case event := <-second:
		t.Fatalf("replayed already-consumed event: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

// cursorRefusingStore fails every cursor write. Delivery must park instead of
// running ahead: the group replay ring only covers one batch, so a restart would
// replay events whose ProbeIDs it has already dropped and count them twice.
type cursorRefusingStore struct {
	store.Store
	refuse atomic.Bool
}

func (s *cursorRefusingStore) PutHealthEventCursor(ctx context.Context, consumer string, seq int64) error {
	if s.refuse.Load() {
		return errors.New("injected cursor write failure")
	}
	return s.Store.PutHealthEventCursor(ctx, consumer, seq)
}

// AdvanceHealthCursorWithState is the commit path the hub actually uses, so the
// injection has to sit here too; overriding only PutHealthEventCursor would let
// the test pass while production writes kept succeeding.
func (s *cursorRefusingStore) AdvanceHealthCursorWithState(ctx context.Context, consumer string, seq int64, states []store.GroupNodeState) error {
	if s.refuse.Load() {
		return errors.New("injected cursor write failure")
	}
	return s.Store.AdvanceHealthCursorWithState(ctx, consumer, seq, states)
}

func TestHealthEventCursorFailureStopsDelivery(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "cursor-blocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	guarded := &cursorRefusingStore{Store: db}
	hub := newHealthEventHub(func() store.Store { return guarded })
	defer hub.close()

	// Refuse cursor writes from the start, so even first registration cannot
	// commit progress.
	guarded.refuse.Store(true)
	var mu sync.Mutex
	delivered := 0
	hub.subscribeReadOnly("pool:13", []string{"node-a"}, nil, false, func(HealthResultEvent) {
		mu.Lock()
		delivered++
		mu.Unlock()
	})
	// Ten batches' worth: enough that running ahead would blow past the 64-entry
	// in-process replay ring.
	seqs := appendJournal(t, db, 10*healthEventReadBatch)
	for _, seq := range seqs {
		hub.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seq})
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := delivered
		mu.Unlock()
		if n >= healthEventReadBatch {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give a runaway loop room to overshoot before asserting.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got > healthEventReadBatch {
		t.Fatalf("delivered %d events with an unpersisted cursor; one batch (%d) is the replayable window",
			got, healthEventReadBatch)
	}
	if _, found, err := db.GetHealthEventCursor(context.Background(), "pool:13"); err != nil || found {
		t.Fatalf("unexpected persisted cursor: %v %v", found, err)
	}

	// Once the write succeeds the consumer resumes from where it parked.
	guarded.refuse.Store(false)
	next := appendJournal(t, db, 1)
	hub.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: next[0]})
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if seq, found, err := db.GetHealthEventCursor(context.Background(), "pool:13"); err == nil && found && seq == next[0] {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("consumer did not resume after the cursor write recovered")
}

// Steady state journal size is probe rate × the consumed-retention window. If
// that product exceeds capacity, a normally-behaving deployment rejects every
// measurement even though every consumer is caught up. The first assertion pins
// the window to a rate far above any configurable probe interval; the second
// proves a caught-up consumer actually releases rows on that window.
func TestHealthEventJournalSustainsConfiguredProbeRate(t *testing.T) {
	if rate := float64(store.HealthEventJournalLimit) / healthEventConsumedRetention.Seconds(); rate < 20 {
		t.Fatalf("consumed retention %v sustains only %.1f rows/s at capacity %d; too low for multi-node probing",
			healthEventConsumedRetention, rate, store.HealthEventJournalLimit)
	}

	hub, db := healthJournalFixture(t)
	ctx := context.Background()
	hub.subscribeReadOnly("pool:15", []string{"node-a"}, nil, false, func(HealthResultEvent) {})
	// 2000 rows already older than the consumed window, with the sole consumer
	// caught up: a healthy deployment reclaiming space, not a wedged one.
	old := time.Now().UTC().Add(-healthEventConsumedRetention - time.Minute)
	last := int64(0)
	err := db.WithTx(ctx, func(tx store.Store) error {
		for i := 0; i < 2000; i++ {
			seq, err := tx.AppendHealthEvent(ctx, store.HealthEvent{
				NodeID: 1, Tag: "node-a", ProbeID: store.NewProbeID("probe"), CheckedAt: old, RecordedAt: old,
			})
			if err != nil {
				return err
			}
			last = seq
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutHealthEventCursor(ctx, "pool:15", last); err != nil {
		t.Fatal(err)
	}
	removed, err := db.PruneHealthEvents(ctx, time.Now().UTC().Add(-healthEventConsumedRetention), hub.durableConsumers())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2000 {
		t.Fatalf("caught-up consumer released only %d of 2000 consumed rows", removed)
	}
	if _, err := db.AppendHealthEvent(ctx, store.HealthEvent{NodeID: 1, Tag: "node-a", ProbeID: "after"}); err != nil {
		t.Fatalf("measurement still rejected after a healthy prune: %v", err)
	}
}

func TestHealthEventFirstRegistrationStartsAtTail(t *testing.T) {
	hub, db := healthJournalFixture(t)
	// History that predates the consumer must not be replayed: at first start
	// that would look like a burst of stale evictions and recoveries.
	appendJournal(t, db, 4)

	delivered := make(chan HealthResultEvent, 8)
	hub.subscribeReadOnly("pool:9", []string{"node-a"}, nil, false, func(event HealthResultEvent) { delivered <- event })
	seqs := appendJournal(t, db, 1)
	hub.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seqs[0]})
	select {
	case event := <-delivered:
		if event.Seq != seqs[0] {
			t.Fatalf("delivered stale seq %d, want %d", event.Seq, seqs[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("new event not delivered")
	}
	select {
	case event := <-delivered:
		t.Fatalf("first registration replayed history: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHealthEventCloseKeepsJournalForNextStart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "close.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	journal := func() store.Store { return db }

	entered, release := make(chan struct{}), make(chan struct{})
	hubOne := newHealthEventHub(journal)
	var once sync.Once
	hubOne.subscribeReadOnly("pool:5", []string{"node-a"}, nil, false, func(HealthResultEvent) {
		once.Do(func() { close(entered); <-release })
	})
	seqs := appendJournal(t, db, 2)
	for _, seq := range seqs {
		hubOne.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: seq})
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer never started")
	}
	hubOne.close() // cancels waiting; the in-flight callback may still finish
	close(release)

	// The undelivered rows survive the shutdown and are replayed on restart.
	// This replaces the old FIFO behaviour where close discarded the backlog.
	second := make(chan HealthResultEvent, 8)
	hubTwo := newHealthEventHub(journal)
	defer hubTwo.close()
	hubTwo.subscribeReadOnly("pool:5", []string{"node-a"}, nil, false, func(event HealthResultEvent) { second <- event })
	next := appendJournal(t, db, 1)
	hubTwo.publish(HealthResultEvent{Tag: "node-a", NodeID: 1, Seq: next[0]})
	for i := 0; i < len(seqs)+1; i++ {
		select {
		case <-second:
		case <-time.After(10 * time.Second):
			t.Fatalf("recovered %d of %d events after restart", i, len(seqs)+1)
		}
	}
}

// TestHealthEventInlineFallbackIsExplicitlyBounded covers the no-store
// compatibility path. It is not the production contract: without a journal
// there is nothing durable to replay from.
func TestHealthEventInlineFallbackIsExplicitlyBounded(t *testing.T) {
	hub := newHealthEventHub(func() store.Store { return nil })
	defer hub.close()
	entered, release := make(chan struct{}), make(chan struct{})
	var received atomic.Int64
	var once sync.Once
	unsubscribe := hub.subscribeReadOnly("", nil, nil, true, func(HealthResultEvent) {
		once.Do(func() { close(entered); <-release })
		received.Add(1)
	})
	defer unsubscribe()
	subscription := hub.onlySubscription(t)
	hub.publish(HealthResultEvent{Tag: "node-a", Success: true})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer never started")
	}
	for i := 0; i < healthEventInlineLimit+10; i++ {
		hub.publish(HealthResultEvent{Tag: "node-a", Success: true})
	}
	if subscription.Dropped() != int64(10) {
		t.Fatalf("dropped = %d, want 10", subscription.Dropped())
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for int(received.Load()) < healthEventInlineLimit+1 {
		if time.Now().After(deadline) {
			t.Fatalf("received %d of %d", received.Load(), healthEventInlineLimit+1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// onlySubscription returns the single registered subscription. Tests use it to
// read counters that are intentionally not part of the public API.
func (h *healthEventHub) onlySubscription(t *testing.T) *healthEventSubscription {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.byID) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(h.byID))
	}
	for _, subscription := range h.byID {
		return subscription
	}
	return nil
}

func TestHealthEventSnapshotOnlyIsMergeable(t *testing.T) {
	hub, _ := healthJournalFixture(t)
	delivered := make(chan HealthResultEvent, 8)
	hub.subscribeReadOnly("pool:3", []string{"node-a"}, nil, false, func(event HealthResultEvent) { delivered <- event })
	// Repeated refresh requests collapse into one notification. Refresh
	// markers are broadcast (they are not addressed to one tag or node).
	for i := 0; i < 5; i++ {
		hub.broadcast(HealthResultEvent{SnapshotOnly: true})
	}
	seen := 0
	for {
		select {
		case event := <-delivered:
			if !event.SnapshotOnly {
				t.Fatalf("unexpected event: %+v", event)
			}
			seen++
		case <-time.After(100 * time.Millisecond):
			if seen == 0 {
				t.Fatal("no refresh delivered")
			}
			return
		}
	}
}

// The cursor and the member state it produced must be one commit. This drives
// the hub end to end: when the combined write fails, delivery must park and
// neither half may land; when it recovers, both appear together.
type failingCommitStore struct {
	store.Store
	fail atomic.Bool
}

func (s *failingCommitStore) AdvanceHealthCursorWithState(ctx context.Context, consumer string, seq int64, states []store.GroupNodeState) error {
	if s.fail.Load() {
		return errors.New("injected commit failure")
	}
	return s.Store.AdvanceHealthCursorWithState(ctx, consumer, seq, states)
}

func TestHealthEventCursorAndStateCommitTogether(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "atomic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	node := &store.Node{URI: "http://atomic.example:80", Name: "atomic", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	groupPool := &store.GroupPool{Name: "atomic", BindAddress: "127.0.0.1", BindPort: 12099, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	guarded := &failingCommitStore{Store: db}
	hub := newHealthEventHub(func() store.Store { return guarded })
	defer hub.close()

	var delivered atomic.Int64
	hub.subscribeAs("pool:21", []string{"node-a"}, nil, false, func(HealthResultEvent) []store.GroupNodeState {
		delivered.Add(1)
		return []store.GroupNodeState{{GroupID: groupPool.ID, NodeID: node.ID,
			FailureHistory: []int64{1, 2, 3}, Evicted: true, LastError: "down"}}
	})

	guarded.fail.Store(true)
	seqs := appendJournal(t, db, 3)
	for _, seq := range seqs {
		hub.publish(HealthResultEvent{Tag: "node-a", NodeID: node.ID, Seq: seq})
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && delivered.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() == 0 {
		t.Fatal("callback never ran")
	}
	// Delivery must park: running ahead of an uncommitted transition is what
	// makes a restart replay it against a state that already contains it.
	time.Sleep(250 * time.Millisecond)
	if seq, _, err := db.GetHealthEventCursor(ctx, "pool:21"); err != nil || seq != 0 {
		t.Fatalf("cursor advanced to %d while the commit was failing: %v", seq, err)
	}
	loaded, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.NodeStates) != 0 {
		t.Fatalf("state written without its cursor: %+v", loaded.NodeStates)
	}

	// Once the commit succeeds, cursor and state land together.
	guarded.fail.Store(false)
	hub.onlySubscription(t).wakeup()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loaded, err = db.GetGroupPool(ctx, groupPool.ID)
		if err == nil && len(loaded.NodeStates) == 1 && loaded.NodeStates[0].Evicted {
			if seq, _, err := db.GetHealthEventCursor(ctx, "pool:21"); err == nil && seq == seqs[len(seqs)-1] {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state and cursor did not converge: states=%+v", loaded.NodeStates)
}
