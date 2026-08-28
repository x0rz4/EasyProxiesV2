package monitor

import (
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectLimitedBoundsConcurrencyAndReturnsEveryResult(t *testing.T) {
	values := make([]int, 24)
	for index := range values {
		values[index] = index
	}
	var active, maximum atomic.Int64
	results := collectLimited(3, values, func(value int) int {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return value
	})

	var collected []int
	for result := range results {
		collected = append(collected, result)
	}
	sort.Ints(collected)
	if len(collected) != len(values) {
		t.Fatalf("results = %d, want %d", len(collected), len(values))
	}
	for index, value := range collected {
		if value != index {
			t.Fatalf("result[%d] = %d", index, value)
		}
	}
	if got := maximum.Load(); got != 3 {
		t.Fatalf("maximum concurrency = %d, want 3", got)
	}
}

func TestRunLimitedNormalizesInvalidAndOversizedLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		want  int64
	}{{"invalid", 0, 1}, {"oversized", 10, 2}} {
		t.Run(test.name, func(t *testing.T) {
			var active, maximum atomic.Int64
			runLimited(test.limit, []int{1, 2}, func(int) {
				current := active.Add(1)
				for {
					previous := maximum.Load()
					if current <= previous || maximum.CompareAndSwap(previous, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				active.Add(-1)
			})
			if got := maximum.Load(); got != test.want {
				t.Fatalf("maximum concurrency = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCollectLimitedEmptyInputClosesImmediately(t *testing.T) {
	if _, ok := <-collectLimited[int, int](4, nil, func(value int) int { return value }); ok {
		t.Fatal("empty result channel remained open")
	}
}
