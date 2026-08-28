package unlock

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type concurrencyChecker struct {
	key     string
	active  *atomic.Int64
	maximum *atomic.Int64
	calls   *atomic.Int64
	panic   bool
}

func (checker concurrencyChecker) Key() string         { return checker.key }
func (checker concurrencyChecker) Aliases() []string   { return nil }
func (checker concurrencyChecker) DisplayName() string { return checker.key }
func (checker concurrencyChecker) Order() int          { return 0 }
func (checker concurrencyChecker) Check(Runtime) ServiceResult {
	checker.calls.Add(1)
	current := checker.active.Add(1)
	defer checker.active.Add(-1)
	for {
		previous := checker.maximum.Load()
		if current <= previous || checker.maximum.CompareAndSwap(previous, current) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	if checker.panic {
		panic("checker failure")
	}
	return ServiceResult{Status: StatusUnlocked}
}

func TestRunRegisteredCheckersLimitsConcurrencyAndPreservesOrder(t *testing.T) {
	var active, maximum, calls atomic.Int64
	checkers := make([]Checker, 8)
	for index := range checkers {
		checkers[index] = concurrencyChecker{
			key: fmt.Sprintf("checker-%d", index), active: &active, maximum: &maximum,
			calls: &calls, panic: index == 3,
		}
	}

	services := runRegisteredCheckers(t.Context(), Runtime{}, checkers)
	if len(services) != len(checkers) || calls.Load() != int64(len(checkers)) {
		t.Fatalf("services=%d calls=%d", len(services), calls.Load())
	}
	if got := maximum.Load(); got != checkerConcurrency {
		t.Fatalf("maximum concurrency = %d, want %d", got, checkerConcurrency)
	}
	for index, service := range services {
		if service.Name != fmt.Sprintf("checker_%d", index) {
			t.Fatalf("service[%d] name = %q", index, service.Name)
		}
		wantStatus := StatusUnlocked
		if index == 3 {
			wantStatus = StatusFailed
		}
		if service.Status != wantStatus {
			t.Fatalf("service[%d] status = %q, want %q", index, service.Status, wantStatus)
		}
	}
}

func TestRunRegisteredCheckersMarksCancelledWorkFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var active, maximum, calls atomic.Int64
	checker := concurrencyChecker{key: "cancelled", active: &active, maximum: &maximum, calls: &calls}
	services := runRegisteredCheckers(ctx, Runtime{}, []Checker{checker, checker})
	if calls.Load() != 0 {
		t.Fatalf("cancelled checkers executed %d times", calls.Load())
	}
	for index, service := range services {
		if service.Status != StatusFailed {
			t.Fatalf("service[%d] status = %q, want failed", index, service.Status)
		}
	}
}
