package group

import (
	"errors"
	"testing"
	"time"
)

func TestSlidingWindowEvictionPropagatesAndRestore(t *testing.T) {
	Reset()
	defer Reset()
	now := time.Now()
	members := map[string]GroupInitialState{"node-a": {NodeID: 42}}
	Register(1, 5*time.Minute, 3, "node-a", members)
	Register(2, 5*time.Minute, 3, "node-a", members)

	if RecordFailure(1, "node-a", errors.New("first"), now) {
		t.Fatal("evicted after first failure")
	}
	if RecordFailure(1, "node-a", errors.New("second"), now.Add(time.Minute)) {
		t.Fatal("evicted after second failure")
	}
	if !RecordFailure(1, "node-a", errors.New("third"), now.Add(2*time.Minute)) {
		t.Fatal("not evicted after third failure")
	}
	if MemberAvailable(1, "node-a") || MemberAvailable(2, "node-a") {
		t.Fatal("eviction did not propagate to every group")
	}

	if err := RestoreGroupMember(1, 42); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !MemberAvailable(1, "node-a") || !MemberAvailable(2, "node-a") {
		t.Fatal("restore did not recover every group membership")
	}
}

func TestSlidingWindowDropsExpiredFailures(t *testing.T) {
	Reset()
	defer Reset()
	now := time.Now()
	Register(9, 5*time.Minute, 3, "", map[string]GroupInitialState{
		"node-a": {NodeID: 9, FailureHistory: []int64{now.Add(-6 * time.Minute).Unix()}},
	})
	snapshot := GroupRuntimeSnapshots()[9]
	if len(snapshot.Members) != 1 || snapshot.Members[0].Status != "ALIVE" || snapshot.Members[0].FailureCount != 0 {
		t.Fatalf("expired failure was not pruned: %+v", snapshot.Members)
	}
}

func TestSuccessfulHealthCheckClearsConsecutiveFailures(t *testing.T) {
	Reset()
	defer Reset()
	now := time.Now()
	Register(10, 5*time.Minute, 3, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 10}})

	RecordFailure(10, "node-a", errors.New("first"), now)
	RecordFailure(10, "node-a", errors.New("second"), now.Add(time.Minute))
	if MemberAvailable(10, "node-a") {
		t.Fatal("suspect member remained available")
	}
	if !RecordHealthSuccess(10, "node-a") {
		t.Fatal("successful probe did not restore suspect member")
	}
	if !MemberAvailable(10, "node-a") {
		t.Fatal("member did not become available after a successful probe")
	}
	RecordFailure(10, "node-a", errors.New("third"), now.Add(2*time.Minute))
	RecordFailure(10, "node-a", errors.New("fourth"), now.Add(3*time.Minute))
	if GroupRuntimeSnapshots()[10].Members[0].Status == "EVICTED" {
		t.Fatal("failures before a successful probe were not cleared")
	}
}

func TestCurrentClearEmitsExplicitChange(t *testing.T) {
	Reset()
	defer Reset()
	events := make(chan GroupStateEvent, 4)
	SetGroupStateObserver(func(event GroupStateEvent) { events <- event })
	defer SetGroupStateObserver(nil)
	Register(11, 5*time.Minute, 3, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 11}})
	<-events // initial current

	RecordFailure(11, "node-a", errors.New("down"), time.Now())
	event := <-events
	if !event.CurrentChanged || event.CurrentNodeID != 0 {
		t.Fatalf("clear event = %+v, want CurrentChanged with node ID 0", event)
	}
	if got := CurrentTag(11); got != "" {
		t.Fatalf("current tag = %q, want empty", got)
	}
}

func TestOldRuntimeCleanupDoesNotDeleteNewGeneration(t *testing.T) {
	Reset()
	defer Reset()
	cleanupOld := Register(99, time.Minute, 3, "old", map[string]GroupInitialState{"old": {NodeID: 1}})
	cleanupNew := Register(99, time.Minute, 3, "new", map[string]GroupInitialState{"new": {NodeID: 2}})
	defer cleanupNew()
	cleanupOld()
	snapshot, ok := GroupRuntimeSnapshots()[99]
	if !ok || len(snapshot.Members) != 1 || snapshot.Members[0].NodeID != 2 {
		t.Fatalf("new runtime generation was removed: %+v, present=%v", snapshot, ok)
	}
}

func TestStateSubscribersReceiveChangesAndCanUnsubscribe(t *testing.T) {
	Reset()
	defer Reset()
	events := make(chan GroupStateEvent, 2)
	unsubscribe := SubscribeStateChanges(func(event GroupStateEvent) { events <- event })
	Register(101, time.Minute, 3, "node", map[string]GroupInitialState{"node": {NodeID: 1}})
	select {
	case event := <-events:
		if !event.CurrentChanged || event.CurrentNodeID != 1 {
			t.Fatalf("initial subscriber event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("state subscriber was not called")
	}
	unsubscribe()
	SetCurrentTag(101, "")
	select {
	case event := <-events:
		t.Fatalf("unsubscribed callback received %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestReconcileSilentMigratesStateByNodeIDAndAppliesPolicy(t *testing.T) {
	Reset()
	defer Reset()
	now := time.Now()
	Register(202, 10*time.Minute, 3, "node@v1", map[string]GroupInitialState{"node@v1": {NodeID: 7}})
	RecordFailure(202, "node@v1", errors.New("one"), now)
	RecordFailure(202, "node@v1", errors.New("two"), now.Add(time.Second))
	events := ReconcileSilent(202, RuntimeUpdate{FailureWindow: 5 * time.Minute, FailureThreshold: 2,
		PreferredNodeID: 7, Members: map[string]GroupInitialState{"node@v2": {NodeID: 7}, "new@v2": {NodeID: 8}}})
	if MemberAvailable(202, "node@v1") {
		t.Fatal("retired runtime tag remained a member")
	}
	snapshot := GroupRuntimeSnapshots()[202]
	if len(snapshot.Members) != 2 || snapshot.Members[0].NodeID != 7 || snapshot.Members[0].Status != "EVICTED" {
		t.Fatalf("state was not migrated and re-evaluated: %+v", snapshot)
	}
	if len(events) == 0 || !events[0].StateChanged {
		t.Fatalf("policy transition did not produce a deferred event: %+v", events)
	}
}
