package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

func TestJournalPressureReclaimsOnlyConfirmedPrefix(t *testing.T) {
	hub, db := healthJournalFixture(t)
	hub.subscribeReadOnly("fast", nil, nil, true, func(HealthResultEvent) {})
	hub.subscribeReadOnly("slow", nil, nil, true, func(HealthResultEvent) {})
	ctx := context.Background()
	seqs := appendJournal(t, db, healthEventPressureThreshold)
	if err := db.PutHealthEventCursor(ctx, "fast", seqs[len(seqs)-1]); err != nil {
		t.Fatal(err)
	}
	if err := db.PutHealthEventCursor(ctx, "slow", seqs[1023]); err != nil {
		t.Fatal(err)
	}
	removed, err := hub.prune(ctx, time.Now().Add(time.Second), false)
	if err != nil || removed != 1024 {
		t.Fatalf("pressure prune removed=%d err=%v", removed, err)
	}
	remaining, err := db.ListHealthEventsAfter(ctx, 0, 1)
	if err != nil || len(remaining) != 1 || remaining[0].Seq != seqs[1024] {
		t.Fatalf("unconfirmed prefix lost: %+v %v", remaining, err)
	}
	appendJournal(t, db, store.HealthEventJournalLimit-(healthEventPressureThreshold-1024))
	if removed, err := hub.prune(ctx, time.Now().Add(time.Second), false); err != nil || removed != 0 {
		t.Fatalf("stalled consumer debt pruned: %d %v", removed, err)
	}
	if _, err := db.AppendHealthEvent(ctx, store.HealthEvent{NodeID: 1, ProbeID: "overflow"}); !errors.Is(err, store.ErrHealthEventJournalFull) {
		t.Fatalf("stalled consumer lost hard backpressure: %v", err)
	}
	stats, err := hub.stats(ctx)
	if err != nil || stats.PressurePrunes != 1 || stats.PrunedEvents != 1024 || stats.PressureThreshold != healthEventPressureThreshold {
		t.Fatalf("pressure not observable: %+v %v", stats, err)
	}
}

func TestJournalLowPressurePreservesHistory(t *testing.T) {
	hub, db := healthJournalFixture(t)
	ctx := context.Background()
	seqs := appendJournal(t, db, 10)
	hub.subscribeReadOnly("caught-up", nil, nil, true, func(HealthResultEvent) {})
	if removed, err := hub.prune(ctx, time.Now().Add(time.Second), true); err != nil || removed != 0 {
		t.Fatalf("fresh history pruned: %d %v", removed, err)
	}
	if err := db.PutHealthEventCursor(ctx, "caught-up", seqs[9]); err != nil {
		t.Fatal(err)
	}
	if removed, err := hub.prune(ctx, time.Now().Add(healthEventConsumedRetention+time.Second), true); err != nil || removed != 10 {
		t.Fatalf("old history retained: %d %v", removed, err)
	}
}

// Simulate the one-second pressure checks without sleeping. Each batch uses
// real SQLite transactions; advancing the cursor models a caught-up durable
// consumer, not dropping events to make a rate assertion pass.
func TestJournalPressureSustainsHighRateWithBoundedRows(t *testing.T) {
	for _, rate := range []int{100, 500} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			hub, db := healthJournalFixture(t)
			ctx := context.Background()
			hub.subscribeReadOnly("caught-up", nil, nil, true, func(HealthResultEvent) {})
			base := time.Now().UTC()
			started := time.Now()
			var maxRows int64
			for tick := 0; tick < 90; tick++ {
				at := base.Add(time.Duration(tick) * time.Second)
				if err := db.WithTx(ctx, func(tx store.Store) error {
					var last int64
					for n := 0; n < rate; n++ {
						seq, err := tx.AppendHealthEvent(ctx, store.HealthEvent{NodeID: 1, Tag: "node-a", ProbeID: fmt.Sprintf("%d/%d", tick, n), RecordedAt: at})
						if err != nil {
							return err
						}
						last = seq
					}
					return tx.PutHealthEventCursor(ctx, "caught-up", last)
				}); err != nil {
					t.Fatalf("tick=%d: %v", tick, err)
				}
				stats, err := db.HealthEventJournalStats(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if stats.Count > maxRows {
					maxRows = stats.Count
				}
				if _, err := hub.prune(ctx, at.Add(time.Second), tick%60 == 0); err != nil {
					t.Fatal(err)
				}
			}
			if maxRows >= store.HealthEventJournalLimit || hub.pressurePrunes.Load() == 0 {
				t.Fatalf("pressure ineffective: max=%d prunes=%d", maxRows, hub.pressurePrunes.Load())
			}
			t.Logf("accepted=%d simulated_seconds=90 rate=%d/s max_rows=%d capacity=%d wall_time=%s", rate*90, rate, maxRows, store.HealthEventJournalLimit, time.Since(started))
		})
	}
}
