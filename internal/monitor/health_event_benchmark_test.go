package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

type cursorAcknowledgement struct {
	store.Store
	seq  atomic.Int64
	wake chan struct{}
}

func (s *cursorAcknowledgement) AdvanceHealthCursorWithState(ctx context.Context, key string, seq int64, states []store.GroupNodeState) error {
	if err := s.Store.AdvanceHealthCursorWithState(ctx, key, seq, states); err != nil {
		return err
	}
	s.seq.Store(seq)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// This is a local SQLite + journal consumer benchmark. Batch=1 commits every
// measurement separately; batch=64 illustrates amortization, not probe behavior.
// Network requests, measurement fact writes and group transitions are excluded.
func BenchmarkHealthJournalDelivery(b *testing.B) {
	for _, batch := range []int{1, 64} {
		b.Run(fmt.Sprint(batch), func(b *testing.B) {
			db, err := store.Open(filepath.Join(b.TempDir(), "journal.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			ack := &cursorAcknowledgement{Store: db, wake: make(chan struct{}, 1)}
			hub := newHealthEventHub(func() store.Store { return ack })
			defer hub.close()
			var delivered atomic.Int64
			hub.subscribeReadOnly("bench", nil, nil, true, func(HealthResultEvent) { delivered.Add(1) })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var last int64
				if err := db.WithTx(ctx, func(tx store.Store) error {
					for n := 0; n < batch; n++ {
						seq, err := tx.AppendHealthEvent(ctx, store.HealthEvent{NodeID: 1, Tag: "node-a", ProbeID: fmt.Sprintf("%d/%d", i, n)})
						if err != nil {
							return err
						}
						last = seq
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				hub.publish(HealthResultEvent{NodeID: 1, Tag: "node-a", Seq: last})
				for ack.seq.Load() < last {
					select {
					case <-ack.wake:
					case <-ctx.Done():
						b.Fatal(ctx.Err())
					}
				}
				if (i+1)*batch%1024 == 0 {
					if _, err := hub.prune(ctx, time.Now().Add(time.Second), false); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			if delivered.Load() != int64(b.N*batch) {
				b.Fatalf("delivered=%d expected=%d", delivered.Load(), b.N*batch)
			}
			b.ReportMetric(float64(b.N*batch)/b.Elapsed().Seconds(), "events/s")
		})
	}
}
