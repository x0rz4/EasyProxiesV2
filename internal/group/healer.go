package group

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Refill actions never change membership eligibility or recovery policy.
const (
	PromoteStandby      = "promote_standby"
	ProbeQuarantined    = "probe_quarantined"
	RefreshSubscription = "refresh_subscription"
	CapacityAlert       = "capacity_alert"
)

// HealPolicy is opt-in. TargetSize zero disables capacity management; Execute
// false permits only planning and audit writes, including when a target is set.
type HealPolicy struct {
	MinHealthy int      `json:"min_healthy"`
	TargetSize int      `json:"target_size"`
	Execute    bool     `json:"healing_enabled"`
	Refill     []string `json:"refill"`
}

func (p HealPolicy) Clone() HealPolicy {
	p.Refill = slices.Clone(p.Refill)
	return p
}

func (p HealPolicy) Validate() error {
	if p.MinHealthy < 0 || p.TargetSize < 0 || p.MinHealthy > p.TargetSize || p.TargetSize > 100000 {
		return fmt.Errorf("capacity requires 0 <= min_healthy <= target_size <= 100000")
	}
	seen := map[string]bool{}
	previous := -1
	for _, action := range p.Refill {
		if action != PromoteStandby && action != ProbeQuarantined && action != RefreshSubscription {
			return fmt.Errorf("unknown refill action %q", action)
		}
		if seen[action] {
			return fmt.Errorf("duplicate refill action %q", action)
		}
		seen[action] = true
		order := slices.Index([]string{PromoteStandby, ProbeQuarantined, RefreshSubscription}, action)
		if order < previous {
			return fmt.Errorf("refill order must be promote_standby, probe_quarantined, refresh_subscription")
		}
		previous = order
	}
	return nil
}

// HealCandidate must already have passed groupmember.Filter. Active means
// present in the published pool snapshot, even if currently evicted.
type HealCandidate struct {
	NodeID              int64
	Active              bool
	Status              string
	Available           bool
	InitialCheckDone    bool
	Provisional         bool
	HealthCheckedAt     time.Time
	RecoveryEnabled     bool
	RecoveryCooldownEnd time.Time
}

type HealInput struct {
	GroupID         int64
	Policy          HealPolicy
	Candidates      []HealCandidate
	SubscriptionIDs []int64
	Now             time.Time
	MaxHealthAge    time.Duration
}

type HealAction struct {
	Kind            string  `json:"kind"`
	NodeIDs         []int64 `json:"node_ids,omitempty"`
	RemovedIDs      []int64 `json:"removed_ids,omitempty"`
	SubscriptionIDs []int64 `json:"subscription_ids,omitempty"`
	Reason          string  `json:"reason"`
}

type HealPlan struct {
	GroupID    int64        `json:"group_id"`
	DryRun     bool         `json:"dry_run"`
	Healthy    int          `json:"healthy"`
	Projected  int          `json:"projected_healthy"`
	Deficit    int          `json:"deficit"`
	DesiredIDs []int64      `json:"desired_ids"`
	StandbyIDs []int64      `json:"standby_ids"`
	Actions    []HealAction `json:"actions"`
}

// PlanHealing is pure: caller-supplied time and evidence make replay exact.
// Probe and refresh actions are requests, never assumed successful in Projected.
func PlanHealing(in HealInput) (HealPlan, error) {
	plan := HealPlan{GroupID: in.GroupID, DryRun: !in.Policy.Execute}
	if err := in.Policy.Validate(); err != nil {
		return plan, err
	}
	if in.Policy.TargetSize == 0 {
		return plan, nil
	}
	if in.Now.IsZero() || in.MaxHealthAge <= 0 {
		return plan, fmt.Errorf("healing requires an explicit clock and positive health freshness")
	}
	candidates := slices.Clone(in.Candidates)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].NodeID < candidates[j].NodeID })
	seen := map[int64]bool{}
	for _, n := range candidates {
		if n.NodeID <= 0 || seen[n.NodeID] {
			return plan, fmt.Errorf("invalid or duplicate candidate %d", n.NodeID)
		}
		seen[n.NodeID] = true
	}
	healthy := func(n HealCandidate) bool {
		age := in.Now.Sub(n.HealthCheckedAt)
		return n.Status == "ALIVE" && n.Available && n.InitialCheckDone && !n.Provisional &&
			!n.HealthCheckedAt.IsZero() && age >= 0 && age <= in.MaxHealthAge
	}
	selected := map[int64]bool{}
	selectNode := func(n HealCandidate) {
		if len(selected) < in.Policy.TargetSize && !selected[n.NodeID] {
			selected[n.NodeID] = true
		}
	}
	for _, n := range candidates {
		if n.Active && healthy(n) {
			plan.Healthy++
			selectNode(n)
		}
	}
	// Keep current healthy members first. Promotion replaces unhealthy members
	// only when capacity is short; it does not churn a sufficiently healthy pool.
	short := len(selected) < in.Policy.MinHealthy
	canPromote := slices.Contains(in.Policy.Refill, PromoteStandby)
	if short && canPromote {
		for _, n := range candidates {
			if !n.Active && healthy(n) {
				selectNode(n)
			}
		}
	}
	for _, n := range candidates {
		if n.Active {
			selectNode(n)
		}
	}
	var added, removed []int64
	for _, n := range candidates {
		if selected[n.NodeID] {
			plan.DesiredIDs = append(plan.DesiredIDs, n.NodeID)
			if healthy(n) {
				plan.Projected++
			}
			if !n.Active {
				added = append(added, n.NodeID)
			}
		} else {
			plan.StandbyIDs = append(plan.StandbyIDs, n.NodeID)
			if n.Active {
				removed = append(removed, n.NodeID)
			}
		}
	}
	plan.Deficit = max(0, in.Policy.MinHealthy-plan.Projected)
	if len(added) > 0 || len(removed) > 0 {
		plan.Actions = append(plan.Actions, HealAction{Kind: PromoteStandby, NodeIDs: added, RemovedIDs: removed, Reason: "select fresh healthy members within target capacity"})
	}
	// All asynchronous recovery actions follow the configured sequence. Their
	// execution coordinator applies global deduplication, budgets and backoff.
	if plan.Deficit > 0 {
		for _, action := range in.Policy.Refill {
			switch action {
			case ProbeQuarantined:
				var ids []int64
				for _, n := range candidates {
					if n.Status == "EVICTED" && n.RecoveryEnabled && !in.Now.Before(n.RecoveryCooldownEnd) {
						ids = append(ids, n.NodeID)
					}
				}
				if len(ids) > 0 {
					plan.Actions = append(plan.Actions, HealAction{Kind: action, NodeIDs: ids, Reason: "recheck quarantined candidates after recovery cooldown"})
				}
			case RefreshSubscription:
				ids := uniquePositiveIDs(in.SubscriptionIDs)
				if len(ids) > 0 {
					plan.Actions = append(plan.Actions, HealAction{Kind: action, SubscriptionIDs: ids, Reason: "healthy capacity remains below minimum"})
				}
			}
		}
		plan.Actions = append(plan.Actions, HealAction{Kind: CapacityAlert, Reason: fmt.Sprintf("healthy capacity %d below minimum %d", plan.Projected, in.Policy.MinHealthy)})
	}
	return plan, nil
}

func uniquePositiveIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// CapacitySelection selects traffic slots within an already-filtered candidate
// set. Nil previous initializes deterministically; an explicit empty selection
// stays empty. Existing slots never admit a new node merely because one left.
func CapacitySelection(candidates, previous []int64, target int) []int64 {
	ids := uniquePositiveIDs(candidates)
	if target <= 0 {
		return ids
	}
	if previous == nil {
		return ids[:min(target, len(ids))]
	}
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	selected := make([]int64, 0, min(target, len(previous)))
	for _, id := range uniquePositiveIDs(previous) {
		if allowed[id] && len(selected) < target {
			selected = append(selected, id)
		}
	}
	return selected
}
