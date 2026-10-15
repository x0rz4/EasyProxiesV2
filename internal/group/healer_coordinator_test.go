package group

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type fakeHealingBackend struct {
	inputs                             []HealInput
	audits                             []HealAudit
	selections, probes, refreshes      int
	selectionErr, refreshErr, auditErr error
	recoverOnProbe                     bool
}

func (b *fakeHealingBackend) HealingInputs(_ context.Context, now time.Time) ([]HealInput, error) {
	result := slices.Clone(b.inputs)
	for i := range result {
		result[i].Now = now
		result[i].Candidates = slices.Clone(result[i].Candidates)
	}
	return result, nil
}
func (b *fakeHealingBackend) ApplyHealingSelection(_ context.Context, groupID int64, ids []int64) error {
	b.selections++
	if b.selectionErr != nil {
		return b.selectionErr
	}
	for i := range b.inputs {
		if b.inputs[i].GroupID == groupID {
			for j := range b.inputs[i].Candidates {
				n := &b.inputs[i].Candidates[j]
				n.Active = slices.Contains(ids, n.NodeID)
			}
		}
	}
	return nil
}
func (b *fakeHealingBackend) ProbeHealingCandidate(_ context.Context, _ int64, nodeID int64) error {
	b.probes++
	if b.recoverOnProbe {
		for i := range b.inputs {
			for j := range b.inputs[i].Candidates {
				n := &b.inputs[i].Candidates[j]
				if n.NodeID == nodeID {
					n.Status = "ALIVE"
					n.Available = true
					n.InitialCheckDone = true
				}
			}
		}
	}
	return nil
}
func (b *fakeHealingBackend) RefreshHealingSubscription(context.Context, int64, int64) error {
	b.refreshes++
	return b.refreshErr
}
func (b *fakeHealingBackend) AuditHealing(_ context.Context, event HealAudit) error {
	b.audits = append(b.audits, event)
	return b.auditErr
}

func healingFixture(now time.Time) HealInput {
	return HealInput{GroupID: 1, Now: now, MaxHealthAge: time.Hour,
		Policy:     HealPolicy{Execute: true, MinHealthy: 1, TargetSize: 1, Refill: []string{PromoteStandby, ProbeQuarantined, RefreshSubscription}},
		Candidates: []HealCandidate{{NodeID: 1, Active: true, Status: "EVICTED", RecoveryEnabled: true, HealthCheckedAt: now}}, SubscriptionIDs: []int64{1}}
}

func TestHealerDryRunDoesNotConsumeBudgetsOrPerformActions(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := healingFixture(now)
	in.Policy.Execute = false
	b := &fakeHealingBackend{inputs: []HealInput{in}}
	h := NewHealer(b, func() time.Time { return now })
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.audits) == 0 || b.selections+b.probes+b.refreshes != 0 || h.probes+h.refreshes != 0 || len(h.attempts) != 0 {
		t.Fatalf("dry-run changed state: %+v", b)
	}
	for _, event := range b.audits {
		if event.Status != "dry_run" {
			t.Fatalf("event=%+v", event)
		}
	}
}

func TestHealerRechecksRecoveryBeforeRefreshing(t *testing.T) {
	now := time.Unix(1700000000, 0)
	b := &fakeHealingBackend{inputs: []HealInput{healingFixture(now)}, recoverOnProbe: true}
	h := NewHealer(b, func() time.Time { return now })
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.probes != 1 || b.refreshes != 0 {
		t.Fatalf("unnecessary refresh after recovery: %+v", b)
	}
	for _, event := range b.audits {
		if event.Status == "alert" {
			t.Fatal("recovered group incorrectly alerted")
		}
	}
}

func TestHealerFailedPromotionFallsBackAndAlerts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := healingFixture(now)
	in.Candidates = append(in.Candidates, HealCandidate{NodeID: 2, Status: "ALIVE", Available: true, InitialCheckDone: true, HealthCheckedAt: now})
	b := &fakeHealingBackend{inputs: []HealInput{in}, selectionErr: errors.New("prepare failed")}
	h := NewHealer(b, func() time.Time { return now })
	if err := h.Tick(context.Background()); err == nil {
		t.Fatal("failed selection was hidden")
	}
	if b.selections != 1 || b.probes != 1 || b.refreshes != 1 {
		t.Fatalf("fallback skipped or repeated selection: %+v", b)
	}
	if !slices.ContainsFunc(b.audits, func(e HealAudit) bool { return e.Status == "alert" }) {
		t.Fatal("missing observed capacity alert")
	}
}

func TestHealerSharesDedupAndBudgetAcrossGroups(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := healingFixture(now)
	in.Candidates = nil
	for id := int64(1); id <= 25; id++ {
		in.Candidates = append(in.Candidates, HealCandidate{NodeID: id, Active: true, Status: "EVICTED", RecoveryEnabled: true})
	}
	in.Policy.TargetSize = 25
	in.SubscriptionIDs = []int64{1, 2, 3}
	other := in
	other.GroupID = 2
	b := &fakeHealingBackend{inputs: []HealInput{in, other}}
	h := NewHealer(b, func() time.Time { return now })
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.probes != 20 || b.refreshes != 2 {
		t.Fatalf("global budgets bypassed: probes=%d refreshes=%d", b.probes, b.refreshes)
	}
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.probes != 20 || b.refreshes != 2 {
		t.Fatal("same-minute work repeated")
	}
}

func TestHealerRefreshCircuitAndAuditFailure(t *testing.T) {
	now := time.Unix(1700000000, 0)
	in := healingFixture(now)
	in.Policy.Refill = []string{RefreshSubscription}
	b := &fakeHealingBackend{inputs: []HealInput{in}, refreshErr: errors.New("upstream failed")}
	h := NewHealer(b, func() time.Time { return now })
	for _, advance := range []time.Duration{0, 5 * time.Minute, 10 * time.Minute} {
		now = now.Add(advance)
		if err := h.Tick(context.Background()); err == nil {
			t.Fatal("failure hidden")
		}
	}
	if b.refreshes != 3 {
		t.Fatalf("refreshes=%d", b.refreshes)
	}
	now = now.Add(20 * time.Minute)
	if err := h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.refreshes != 3 {
		t.Fatal("circuit retried before 30 minutes")
	}
	b.auditErr = errors.New("disk full")
	now = now.Add(11 * time.Minute)
	if err := h.Tick(context.Background()); err == nil {
		t.Fatal("audit failure hidden")
	}
	if b.refreshes != 3 {
		t.Fatal("unrecorded action executed")
	}
}
