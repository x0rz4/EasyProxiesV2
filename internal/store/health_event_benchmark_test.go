package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkHealthJournalStatsWithConsumers(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "stats.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx Store) error {
		for i := 0; i < 1000; i++ {
			if err := tx.PutHealthEventCursor(ctx, fmt.Sprint(i), int64(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := db.HealthEventJournalStats(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
