package group

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

type HealAudit struct {
	GroupID int64
	Status  string
	Action  HealAction
	At      time.Time
}

// HealingBackend is implemented at the application boundary. All mutations must
// revalidate current policy and candidate eligibility before performing work.
type HealingBackend interface {
	HealingInputs(context.Context, time.Time) ([]HealInput, error)
	ApplyHealingSelection(context.Context, int64, []int64) error
	ProbeHealingCandidate(context.Context, int64, int64) error
	RefreshHealingSubscription(context.Context, int64, int64) error
	AuditHealing(context.Context, HealAudit) error
}

// DeferredHealing is a policy/busy decision rather than an unsuccessful probe.
type DeferredHealing struct {
	Reason string
	Until  time.Time
}

func (d *DeferredHealing) Error() string { return d.Reason }

type healAttempt struct {
	next     time.Time
	failures int
}

// Healer serializes cycles. Budgets and dedup keys are shared by all groups,
// preventing the same quarantined node or subscription from being hammered.
type Healer struct {
	mu                sync.Mutex
	backend           HealingBackend
	now               func() time.Time
	attempts          map[string]healAttempt
	window            time.Time
	probes, refreshes int
	cursor            int
}

func NewHealer(backend HealingBackend, now func() time.Time) *Healer {
	if now == nil {
		now = time.Now
	}
	return &Healer{backend: backend, now: now, attempts: map[string]healAttempt{}}
}

func (h *Healer) Tick(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.window.IsZero() || now.Sub(h.window) >= time.Minute {
		h.window, h.probes, h.refreshes = now, 0, 0
	}
	for key, attempt := range h.attempts {
		if now.Sub(attempt.next) > time.Hour {
			delete(h.attempts, key)
		}
	}
	inputs, err := h.backend.HealingInputs(ctx, now)
	if err != nil {
		return err
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].GroupID < inputs[j].GroupID })
	var result error
	for offset := range inputs {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		in := inputs[(h.cursor+offset)%len(inputs)]
		if err := h.runGroup(ctx, in); err != nil {
			result = errors.Join(result, fmt.Errorf("heal group %d: %w", in.GroupID, err))
		}
	}
	if len(inputs) > 0 {
		h.cursor = (h.cursor + 1) % len(inputs)
	}
	return result
}

func (h *Healer) current(ctx context.Context, id int64) (HealInput, HealPlan, error) {
	inputs, err := h.backend.HealingInputs(ctx, h.now())
	if err != nil {
		return HealInput{}, HealPlan{}, err
	}
	for _, in := range inputs {
		if in.GroupID == id {
			plan, err := PlanHealing(in)
			return in, plan, err
		}
	}
	return HealInput{}, HealPlan{}, &DeferredHealing{Reason: "group capacity policy removed or disabled"}
}

func (h *Healer) audit(ctx context.Context, id int64, status string, action HealAction) error {
	return h.backend.AuditHealing(ctx, HealAudit{GroupID: id, Status: status, Action: action, At: h.now()})
}

func (h *Healer) runGroup(ctx context.Context, initial HealInput) error {
	plan, err := PlanHealing(initial)
	if err != nil {
		return err
	}
	if plan.DryRun {
		var result error
		for _, action := range plan.Actions {
			result = errors.Join(result, h.audit(ctx, initial.GroupID, "dry_run", action))
		}
		return result
	}
	var result error
	// A recovery probe can make a standby promotable; re-evaluate promotion
	// before spending subscription budget. No stage assumes an action succeeded.
	for _, kind := range []string{PromoteStandby, ProbeQuarantined, PromoteStandby, RefreshSubscription} {
		in, current, err := h.current(ctx, initial.GroupID)
		if err != nil {
			return errors.Join(result, err)
		}
		if !in.Policy.Execute {
			return result
		}
		if kind == ProbeQuarantined || kind == RefreshSubscription {
			// A failed/deferred promotion is not available capacity. Compute
			// fallback actions from observed membership without assuming promotion.
			observed := in
			observed.Policy = in.Policy.Clone()
			observed.Policy.Refill = slices.DeleteFunc(observed.Policy.Refill, func(v string) bool { return v == PromoteStandby })
			current, err = PlanHealing(observed)
			if err != nil {
				return errors.Join(result, err)
			}
		}
		for _, action := range current.Actions {
			if action.Kind != kind {
				continue
			}
			switch kind {
			case PromoteStandby:
				err = h.perform(ctx, in.GroupID, fmt.Sprintf("group:%d", in.GroupID), action, func() error { return h.backend.ApplyHealingSelection(ctx, in.GroupID, current.DesiredIDs) })
			case ProbeQuarantined:
				for _, id := range action.NodeIDs {
					one := action
					one.NodeIDs = []int64{id}
					err = errors.Join(err, h.perform(ctx, in.GroupID, fmt.Sprintf("probe:%d", id), one, func() error { return h.backend.ProbeHealingCandidate(ctx, in.GroupID, id) }))
				}
			case RefreshSubscription:
				for _, id := range action.SubscriptionIDs {
					one := action
					one.SubscriptionIDs = []int64{id}
					err = errors.Join(err, h.perform(ctx, in.GroupID, fmt.Sprintf("subscription:%d", id), one, func() error { return h.backend.RefreshHealingSubscription(ctx, in.GroupID, id) }))
				}
			}
			result = errors.Join(result, err)
		}
	}
	in, final, err := h.current(ctx, initial.GroupID)
	if err != nil {
		return errors.Join(result, err)
	}
	// Report observed active health, not a hypothetical promotion from the plan.
	if in.Policy.Execute && final.Healthy < in.Policy.MinHealthy {
		result = errors.Join(result, h.audit(ctx, in.GroupID, "alert", HealAction{Kind: CapacityAlert, Reason: fmt.Sprintf("healthy capacity %d below minimum %d", final.Healthy, in.Policy.MinHealthy)}))
	}
	return result
}

func (h *Healer) perform(ctx context.Context, groupID int64, key string, action HealAction, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := h.now()
	previous := h.attempts[key]
	reason := ""
	if now.Before(previous.next) {
		reason = "cooldown or failure backoff until " + previous.next.UTC().Format(time.RFC3339)
	}
	if action.Kind == ProbeQuarantined && h.probes >= 20 {
		reason = "global recovery probe budget exhausted (20/minute)"
	}
	if action.Kind == RefreshSubscription && h.refreshes >= 2 {
		reason = "global subscription refresh budget exhausted (2/minute)"
	}
	if reason != "" {
		action.Reason = reason
		return h.audit(ctx, groupID, "deferred", action)
	}
	// Audit failure prevents an unrecorded external action.
	if err := h.audit(ctx, groupID, "planned", action); err != nil {
		return err
	}
	if action.Kind == ProbeQuarantined {
		h.probes++
	}
	if action.Kind == RefreshSubscription {
		h.refreshes++
	}
	err := run()
	now = h.now()
	delay := time.Minute
	if action.Kind == RefreshSubscription {
		delay = 5 * time.Minute
	}
	if action.Kind == PromoteStandby {
		delay = 10 * time.Second
	}
	status := "succeeded"
	var deferred *DeferredHealing
	if errors.As(err, &deferred) {
		status, action.Reason = "deferred", deferred.Reason
		previous.next = maxTime(now.Add(delay), deferred.Until)
	} else if err != nil {
		status, action.Reason = "failed", err.Error()
		previous.failures++
		delay *= time.Duration(1 << min(previous.failures-1, 4))
		if action.Kind == RefreshSubscription && previous.failures >= 3 {
			delay = 30 * time.Minute
		}
		previous.next = now.Add(min(delay, 30*time.Minute))
	} else {
		previous.failures = 0
		previous.next = now.Add(delay)
	}
	h.attempts[key] = previous
	auditErr := h.audit(ctx, groupID, status, action)
	if deferred != nil {
		return auditErr
	}
	return errors.Join(err, auditErr)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// HealingActionAllowed revalidates refresh authorization against a fresh input.
func HealingActionAllowed(in HealInput, action string) bool {
	return in.Policy.Execute && in.Policy.TargetSize > 0 && slices.Contains(in.Policy.Refill, action)
}
