package boxmgr

import (
	"context"
	"fmt"
	"slices"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
)

// HealingInputs uses the same candidate predicate as builder and normal
// membership reconciliation. Runtime evidence never broadens that candidate set.
func (m *Manager) HealingInputs(ctx context.Context, now time.Time) ([]group.HealInput, error) {
	return m.healingInputs(ctx, now, 0)
}

// HealingInput revalidates only the requested group, using the same builder
// and candidate predicate as the cycle's initial full snapshot.
func (m *Manager) HealingInput(ctx context.Context, id int64, now time.Time) (group.HealInput, error) {
	if id <= 0 {
		return group.HealInput{}, &group.DeferredHealing{Reason: "invalid group ID"}
	}
	inputs, err := m.healingInputs(ctx, now, id)
	if err != nil {
		return group.HealInput{}, err
	}
	if len(inputs) == 0 {
		return group.HealInput{}, &group.DeferredHealing{Reason: "group capacity policy removed or disabled"}
	}
	return inputs[0], nil
}

func (m *Manager) healingInputs(ctx context.Context, now time.Time, groupID int64) ([]group.HealInput, error) {
	m.mu.RLock()
	source := m.cfg
	if source != nil && groupID != 0 {
		// Only these fields feed candidate selection. Construct a fresh config
		// before cloning its slices; Config also owns a mutex that must not copy.
		scoped := &config.Config{Nodes: source.Nodes, TagNames: source.TagNames}
		if g, ok := groupConfigByID(source, groupID); ok {
			scoped.Groups = []config.GroupPoolConfig{g}
		}
		source = scoped
	}
	cfg, mon, db := source.Clone(), m.monitorMgr, m.store
	m.mu.RUnlock()
	if cfg == nil || mon == nil || len(cfg.Groups) == 0 {
		return nil, nil
	}
	var subscriptionIDs []int64
	if db != nil {
		subscriptions, err := db.ListSubscriptions(ctx)
		if err != nil {
			return nil, err
		}
		for _, sub := range subscriptions {
			if sub.Enabled {
				subscriptionIDs = append(subscriptionIDs, sub.ID)
			}
		}
	}
	health := map[int64]monitor.Snapshot{}
	for _, snapshot := range mon.Snapshot() {
		health[snapshot.NodeID] = snapshot
	}
	var runtimes map[int64]group.GroupRuntimeSnapshot
	if groupID == 0 {
		runtimes = group.GroupRuntimeSnapshotsReadonly()
	} else {
		snapshot, _ := group.GroupRuntimeSnapshotReadonly(groupID)
		runtimes = map[int64]group.GroupRuntimeSnapshot{groupID: snapshot}
	}
	inputs := []group.HealInput{}
	for _, g := range cfg.Groups {
		if !g.Enabled || g.TargetSize <= 0 {
			continue
		}
		in := group.HealInput{GroupID: g.ID, Policy: g.HealPolicy.Clone(), Now: now, MaxHealthAge: max(time.Minute, 2*g.HealthCheckInterval)}
		// Groups do not constrain subscription sources. Refresh can discover new
		// matching nodes even when the current candidate set is empty.
		in.SubscriptionIDs = slices.Clone(subscriptionIDs)
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
	in, err := m.HealingInput(ctx, groupID, time.Now())
	if err != nil {
		return err
	}
	plan, err := group.PlanHealing(in)
	if err != nil {
		return err
	}
	valid := in.Policy.Execute && slices.Equal(plan.DesiredIDs, desired)
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
	in, err := m.HealingInput(ctx, groupID, time.Now())
	if err != nil {
		return err
	}
	allowed := false
	if group.HealingActionAllowed(in, group.ProbeQuarantined) {
		plan, err := group.PlanHealing(in)
		if err != nil {
			return err
		}
		if plan.Healthy >= in.Policy.MinHealthy {
			return &group.DeferredHealing{Reason: "healthy capacity already satisfied"}
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
