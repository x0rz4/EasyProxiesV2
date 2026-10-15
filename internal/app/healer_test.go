package app

import (
	"context"
	"testing"
	"time"

	"easy_proxies/internal/group"
)

type blockingHealingBackend struct {
	group.HealingBackend
	entered chan struct{}
}

func (b *blockingHealingBackend) HealingInputs(ctx context.Context, _ time.Time) ([]group.HealInput, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestHealerShutdownCancelsAndJoinsCycle(t *testing.T) {
	b := &blockingHealingBackend{entered: make(chan struct{})}
	stop := startGroupHealer(context.Background(), b)
	<-b.entered
	done := make(chan struct{})
	go func() { stop(); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("healer shutdown did not cancel active cycle")
	}
}
