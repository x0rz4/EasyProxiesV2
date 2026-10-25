package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"easy_proxies/internal/store"
)

type usageObservation struct {
	id int64
	at time.Time
}
type usageStore struct {
	store.Store
	mu    sync.Mutex
	calls []usageObservation
	fail  bool
	block bool
}

func (s *usageStore) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	s.mu.Lock()
	s.calls = append(s.calls, usageObservation{id, at})
	fail, block := s.fail, s.block
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail {
		return errors.New("busy")
	}
	return nil
}
func (s *usageStore) observed() []usageObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usageObservation(nil), s.calls...)
}

func TestAPIKeyUseCoalescesAndFlushesLatest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := &usageStore{}
		a := NewAuth(AuthOptions{Store: db})
		a.RecordAPIKeyUse(1)
		synctest.Wait()
		if len(db.observed()) != 1 {
			t.Fatal("first use was not recorded")
		}
		time.Sleep(500 * time.Millisecond)
		latest := time.Now()
		for range 1000 {
			a.RecordAPIKeyUse(1)
		}
		a.SetStore(db) // Rebinding the same database must preserve pending metadata.
		synctest.Wait()
		if len(db.observed()) != 1 {
			t.Fatal("request burst bypassed throttling")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		calls := db.observed()
		if len(calls) != 2 || !calls[1].at.Equal(latest) {
			t.Fatalf("calls=%+v latest=%v", calls, latest)
		}
		a.RecordAPIKeyUse(1)
		final := time.Now()
		if err := a.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		calls = db.observed()
		if len(calls) != 3 || !calls[2].at.Equal(final) {
			t.Fatalf("final calls=%+v", calls)
		}
		a.RecordAPIKeyUse(2)
		if len(db.observed()) != 3 {
			t.Fatal("closed service accepted work")
		}
	})
}

func TestAPIKeyUseRetriesWithoutDroppingNewObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := &usageStore{fail: true}
		a := NewAuth(AuthOptions{Store: db})
		a.RecordAPIKeyUse(1)
		synctest.Wait()
		time.Sleep(time.Second)
		latest := time.Now()
		a.RecordAPIKeyUse(1)
		synctest.Wait()
		if len(db.observed()) != 1 {
			t.Fatal("retry backoff bypassed")
		}
		db.mu.Lock()
		db.fail = false
		db.mu.Unlock()
		time.Sleep(apiKeyUseRetry)
		synctest.Wait()
		calls := db.observed()
		if len(calls) != 2 || !calls[1].at.Equal(latest) {
			t.Fatalf("retry calls=%+v", calls)
		}
		if err := a.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAPIKeyUseShutdownBudgetAndRebinding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old, next := &usageStore{}, &usageStore{block: true}
		a := NewAuth(AuthOptions{Store: old})
		a.RecordAPIKeyUse(1)
		synctest.Wait()
		a.RecordAPIKeyUse(1) // Pending, throttled observation from the old binding.
		a.SetStore(next)
		a.RecordAPIKeyUse(2)
		synctest.Wait()
		budget, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := a.Close(budget); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close=%v", err)
		}
		synctest.Wait()
		for _, call := range next.observed() {
			if call.id != 2 {
				t.Fatalf("old binding crossed store: %+v", call)
			}
		}
	})
}

func TestAPIKeyUsageQueueIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAuth(AuthOptions{Store: &usageStore{}})
		for id := int64(1); id <= 2*maxAPIKeyUseEntries; id++ {
			a.RecordAPIKeyUse(id)
		}
		synctest.Wait()
		a.mu.Lock()
		count, dropped := len(a.usage.entries), a.usage.dropped
		a.mu.Unlock()
		if count > maxAPIKeyUseEntries || dropped == 0 {
			t.Fatalf("entries=%d dropped=%d", count, dropped)
		}
		if err := a.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
