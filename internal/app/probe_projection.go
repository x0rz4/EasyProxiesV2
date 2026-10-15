package app

import (
	"context"
	"log"
	"time"

	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

func startProbeProjections(parent context.Context, db store.Store, project func(context.Context, probe.Outcome) error, retag func(...int64)) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		applier := probe.New(db)
		for {
			attempt, stop := context.WithTimeout(ctx, 15*time.Second)
			err := applier.Reconcile(attempt, func(ctx context.Context, out probe.Outcome) error {
				if err := project(ctx, out); err != nil {
					return err
				}
				if out.Event.Kind != probe.Health {
					retag(out.Event.NodeID)
				}
				return nil
			})
			stop()
			if err != nil && ctx.Err() == nil {
				log.Printf("[probe] pending projection retry: %v", err)
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
