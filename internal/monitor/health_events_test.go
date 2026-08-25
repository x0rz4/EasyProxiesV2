package monitor

import (
	"context"
	"errors"
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
	if !mgr.probeDue("node-a", lastCheck, now) {
		t.Fatal("group member was not due at its shorter interval")
	}
	if mgr.probeDue("node-b", lastCheck, now) {
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
