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
