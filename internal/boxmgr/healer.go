package boxmgr

import (
	"context"
	"fmt"
	"slices"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
)

// HealingInputs uses the same candidate predicate as builder and normal
// membership reconciliation. Runtime evidence never broadens that candidate set.
func (m *Manager) HealingInputs(ctx context.Context, now time.Time) ([]group.HealInput, error) {
	m.mu.RLock()
	cfg, mon, db := m.cfg.Clone(), m.monitorMgr, m.store
	m.mu.RUnlock()
	if cfg == nil || mon == nil {
		return nil, nil
	}
	subscriptions := map[int64][]int64{}
	if db != nil {
		nodes, err := db.ListManagedNodes(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			subscriptions[node.ID] = node.SubscriptionIDs
		}
	}
	health := map[int64]monitor.Snapshot{}
	for _, snapshot := range mon.Snapshot() {
		health[snapshot.NodeID] = snapshot
	}
	runtimes := group.GroupRuntimeSnapshots()
	inputs := []group.HealInput{}
	for _, g := range cfg.Groups {
		if !g.Enabled || g.TargetSize <= 0 {
			continue
		}
		in := group.HealInput{GroupID: g.ID, Policy: g.HealPolicy.Clone(), Now: now, MaxHealthAge: max(time.Minute, 2*g.HealthCheckInterval)}
		members := map[int64]group.GroupMemberSnapshot{}
		for _, member := range runtimes[g.ID].Members {
			members[member.NodeID] = member
		}
		for _, node := range groupMemberNodes(cfg, g) {
			member, present := members[node.ID]
			h := health[node.ID]
			status := member.Status
			if !present {
				status = "UNKNOWN"
			}
			in.Candidates = append(in.Candidates, group.HealCandidate{NodeID: node.ID, Active: present && !member.Standby,
				Status: status, Available: h.Available && !h.Blacklisted, InitialCheckDone: h.InitialCheckDone,
				Provisional: h.Provisional, HealthCheckedAt: h.HealthCheckedAt,
				RecoveryEnabled: g.RecoveryEnabled, RecoveryCooldownEnd: member.RecoveryCooldownEnd})
			in.SubscriptionIDs = append(in.SubscriptionIDs, subscriptions[node.ID]...)
		}
		inputs = append(inputs, in)
	}
	return inputs, nil
}

// ApplyHealingSelection serializes with normal group updates, rechecks the plan,
// and publishes through PrepareUpdate/Commit. No listener or base box restarts.
func (m *Manager) ApplyHealingSelection(ctx context.Context, groupID int64, desired []int64) error {
	slot := m.groupSlot(groupID)
	if err := acquireGroupSlot(ctx, slot); err != nil {
		return err
	}
	defer releaseGroupSlot(slot)
	inputs, err := m.HealingInputs(ctx, time.Now())
	if err != nil {
		return err
	}
	valid := false
	for _, in := range inputs {
		if in.GroupID != groupID || !in.Policy.Execute {
			continue
		}
		plan, err := group.PlanHealing(in)
		if err != nil {
			return err
		}
		valid = slices.Equal(plan.DesiredIDs, desired)
	}
	if !valid {
		return fmt.Errorf("group %d healing plan changed", groupID)
	}
	m.mu.RLock()
	cfg := m.cfg.Clone()
	m.mu.RUnlock()
	g, ok := groupConfigByID(cfg, groupID)
	if !ok || !g.Enabled || !g.Execute {
		return fmt.Errorf("group %d capacity execution disabled", groupID)
	}
	slot.mu.RLock()
	instance := slot.box
	slot.mu.RUnlock()
	if instance == nil {
		return fmt.Errorf("group %d runtime unavailable", groupID)
	}
	return m.applyGroupRuntimeIncremental(cfg, g, slot, instance, groupAppliedFingerprint(cfg, g), desired)
}

// ProbeHealingCandidate uses the normal health pipeline; it neither clears an
// eviction nor bypasses the group's recovery success threshold or cooldown.
func (m *Manager) ProbeHealingCandidate(ctx context.Context, groupID, nodeID int64) error {
	inputs, err := m.HealingInputs(ctx, time.Now())
	if err != nil {
		return err
	}
	allowed := false
	for _, in := range inputs {
		if in.GroupID != groupID || !in.Policy.Execute || !slices.Contains(in.Policy.Refill, group.ProbeQuarantined) {
			continue
		}
		for _, n := range in.Candidates {
			if n.NodeID == nodeID && n.Status == "EVICTED" && n.RecoveryEnabled && !in.Now.Before(n.RecoveryCooldownEnd) {
				allowed = true
			}
		}
	}
	if !allowed {
		return fmt.Errorf("node %d no longer eligible for recovery probe", nodeID)
	}
	m.mu.RLock()
	mon := m.monitorMgr
	m.mu.RUnlock()
	if mon == nil {
		return fmt.Errorf("health manager unavailable")
	}
	snap := mon.SnapshotForNodeID(nodeID)
	if snap == nil {
		return fmt.Errorf("node %d runtime unavailable", nodeID)
	}
	_, err = mon.Probe(ctx, snap.Tag)
	return err
}
