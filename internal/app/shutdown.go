package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// shutdownStack owns one cleanup goroutine for the entire application. A closer
// without a context (including embedded box.Close) cannot hold Run indefinitely.
// On timeout cleanup keeps its order: the store is never closed underneath a
// worker that has not returned. The process owner decides when to force exit.
type shutdownStack struct {
	steps  []func(context.Context) error
	once   sync.Once
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func (s *shutdownStack) add(close func(context.Context) error) { s.steps = append(s.steps, close) }

func (s *shutdownStack) close(ctx context.Context) error {
	s.once.Do(func() {
		s.done = make(chan struct{})
		s.cancel()
		go func() {
			defer close(s.done)
			for i := len(s.steps) - 1; i >= 0; i-- {
				s.err = errors.Join(s.err, s.steps[i](ctx))
			}
		}()
	})
	select {
	case <-s.done:
		return s.err
	case <-ctx.Done():
		return fmt.Errorf("application shutdown: %w", ctx.Err())
	}
}
