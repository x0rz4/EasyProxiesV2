package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

func TestStartupProbeRetriesOnceAfterBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, err := NewManager(Config{ProbeTarget: "http://example.com", RoutineProbeRetries: 2})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Stop()
		started := time.Now()
		var attempts atomic.Int32
		mgr.Register(NodeInfo{Tag: "startup-node"}).SetProbe(func(context.Context) (time.Duration, error) {
			attempts.Add(1)
			return 0, errors.New("offline")
		})
		summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundStartup, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 2 || summary.Failed != 1 || time.Since(started) != initialProbeRetryDelay {
			t.Fatalf("attempts=%d elapsed=%v summary=%+v", attempts.Load(), time.Since(started), summary)
		}
		snapshot := mgr.SnapshotForTag("startup-node")
		if snapshot == nil || !snapshot.InitialCheckDone || snapshot.Available || snapshot.LastError != "offline" {
			t.Fatalf("failed startup probe did not reach unavailable: %+v", snapshot)
		}
	})
}

func TestStartupProbeSecondAttemptCanRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Stop()
		var attempts atomic.Int32
		mgr.Register(NodeInfo{Tag: "startup-recovers"}).SetProbe(func(context.Context) (time.Duration, error) {
			if attempts.Add(1) == 1 {
				return 0, errors.New("temporary")
			}
			return 23 * time.Millisecond, nil
		})
		summary, runErr := mgr.RunProbeBatch(t.Context(), ProbeRoundStartup, false, nil, nil)
		if runErr != nil {
			t.Fatal(runErr)
		}
		snapshot := mgr.SnapshotForTag("startup-recovers")
		if attempts.Load() != 2 || summary.Success != 1 || snapshot == nil || !snapshot.InitialCheckDone || !snapshot.Available || snapshot.LastLatencyMs != 23 {
			t.Fatalf("attempts=%d summary=%+v snapshot=%+v", attempts.Load(), summary, snapshot)
		}
	})
}

func TestProbeBatchCompletesFailuresAndSerializesResults(t *testing.T) {
	mgr, err := NewManager(Config{
		ProbeTarget: "http://example.com", ProbeConcurrency: 4,
		RoutineProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	for index := range 12 {
		handle := mgr.Register(NodeInfo{Tag: fmt.Sprintf("mixed-%d", index)})
		failed := index%3 == 0
		handle.SetProbe(func(context.Context) (time.Duration, error) {
			if failed {
				return 0, errors.New("offline")
			}
			return time.Millisecond, nil
		})
	}
	var callbacks, activeCallbacks, maximumCallbacks atomic.Int64
	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, func(ProbeBatchResult) {
		current := activeCallbacks.Add(1)
		for {
			previous := maximumCallbacks.Load()
			if current <= previous || maximumCallbacks.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		callbacks.Add(1)
		activeCallbacks.Add(-1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 12 || summary.Success != 8 || summary.Failed != 4 {
		t.Fatalf("summary = %+v", summary)
	}
	if callbacks.Load() != 12 || maximumCallbacks.Load() != 1 {
		t.Fatalf("callbacks=%d maximum concurrent callbacks=%d", callbacks.Load(), maximumCallbacks.Load())
	}
}

func TestProbeBatchCancellationReleasesEveryReservedTag(t *testing.T) {
	mgr, err := NewManager(Config{
		ProbeTarget: "http://example.com", ProbeConcurrency: 1,
		RoutineProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	firstStarted := make(chan struct{})
	var blockFirst atomic.Bool
	blockFirst.Store(true)
	for index := range 3 {
		handle := mgr.Register(NodeInfo{Tag: fmt.Sprintf("cancel-release-%d", index)})
		handle.SetProbe(func(ctx context.Context) (time.Duration, error) {
			if blockFirst.CompareAndSwap(true, false) {
				close(firstStarted)
				<-ctx.Done()
				return 0, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			return time.Millisecond, nil
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunProbeBatch(ctx, ProbeRoundManual, false, nil, nil)
		done <- runErr
	}()
	<-firstStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch error = %v", err)
	}

	summary, err := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 3 || summary.Success != 3 || summary.Failed != 0 {
		t.Fatalf("second batch summary = %+v; reserved tags were not all released", summary)
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

func waitForInitialConvergence(t *testing.T, mgr *Manager) InitialProbeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := mgr.InitialProbeStatus()
		if !status.Converging && status.Pending == 0 {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status := mgr.InitialProbeStatus()
	t.Fatalf("initial convergence did not finish: %+v round=%+v", status, mgr.ProbeRoundStatus())
	return status
}

func TestInitialConvergenceQueuedDuringManualRoundIsNotDropped(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 1, StartupProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	manualStarted := make(chan struct{})
	releaseManual := make(chan struct{})
	mgr.Register(NodeInfo{Tag: "manual-blocker"}).SetProbe(func(context.Context) (time.Duration, error) {
		close(manualStarted)
		<-releaseManual
		return time.Millisecond, nil
	})
	done := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
		done <- runErr
	}()
	<-manualStarted
	var queuedCalls atomic.Int32
	mgr.Register(NodeInfo{Tag: "queued-after-manual"}).SetProbe(func(context.Context) (time.Duration, error) {
		queuedCalls.Add(1)
		return time.Millisecond, nil
	})
	mgr.RequestProbeTagsOnce([]string{"queued-after-manual"})
	if status := mgr.InitialProbeStatus(); !status.Converging || status.Queued != 1 {
		t.Fatalf("startup request was not retained: %+v", status)
	}
	close(releaseManual)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitForInitialConvergence(t, mgr)
	if queuedCalls.Load() != 1 {
		t.Fatalf("queued probe calls = %d, want 1", queuedCalls.Load())
	}
}

func TestPeriodicRequestQueuedDuringManualRoundIsNotDropped(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 1, RoutineProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	manualStarted := make(chan struct{})
	releaseManual := make(chan struct{})
	var manualCalls atomic.Int32
	mgr.Register(NodeInfo{Tag: "manual-owner"}).SetProbe(func(context.Context) (time.Duration, error) {
		if manualCalls.Add(1) == 1 {
			close(manualStarted)
			<-releaseManual
		}
		return time.Millisecond, nil
	})
	done := make(chan error, 1)
	go func() {
		_, runErr := mgr.RunProbeBatch(t.Context(), ProbeRoundManual, false, nil, nil)
		done <- runErr
	}()
	<-manualStarted
	var queuedCalls atomic.Int32
	mgr.Register(NodeInfo{Tag: "periodic-queued"}).SetProbe(func(context.Context) (time.Duration, error) {
		queuedCalls.Add(1)
		return time.Millisecond, nil
	})
	mgr.RequestRoutineProbeAllOnce()
	close(releaseManual)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for queuedCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if queuedCalls.Load() != 1 {
		t.Fatalf("queued periodic calls = %d, want 1", queuedCalls.Load())
	}
}

func TestInitialConvergenceBoundsTargetedProbeConcurrency(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 17, StartupProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	const count = 1000
	tags := make([]string, 0, count)
	var calls, active, maximum atomic.Int64
	for index := range count {
		tag := fmt.Sprintf("targeted-%04d", index)
		tags = append(tags, tag)
		mgr.Register(NodeInfo{Tag: tag}).SetProbe(func(context.Context) (time.Duration, error) {
			calls.Add(1)
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			return time.Millisecond, nil
		})
	}
	mgr.RequestProbeTagsOnce(tags)
	waitForInitialConvergence(t, mgr)
	if calls.Load() != count || maximum.Load() > 17 {
		t.Fatalf("calls=%d maximum=%d", calls.Load(), maximum.Load())
	}
}

func TestInitialConvergenceRetriesBusyTagAfterLeaseRelease(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	var calls atomic.Int32
	mgr.Register(NodeInfo{Tag: "busy-initial"}).SetProbe(func(context.Context) (time.Duration, error) {
		calls.Add(1)
		return time.Millisecond, nil
	})
	if !mgr.beginTagProbe("busy-initial") {
		t.Fatal("failed to reserve test tag")
	}
	mgr.RequestProbeTagsOnce([]string{"busy-initial"})
	time.Sleep(10 * time.Millisecond)
	if calls.Load() != 0 || mgr.InitialProbeStatus().Queued != 1 {
		t.Fatalf("busy tag did not remain queued: calls=%d status=%+v", calls.Load(), mgr.InitialProbeStatus())
	}
	mgr.endTagProbe("busy-initial")
	waitForInitialConvergence(t, mgr)
	if calls.Load() != 1 {
		t.Fatalf("busy tag calls = %d, want 1", calls.Load())
	}
}

func TestInitialConvergenceRecoversWithoutWakeNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Stop()
		var calls atomic.Int32
		mgr.Register(NodeInfo{Tag: "lost-wake"}).SetProbe(func(context.Context) (time.Duration, error) {
			calls.Add(1)
			return time.Millisecond, nil
		})
		// Install queued state without using enqueueInitialProbeTags, modeling a
		// coalesced wake notification at the exact producer/consumer boundary.
		mgr.initialMu.Lock()
		mgr.initialRunning = true
		mgr.initialQueue["lost-wake"] = struct{}{}
		mgr.initialMu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("watchdog probe calls = %d, want 1", calls.Load())
		}
		if status := mgr.InitialProbeStatus(); status.Converging || status.Pending != 0 {
			t.Fatalf("watchdog convergence status = %+v", status)
		}
	})
}

func TestInitialConvergenceMissingProbeBecomesUnavailable(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(NodeInfo{Tag: "missing-probe"})
	mgr.RequestProbeTagsOnce([]string{"missing-probe"})
	waitForInitialConvergence(t, mgr)
	snapshot := mgr.SnapshotForTag("missing-probe")
	if snapshot == nil || !snapshot.InitialCheckDone || snapshot.Available || snapshot.LastError != "probe function not configured" {
		t.Fatalf("missing probe snapshot = %+v", snapshot)
	}
}

func TestInitialProbeAttemptsUseIndependentTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const attemptTimeout = 2 * time.Second
		mgr, err := NewManager(Config{ProbeTarget: "http://example.com", StartupProbeTimeout: attemptTimeout})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Stop()
		var attempts atomic.Int32
		mgr.Register(NodeInfo{Tag: "timeout-twice"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
			attempts.Add(1)
			<-ctx.Done()
			return 0, ctx.Err()
		})
		started := time.Now()
		summary, runErr := mgr.RunProbeBatch(t.Context(), ProbeRoundStartup, false, nil, nil)
		if runErr != nil {
			t.Fatal(runErr)
		}
		wantElapsed := 2*attemptTimeout + initialProbeRetryDelay
		if attempts.Load() != 2 || summary.Failed != 1 || time.Since(started) != wantElapsed {
			t.Fatalf("attempts=%d elapsed=%v want=%v summary=%+v", attempts.Load(), time.Since(started), wantElapsed, summary)
		}
	})
}

func TestStoppingInitialConvergenceReleasesLeaseWithoutMarkingFailure(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 1, StartupProbeTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	mgr.Register(NodeInfo{Tag: "cancel-initial"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	mgr.RequestStartupProbeAllOnce()
	<-started
	mgr.Stop()
	deadline := time.Now().Add(time.Second)
	released := false
	for time.Now().Before(deadline) {
		if mgr.beginTagProbe("cancel-initial") {
			mgr.endTagProbe("cancel-initial")
			released = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !released {
		t.Fatal("initial probe lease was not released after shutdown")
	}
	snapshot := mgr.SnapshotForTag("cancel-initial")
	if snapshot == nil || snapshot.InitialCheckDone || snapshot.LastError != "" {
		t.Fatalf("shutdown synthesized an unavailable result: %+v", snapshot)
	}
}

func TestProbeBatchRuntimeTagMigrationReleasesOldLeaseAndSkipsOldResult(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com", ProbeConcurrency: 1, RoutineProbeTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var oldCalls, newCalls atomic.Int32
	handle := mgr.Register(NodeInfo{NodeID: 2, Tag: "node@v1"})
	oldProbe := func(context.Context) (time.Duration, error) {
		oldCalls.Add(1)
		close(probeStarted)
		<-releaseProbe
		return time.Millisecond, nil
	}
	handle.SetProbe(oldProbe)
	if !mgr.beginTagProbe("node@v1") {
		t.Fatal("failed to reserve old runtime tag")
	}
	done := make(chan ProbeBatchResult, 1)
	go func() {
		done <- mgr.probeBatchItem(t.Context(), probeWorkItem{entry: handle.ref, tag: "node@v1", name: "old", probe: oldProbe}, time.Second, 0, time.Second, time.Second)
	}()
	<-probeStarted
	migrated := mgr.MigrateRuntimeTag(2, NodeInfo{NodeID: 2, Tag: "node@v2"})
	migrated.SetProbe(func(context.Context) (time.Duration, error) {
		newCalls.Add(1)
		return time.Millisecond, nil
	})
	close(releaseProbe)
	<-done
	if oldCalls.Load() != 1 {
		t.Fatalf("captured v1 probe calls = %d, want 1", oldCalls.Load())
	}
	if snapshot := mgr.SnapshotForTag("node@v2"); snapshot == nil || snapshot.InitialCheckDone {
		t.Fatalf("old generation result leaked into v2: %+v", snapshot)
	}
	if !mgr.beginTagProbe("node@v1") {
		t.Fatal("old runtime tag lease was not released")
	}
	mgr.endTagProbe("node@v1")
	mgr.RequestProbeTagsOnce([]string{"node@v2"})
	waitForInitialConvergence(t, mgr)
	if newCalls.Load() != 1 {
		t.Fatalf("new generation calls = %d, want 1", newCalls.Load())
	}
}
