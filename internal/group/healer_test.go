package group

import (
	"reflect"
	"testing"
	"time"
)

func TestPlanHealingPromotesFreshStandbyWithoutMutatingEvidence(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := HealInput{GroupID: 7, Now: now, MaxHealthAge: time.Minute,
		Policy: HealPolicy{MinHealthy: 2, TargetSize: 2, Refill: []string{PromoteStandby, ProbeQuarantined, RefreshSubscription}},
		Candidates: []HealCandidate{
			{NodeID: 1, Active: true, Status: "EVICTED", RecoveryEnabled: true},
			{NodeID: 2, Active: true, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now},
			{NodeID: 3, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now},
			{NodeID: 4, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now.Add(-2 * time.Minute)},
		}, SubscriptionIDs: []int64{5}}
	before := append([]HealCandidate(nil), in.Candidates...)
	plan, err := PlanHealing(in)
	if err != nil || !plan.DryRun || plan.Healthy != 1 || plan.Projected != 2 || plan.Deficit != 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if !reflect.DeepEqual(plan.DesiredIDs, []int64{2, 3}) || len(plan.Actions) != 1 || !reflect.DeepEqual(plan.Actions[0].RemovedIDs, []int64{1}) {
		t.Fatalf("promotion=%+v", plan)
	}
	if !reflect.DeepEqual(before, in.Candidates) {
		t.Fatal("planner modified caller evidence")
	}
	in.Policy.Execute = true
	executing, err := PlanHealing(in)
	if err != nil || executing.DryRun || !reflect.DeepEqual(executing.Actions, plan.Actions) {
		t.Fatalf("execution flag changed planning: %+v, %v", executing, err)
	}
}

func TestPlanHealingDoesNotAssumeRecoveryOrRefreshSuccess(t *testing.T) {
	now := time.Unix(1700000000, 0)
	plan, err := PlanHealing(HealInput{Now: now, MaxHealthAge: time.Minute,
		Policy: HealPolicy{MinHealthy: 2, TargetSize: 3, Refill: []string{PromoteStandby, ProbeQuarantined, RefreshSubscription}},
		Candidates: []HealCandidate{
			{NodeID: 1, Active: true, Status: "EVICTED", RecoveryEnabled: true, RecoveryCooldownEnd: now},
			{NodeID: 2, Active: true, Status: "EVICTED", RecoveryEnabled: true, RecoveryCooldownEnd: now.Add(time.Second)},
			{NodeID: 3, Active: true, Status: "EVICTED", RecoveryEnabled: false},
			{NodeID: 4, Status: "ALIVE", Available: true, InitialCheckDone: true, Provisional: true, HealthCheckedAt: now},
			{NodeID: 5, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now.Add(time.Second)},
		}, SubscriptionIDs: []int64{9, 9, -1, 8}})
	if err != nil || plan.Projected != 0 || plan.Deficit != 2 || len(plan.Actions) != 3 {
		t.Fatalf("plan=%+v, %v", plan, err)
	}
	if plan.Actions[0].Kind != ProbeQuarantined || !reflect.DeepEqual(plan.Actions[0].NodeIDs, []int64{1}) {
		t.Fatalf("recovery policy bypassed: %+v", plan.Actions)
	}
	if !reflect.DeepEqual(plan.Actions[1].SubscriptionIDs, []int64{8, 9}) || plan.Actions[2].Kind != CapacityAlert {
		t.Fatalf("refresh/alert=%+v", plan.Actions)
	}
}

func TestPlanHealingStableSelectionAndDisabledPromotion(t *testing.T) {
	now := time.Unix(1700000000, 0)
	candidates := []HealCandidate{
		{NodeID: 3, Active: true, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now},
		{NodeID: 1, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now},
		{NodeID: 2, Active: true, Status: "SUSPECT"},
	}
	in := HealInput{Now: now, MaxHealthAge: time.Minute, Candidates: candidates,
		Policy: HealPolicy{MinHealthy: 1, TargetSize: 2, Refill: []string{PromoteStandby}}}
	plan, err := PlanHealing(in)
	if err != nil || len(plan.Actions) != 0 || !reflect.DeepEqual(plan.DesiredIDs, []int64{2, 3}) {
		t.Fatalf("sufficient pool churned: %+v %v", plan, err)
	}
	in.Policy.MinHealthy = 2
	in.Policy.Refill = nil
	plan, err = PlanHealing(in)
	if err != nil || !reflect.DeepEqual(plan.DesiredIDs, []int64{2, 3}) || plan.Deficit != 1 {
		t.Fatalf("disabled promotion ran: %+v %v", plan, err)
	}
	in.Policy.TargetSize, in.Policy.MinHealthy = 0, 0
	plan, err = PlanHealing(in)
	if err != nil || len(plan.Actions) != 0 || len(plan.DesiredIDs) != 0 {
		t.Fatalf("disabled capacity planned actions: %+v %v", plan, err)
	}
}

func TestPlanHealingRejectsInvalidInputs(t *testing.T) {
	for _, policy := range []HealPolicy{
		{MinHealthy: 2, TargetSize: 1}, {TargetSize: -1},
		{Refill: []string{"relax_region"}}, {Refill: []string{PromoteStandby, PromoteStandby}},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("accepted policy %+v", policy)
		}
	}
	_, err := PlanHealing(HealInput{Policy: HealPolicy{TargetSize: 1}, Now: time.Unix(1, 0), MaxHealthAge: time.Minute,
		Candidates: []HealCandidate{{NodeID: 1}, {NodeID: 1}}})
	if err == nil {
		t.Fatal("accepted duplicate candidate")
	}
}

func TestCapacitySelectionPreservesSlotsAndExplicitEmpty(t *testing.T) {
	for _, tc := range []struct{ previous, want []int64 }{
		{nil, []int64{1, 2}}, {[]int64{}, []int64{}}, {[]int64{3, 99}, []int64{3}},
	} {
		if got := CapacitySelection([]int64{3, 1, 2}, tc.previous, 2); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("selection=%v want=%v", got, tc.want)
		}
	}
}

func TestStandbyRecoveryPreservesStreakAcrossCapacityChange(t *testing.T) {
	Reset()
	defer Reset()
	policy := recoverablePolicy(2)
	states := map[string]GroupInitialState{"a": {NodeID: 1}, "b": {NodeID: 2, Standby: true}}
	Register(993, time.Minute, 1, "a", states, policy)
	RecordFailure(993, "a", nil, time.Now())
	RecordHealthSuccess(993, "a", "")
	states["a"] = GroupInitialState{NodeID: 1, Standby: true}
	states["b"] = GroupInitialState{NodeID: 2}
	NotifyEvents(ReconcileSilent(993, RuntimeUpdate{FailureWindow: time.Minute, FailureThreshold: 1, Members: states, Recovery: policy}))
	if !RecordHealthSuccess(993, "a", "") {
		t.Fatal("capacity update reset recovery progress")
	}
	if MemberAvailable(993, "a") {
		t.Fatal("recovery admitted standby to traffic")
	}
	for _, n := range GroupRuntimeSnapshots()[993].Members {
		if n.NodeID == 1 && (n.Status != "ALIVE" || !n.Standby) {
			t.Fatalf("standby failed to recover: %+v", n)
		}
	}
}

func TestHealingSnapshotReadDoesNotPruneOrEmitEvents(t *testing.T) {
	Reset()
	defer Reset()
	Register(994, time.Minute, 3, "", map[string]GroupInitialState{"a": {NodeID: 1}})
	value, _ := groupRuntimeStore.Load(int64(994))
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	runtime.members["a"].failureHistory = []int64{time.Now().Add(-2 * time.Minute).UnixNano()}
	runtime.mu.Unlock()
	before := GroupRuntimeSnapshotsReadonly()[994].Members[0]
	after := GroupRuntimeSnapshotsReadonly()[994].Members[0]
	if before.FailureCount != 1 || after.FailureCount != 1 {
		t.Fatalf("read pruned state: before=%+v after=%+v", before, after)
	}
}
