package monitor

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDetachedProbeRetainsSlotUntilActualReturn(t *testing.T) {
	s := newProbeScheduler(&Manager{})
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	item := probeWorkItem{entry: &entry{}, tag: "blocked", probe: func(context.Context) (time.Duration, error) {
		close(started)
		<-release
		defer close(returned)
		return time.Millisecond, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := s.runProbeAttempt(ctx, item, time.Hour); done <- err }()
	<-started
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancelled attempt: %v", err)
	}
	if s.detachedProbes.Load() != 1 {
		t.Fatal("detached call was not counted")
	}
	_, err := s.runProbeAttempt(t.Context(), item, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("duplicate attempt: %v", err)
	}
	close(release)
	<-returned
	// Completion bookkeeping runs after the probe body returns.
	wait, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	for {
		s.attemptMu.Lock()
		remaining := len(s.runningAttempts)
		s.attemptMu.Unlock()
		if remaining == 0 && s.detachedProbes.Load() == 0 {
			break
		}
		select {
		case <-wait.Done():
			t.Fatalf("finished call retained: remaining=%d detached=%d", remaining, s.detachedProbes.Load())
		default:
			runtime.Gosched()
		}
	}
}

func TestOutstandingProbeCallsBoundRuntimeGenerations(t *testing.T) {
	s := newProbeScheduler(&Manager{})
	for range maxOutstandingProbeCalls {
		s.runningAttempts[probeAttemptKey{entry: &entry{}}] = &probeAttemptState{detached: true}
	}
	called := false
	_, err := s.runProbeAttempt(t.Context(), probeWorkItem{entry: &entry{}, tag: "new-generation", probe: func(context.Context) (time.Duration, error) {
		called = true
		return 0, nil
	}}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "limit reached") || called {
		t.Fatalf("unbounded outstanding call: err=%v called=%v", err, called)
	}
}

func TestProbeCapacityBackpressureDoesNotChangeNodeHealth(t *testing.T) {
	for _, startup := range []bool{false, true} {
		mgr, err := NewManager(Config{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(mgr.Stop)
		handle := mgr.Register(NodeInfo{Tag: "capacity"})
		handle.SetProbe(func(context.Context) (time.Duration, error) {
			t.Error("capacity-rejected probe reached network")
			return 0, nil
		})
		s := mgr.probeScheduler
		s.attemptMu.Lock()
		for range maxOutstandingProbeCalls {
			s.runningAttempts[probeAttemptKey{entry: &entry{}}] = &probeAttemptState{detached: true}
		}
		s.attemptMu.Unlock()
		before := mgr.SnapshotForTag("capacity")
		item := probeWorkItem{entry: mgr.registry.byTagEntry("capacity"), tag: "capacity"}
		item.probe = item.entry.probe
		policy := ProbePolicy{RoutineTimeout: time.Second, StartupTimeout: time.Second}
		var result ProbeBatchResult
		if startup {
			result = s.probeInitialAttempt(t.Context(), item, policy, 1, true)
		} else {
			result = s.probeRoutineItem(t.Context(), item, policy)
		}
		if result.Err != errProbeCallLimit {
			t.Fatalf("capacity result=%v", result.Err)
		}
		after := mgr.SnapshotForTag("capacity")
		if before.InitialCheckDone != after.InitialCheckDone || before.FailureCount != after.FailureCount || before.Available != after.Available {
			t.Fatalf("capacity changed node health: before=%+v after=%+v", before, after)
		}
	}
}
