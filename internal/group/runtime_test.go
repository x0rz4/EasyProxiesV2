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

// recoverablePolicy is the permissive policy used by the recovery tests: no
// cooldown, so consecutive successes are counted immediately.
func recoverablePolicy(successes int) RecoveryPolicy {
	return RecoveryPolicy{Enabled: true, SuccessThreshold: successes, Cooldown: 0}
}

func TestEvictedMemberRecoversAfterConsecutiveSuccesses(t *testing.T) {
	Reset()
	defer Reset()
	members := map[string]GroupInitialState{"node-a": {NodeID: 42}}
	Register(301, time.Minute, 1, "node-a", members, recoverablePolicy(2))
	Register(302, time.Minute, 1, "node-a", members, recoverablePolicy(2))

	if !RecordFailure(301, "node-a", errors.New("down"), time.Now()) {
		t.Fatal("member was not evicted")
	}
	if MemberAvailable(301, "node-a") || MemberAvailable(302, "node-a") {
		t.Fatal("eviction did not propagate")
	}

	if RecordHealthSuccess(301, "node-a") {
		t.Fatal("member recovered before the success threshold was reached")
	}
	progress := GroupRuntimeSnapshots()[301].Members[0]
	if progress.Status != "EVICTED" || progress.RecoverySuccesses != 1 || progress.RecoveryThreshold != 2 {
		t.Fatalf("recovery progress was not exposed: %+v", progress)
	}

	if !RecordHealthSuccess(301, "node-a") {
		t.Fatal("member did not recover after the success threshold was reached")
	}
	if !MemberAvailable(301, "node-a") || !MemberAvailable(302, "node-a") {
		t.Fatal("automatic recovery did not clear the eviction in every group")
	}
	recovered := GroupRuntimeSnapshots()[301].Members[0]
	if recovered.Status != "ALIVE" || recovered.RecoverySuccesses != 0 || !recovered.EvictedAt.IsZero() {
		t.Fatalf("recovered member kept eviction state: %+v", recovered)
	}
}

func TestRecoveryCooldownIgnoresEarlySuccesses(t *testing.T) {
	Reset()
	defer Reset()
	Register(303, time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 43}},
		RecoveryPolicy{Enabled: true, SuccessThreshold: 1, Cooldown: time.Hour})

	RecordFailure(303, "node-a", errors.New("down"), time.Now())
	for i := 0; i < 3; i++ {
		if RecordHealthSuccess(303, "node-a") {
			t.Fatal("member recovered during the cooldown")
		}
	}
	if MemberAvailable(303, "node-a") {
		t.Fatal("cooldown did not keep the member out of rotation")
	}
	if snapshot := GroupRuntimeSnapshots()[303].Members[0]; snapshot.RecoverySuccesses != 0 || snapshot.RecoveryCooldownEnd.IsZero() {
		t.Fatalf("cooldown state was not exposed: %+v", snapshot)
	}
}

func TestRecoveryDisabledStillRequiresManualRestore(t *testing.T) {
	Reset()
	defer Reset()
	Register(304, time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 44}},
		RecoveryPolicy{Enabled: false, SuccessThreshold: 1, Cooldown: 0})

	RecordFailure(304, "node-a", errors.New("down"), time.Now())
	for i := 0; i < 3; i++ {
		if RecordHealthSuccess(304, "node-a") {
			t.Fatal("disabled recovery re-admitted the member")
		}
	}
	if MemberAvailable(304, "node-a") {
		t.Fatal("disabled recovery re-admitted the member")
	}
	if err := RestoreGroupMember(304, 44); err != nil {
		t.Fatalf("manual restore: %v", err)
	}
	if !MemberAvailable(304, "node-a") {
		t.Fatal("manual restore did not re-admit the member")
	}
}

func TestObservedFailureResetsRecoveryStreak(t *testing.T) {
	Reset()
	defer Reset()
	Register(305, time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 45}}, recoverablePolicy(2))

	RecordFailure(305, "node-a", errors.New("down"), time.Now())
	RecordHealthSuccess(305, "node-a")
	// A failure observed while evicted invalidates the streak, so the member
	// must earn a full new run of successes.
	RecordFailure(305, "node-a", errors.New("still down"), time.Now())
	if progress := GroupRuntimeSnapshots()[305].Members[0]; progress.RecoverySuccesses != 0 {
		t.Fatalf("failure did not reset the streak: %+v", progress)
	}
	if RecordHealthSuccess(305, "node-a") {
		t.Fatal("member recovered from a single success after the streak was reset")
	}
	if !RecordHealthSuccess(305, "node-a") {
		t.Fatal("member did not recover after a fresh run of successes")
	}
}

func TestReconcileSilentPrunesGlobalEvictionForFullyRemovedNode(t *testing.T) {
	Reset()
	defer Reset()
	now := time.Now()
	// Node 42 is a member of group 1 and gets evicted there; the eviction
	// propagates to group 2 as well (global mark).
	Register(401, 5*time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 42}})
	Register(402, 5*time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 42}})
	if !RecordFailure(401, "node-a", errors.New("down"), now) {
		t.Fatal("member was not evicted")
	}
	if MemberAvailable(402, "node-a") {
		t.Fatal("eviction did not propagate")
	}

	// Removing the node from group 1 keeps the mark: it is still an evicted
	// member of group 2.
	ReconcileSilent(401, RuntimeUpdate{FailureWindow: 5 * time.Minute, FailureThreshold: 1,
		Members: map[string]GroupInitialState{}})
	if MemberAvailable(402, "node-a") {
		t.Fatal("mark was pruned while another group still had it evicted")
	}

	// Removing it from every group must drop the mark — it can never be probed
	// back to health there, and re-adding it later must not re-inherit the
	// stale eviction.
	ReconcileSilent(402, RuntimeUpdate{FailureWindow: 5 * time.Minute, FailureThreshold: 1,
		Members: map[string]GroupInitialState{}})

	// Re-register the node fresh; the stale global mark must not evict it.
	Register(403, 5*time.Minute, 1, "node-a", map[string]GroupInitialState{"node-a": {NodeID: 42}})
	if !MemberAvailable(403, "node-a") {
		t.Fatal("re-added node inherited a stale global eviction mark")
	}
}
