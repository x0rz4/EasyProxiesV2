package monitor

import (
	"context"
	"time"
)

// prune keeps a short history while there is headroom, then reclaims only the
// acknowledged prefix under pressure. It never expands the fixed journal or
// drops the debt of a registered consumer to make a measurement fit.
func (h *healthEventHub) prune(ctx context.Context, now time.Time, ageSweep bool) (int64, error) {
	db := h.journal()
	if db == nil {
		return 0, nil
	}
	stats, err := db.HealthEventJournalStats(ctx)
	if err != nil {
		return 0, err
	}
	pressure := stats.Count >= healthEventPressureThreshold
	if stats.Count == 0 || !pressure && !ageSweep {
		return 0, nil
	}
	consumers := h.durableConsumers()
	window := healthEventConsumedRetention
	if len(consumers) == 0 {
		window = healthEventRetention
	}
	if pressure {
		window = 0
	}
	removed, err := db.PruneHealthEvents(ctx, now.UTC().Add(-window), consumers)
	if err != nil {
		return 0, err
	}
	h.prunedEvents.Add(removed)
	if pressure && removed > 0 {
		h.pressurePrunes.Add(1)
	}
	return removed, nil
}
