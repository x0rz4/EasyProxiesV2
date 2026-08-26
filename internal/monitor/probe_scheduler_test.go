package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeConcurrencyFormula(t *testing.T) {
	for _, test := range []struct {
		nodes int
		want  int
	}{{0, 32}, {1, 32}, {319, 32}, {320, 32}, {321, 33}, {1000, 100}, {1280, 128}, {5000, 128}} {
		if got := autoProbeConcurrency(test.nodes); got != test.want {
			t.Fatalf("autoProbeConcurrency(%d) = %d, want %d", test.nodes, got, test.want)
		}
	}
	if got := effectiveProbeConcurrency(17, 1000); got != 17 {
		t.Fatalf("explicit concurrency = %d, want 17", got)
	}
	if got := effectiveProbeConcurrency(64, 12); got != 12 {
		t.Fatalf("concurrency was not capped to node count: %d", got)
	}
}

func TestProbeBatchUsesNodeScaledWorkerPool(t *testing.T) {
	mgr, err := NewManager(Config{
		ProbeTarget: "http://example.com", ProbeConcurrency: 0,
		RoutineProbeTimeout: time.Second, ProbeDialTimeout: 100 * time.Millisecond,
		ProbeResponseTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	var active, maximum atomic.Int64
	for index := range 1000 {
		handle := mgr.Register(NodeInfo{Tag: fmt.Sprintf("node-%04d", index)})
		handle.SetProbe(func(context.Context) (time.Duration, error) {
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			active.Add(-1)
			return time.Millisecond, nil
		})
	}
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 1000 || summary.Success != 1000 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if got := maximum.Load(); got != 100 {
		t.Fatalf("maximum concurrency = %d, want 100", got)
	}
}

func TestProbeBatchRetriesOnceAndCommitsOneHealthResult(t *testing.T) {
	mgr, err := NewManager(Config{
		ProbeTarget: "http://example.com", ProbeConcurrency: 1,
		RoutineProbeTimeout: time.Second, ProbeDialTimeout: 100 * time.Millisecond,
		ProbeResponseTimeout: 100 * time.Millisecond, RoutineProbeRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	var attempts atomic.Int32
	handle := mgr.Register(NodeInfo{Tag: "retry-node"})
	handle.SetProbe(func(context.Context) (time.Duration, error) {
		if attempts.Add(1) == 1 {
			return 0, errors.New("temporary failure")
		}
		return 12 * time.Millisecond, nil
	})
	var events atomic.Int32
	unsubscribe := mgr.SubscribeHealthResults(func(HealthResultEvent) { events.Add(1) })
	defer unsubscribe()
	var result ProbeBatchResult
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, func(value ProbeBatchResult) { result = value })
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || result.Attempts != 2 || summary.Success != 1 {
		t.Fatalf("attempts=%d result=%+v summary=%+v", attempts.Load(), result, summary)
	}
	if events.Load() != 1 {
		t.Fatalf("health results = %d, want exactly one final result", events.Load())
	}
}

func TestProbeBatchKeepsPhaseTimeoutSnapshot(t *testing.T) {
	mgr, err := NewManager(Config{
		ProbeTarget: "http://example.com", ProbeConcurrency: 1,
		RoutineProbeTimeout: time.Second, ProbeDialTimeout: 40 * time.Millisecond,
		ProbeResponseTimeout: 60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	observed := make(chan [2]time.Duration, 2)
	var calls atomic.Int32
	for index := range 2 {
		handle := mgr.Register(NodeInfo{Tag: fmt.Sprintf("snapshot-%d", index)})
		handle.SetProbe(func(ctx context.Context) (time.Duration, error) {
			dial, response := mgr.ProbePhaseTimeoutsFor(ctx)
			observed <- [2]time.Duration{dial, response}
			if calls.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			return time.Millisecond, nil
		})
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
		done <- runErr
	}()
	<-firstStarted
	mgr.UpdateProbePolicy(ProbePolicy{
		Concurrency: 1, RoutineTimeout: time.Second,
		DialTimeout: 5 * time.Millisecond, ResponseTimeout: 7 * time.Millisecond,
	})
	close(releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := <-observed; got != [2]time.Duration{40 * time.Millisecond, 60 * time.Millisecond} {
			t.Fatalf("phase timeout changed during round: %v", got)
		}
	}
}

func TestStartupProbeDoesNotRetry(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", RoutineProbeRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	var attempts atomic.Int32
	mgr.Register(NodeInfo{Tag: "startup-node"}).SetProbe(func(context.Context) (time.Duration, error) {
		attempts.Add(1)
		return 0, errors.New("offline")
	})
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundStartup, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 1 || summary.Failed != 1 {
		t.Fatalf("attempts=%d summary=%+v", attempts.Load(), summary)
	}
}

func TestProbeBatchMutualExclusionAndCancellation(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 1, RoutineProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	started := make(chan struct{})
	mgr.Register(NodeInfo{Tag: "blocked"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunProbeBatch(ctx, ProbeRoundManual, false, nil, nil)
		done <- runErr
	}()
	<-started
	if _, err := mgr.RunProbeBatch(t.Context(), ProbeRoundPeriodic, false, nil, nil); !errors.Is(err, ErrProbeRoundInProgress) {
		t.Fatalf("overlapping round error = %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled round error = %v", err)
	}
	if mgr.ProbeRoundStatus().InFlight {
		t.Fatal("round remained in flight after cancellation")
	}
}

func TestProbeBatchSkipsNodeWithStandaloneProbeInFlight(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	started := make(chan struct{})
	mgr.Register(NodeInfo{Tag: "busy-node"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mgr.Probe(ctx, "busy-node")
	}()
	<-started
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 0 || summary.Failed != 0 {
		t.Fatalf("busy standalone probe was counted as a batch failure: %+v", summary)
	}
	cancel()
	<-done
}

func TestProbeBatchDueOnlySkipsFreshNodes(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.SetHealthCheckInterval(time.Hour)
	var attempts atomic.Int32
	handle := mgr.Register(NodeInfo{Tag: "fresh"})
	handle.SetProbe(func(context.Context) (time.Duration, error) { attempts.Add(1); return time.Millisecond, nil })
	handle.ref.mu.Lock()
	handle.ref.lastHealthCheck = time.Now()
	handle.ref.mu.Unlock()
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundPeriodic, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 0 || attempts.Load() != 0 {
		t.Fatalf("fresh node was probed: summary=%+v attempts=%d", summary, attempts.Load())
	}
}
