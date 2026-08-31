package monitor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbePublishesCommittedHealthResults(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "example.com:80"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	entry := mgr.Register(NodeInfo{Tag: "node-a", Name: "A"})
	events := make(chan HealthResultEvent, 2)
	unsubscribe := mgr.SubscribeHealthResults(func(event HealthResultEvent) { events <- event })
	defer unsubscribe()

	entry.SetProbe(func(context.Context) (time.Duration, error) { return 25 * time.Millisecond, nil })
	if _, err := mgr.Probe(context.Background(), "node-a"); err != nil {
		t.Fatal(err)
	}
	success := <-events
	if !success.Success || success.Tag != "node-a" || success.Latency != 25*time.Millisecond {
		t.Fatalf("success event = %+v", success)
	}
	snapshot := mgr.SnapshotForTag("node-a")
	if snapshot == nil || !snapshot.InitialCheckDone || !snapshot.Available || snapshot.LastLatencyMs != 25 {
		t.Fatalf("snapshot was not committed before event: %+v", snapshot)
	}

	probeErr := errors.New("probe failed")
	entry.SetProbe(func(context.Context) (time.Duration, error) { return 0, probeErr })
	if _, err := mgr.Probe(context.Background(), "node-a"); !errors.Is(err, probeErr) {
		t.Fatalf("probe error = %v", err)
	}
	failure := <-events
	if failure.Success || failure.Error != probeErr.Error() {
		t.Fatalf("failure event = %+v", failure)
	}
	snapshot = mgr.SnapshotForTag("node-a")
	if snapshot == nil || snapshot.Available || snapshot.LastLatencyMs != -1 {
		t.Fatalf("failed snapshot = %+v", snapshot)
	}
}

func TestHealthResultsDispatchOnlyToMatchingSubscriptions(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	var global, matching, unrelated atomic.Int32
	unsubscribeGlobal := mgr.SubscribeHealthResults(func(HealthResultEvent) { global.Add(1) })
	unsubscribeMatching := mgr.SubscribeHealthResultsFor([]string{"node-a"}, []int64{7}, func(HealthResultEvent) { matching.Add(1) })
	unsubscribeUnrelated := mgr.SubscribeHealthResultsFor([]string{"node-b"}, []int64{8}, func(HealthResultEvent) { unrelated.Add(1) })
	defer unsubscribeGlobal()
	defer unsubscribeMatching()
	defer unsubscribeUnrelated()

	// Matching both indexes must still invoke the subscription exactly once.
	mgr.publishHealthResult(HealthResultEvent{Tag: "node-a", NodeID: 7, Success: true})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (global.Load() != 1 || matching.Load() != 1) {
		time.Sleep(time.Millisecond)
	}
	if global.Load() != 1 || matching.Load() != 1 || unrelated.Load() != 0 {
		t.Fatalf("first dispatch: global=%d matching=%d unrelated=%d", global.Load(), matching.Load(), unrelated.Load())
	}
	mgr.publishHealthResult(HealthResultEvent{Tag: "node-c", NodeID: 9, Success: true})
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && global.Load() != 2 {
		time.Sleep(time.Millisecond)
	}
	if global.Load() != 2 || matching.Load() != 1 || unrelated.Load() != 0 {
		t.Fatalf("second dispatch: global=%d matching=%d unrelated=%d", global.Load(), matching.Load(), unrelated.Load())
	}
}

func TestRestorePersistedHealthRemainsPendingAndProvisional(t *testing.T) {
	mgr, err := NewManager(Config{StartupAvailabilityPolicy: StartupAvailabilityOptimistic})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(NodeInfo{NodeID: 7, Tag: "restored"})
	updatedAt := time.Now().Add(-time.Hour)
	if restored := mgr.RestorePersistedHealth(map[int64]PersistedHealthState{7: {NodeID: 7, Available: false, LastLatencyMs: 42, SuccessCount: 9, UpdatedAt: updatedAt}}); restored != 1 {
		t.Fatalf("restored = %d", restored)
	}
	snapshot := mgr.SnapshotForNodeID(7)
	if snapshot == nil || snapshot.InitialCheckDone || !snapshot.Provisional || !snapshot.RoutingEligible || snapshot.HealthSource != "persisted" || snapshot.LastLatencyMs != 42 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	mgr.SetStartupAvailabilityPolicy(StartupAvailabilityStrict)
	snapshot = mgr.SnapshotForNodeID(7)
	if snapshot.Provisional || snapshot.RoutingEligible {
		t.Fatalf("strict restored snapshot = %+v", snapshot)
	}
}

func TestProbeDueUsesShortestScheduleForMemberOnly(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.healthMu.Lock()
	mgr.healthInterval = 10 * time.Minute
	mgr.healthMu.Unlock()
	mgr.RegisterGroupHealthSchedule(1, []string{"node-a"}, 2*time.Minute)

	now := time.Now()
	lastCheck := now.Add(-3 * time.Minute)
	if !mgr.probeDue("node-a", 0, lastCheck, now) {
		t.Fatal("group member was not due at its shorter interval")
	}
	if mgr.probeDue("node-b", 0, lastCheck, now) {
		t.Fatal("unrelated node inherited another group's interval")
	}
}

func TestBeginReloadPreservesIndependentGroupSchedules(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.RegisterGroupHealthSchedule(7, []string{"node-a"}, 2*time.Minute)
	mgr.BeginReload()
	mgr.groupScheduleMu.RLock()
	_, exists := mgr.groupSchedules[7]
	mgr.groupScheduleMu.RUnlock()
	if !exists {
		t.Fatal("base reload cleared an independently running group's health schedule")
	}
}

func TestMigrateRuntimeTagPreservesHistoryWithoutDuplicate(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	entry := mgr.Register(NodeInfo{NodeID: 7, Tag: "node@v1", Name: "old"})
	entry.RecordSuccessWithLatency(23 * time.Millisecond)
	entry.AddTraffic(100, 200)
	migrated := mgr.MigrateRuntimeTag(7, NodeInfo{NodeID: 7, Tag: "node@v2", Name: "new"})
	if migrated == nil {
		t.Fatal("migration returned no handle")
	}
	if mgr.SnapshotForTag("node@v1") != nil {
		t.Fatal("old runtime tag remains visible")
	}
	snapshots := mgr.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Tag != "node@v2" || snapshots[0].Name != "new" {
		t.Fatalf("migrated snapshots = %+v", snapshots)
	}
	if snapshots[0].SuccessCount != 1 || snapshots[0].TotalUpload != 100 || snapshots[0].TotalDownload != 200 || snapshots[0].LastLatencyMs != 23 {
		t.Fatalf("migration lost history: %+v", snapshots[0])
	}
	if snapshots[0].InitialCheckDone || snapshots[0].Available {
		t.Fatalf("new concrete runtime inherited availability: %+v", snapshots[0])
	}
	if snapshots[0].HealthSource != "previous_generation" {
		t.Fatalf("migrated health source = %q", snapshots[0].HealthSource)
	}
}

func TestOldGroupScheduleCleanupDoesNotDeleteNewGeneration(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	cleanupOld := mgr.RegisterGroupHealthSchedule(8, []string{"old"}, 2*time.Minute)
	cleanupNew := mgr.RegisterGroupHealthSchedule(8, []string{"new"}, time.Minute)
	cleanupOld()
	mgr.groupScheduleMu.RLock()
	schedule, exists := mgr.groupSchedules[8]
	mgr.groupScheduleMu.RUnlock()
	if !exists {
		t.Fatal("old runtime cleanup removed the new generation's health schedule")
	}
	if _, ok := schedule.tags["new"]; !ok {
		t.Fatalf("active schedule = %+v, want new generation", schedule)
	}
	cleanupNew()
	mgr.groupScheduleMu.RLock()
	_, exists = mgr.groupSchedules[8]
	mgr.groupScheduleMu.RUnlock()
	if exists {
		t.Fatal("active generation cleanup did not remove its schedule")
	}
}
