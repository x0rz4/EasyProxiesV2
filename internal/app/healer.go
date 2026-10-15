package app

import (
	"context"
	"log"
	"slices"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
	"easy_proxies/internal/subscription"
)

type healingBackend struct {
	*boxmgr.Manager
	db            store.Store
	subscriptions *subscription.Manager
}

func (b *healingBackend) AuditHealing(ctx context.Context, event group.HealAudit) error {
	_, err := b.db.AppendGroupHealEvent(ctx, store.GroupHealEvent{GroupID: event.GroupID, Status: event.Status, Action: event.Action, CreatedAt: event.At}, 5*time.Minute)
	return err
}

func (b *healingBackend) RefreshHealingSubscription(ctx context.Context, groupID, subscriptionID int64) error {
	inputs, err := b.HealingInputs(ctx, time.Now())
	if err != nil {
		return err
	}
	for _, in := range inputs {
		if in.GroupID != groupID || !group.HealingActionAllowed(in, group.RefreshSubscription) || !slices.Contains(in.SubscriptionIDs, subscriptionID) {
			continue
		}
		plan, err := group.PlanHealing(in)
		if err != nil {
			return err
		}
		if plan.Healthy >= in.Policy.MinHealthy {
			return &group.DeferredHealing{Reason: "healthy capacity already satisfied"}
		}
		attempt, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return b.subscriptions.RefreshForHealing(attempt, subscriptionID)
	}
	return &group.DeferredHealing{Reason: "refresh no longer authorized by current group policy"}
}

func startGroupHealer(parent context.Context, backend group.HealingBackend) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	healer := group.NewHealer(backend, nil)
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			attempt, stop := context.WithTimeout(ctx, 2*time.Minute)
			err := healer.Tick(attempt)
			stop()
			if err != nil && ctx.Err() == nil {
				log.Printf("[healer] reconcile: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
