package app

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// membershipQueue retains one set of dirty nodes, not one goroutine per change.
// Failed batches stay dirty until reconciled against the latest stored facts.
type membershipQueue struct {
	ctx     context.Context
	cancel  context.CancelFunc
	apply   func(context.Context, []int64) error
	retry   time.Duration
	wake    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	pending map[int64]struct{}
}

func newMembershipQueue(parent context.Context, apply func(context.Context, []int64) error, retry time.Duration) *membershipQueue {
	ctx, cancel := context.WithCancel(parent)
	q := &membershipQueue{ctx: ctx, cancel: cancel, apply: apply, retry: retry,
		wake: make(chan struct{}, 1), done: make(chan struct{}), pending: make(map[int64]struct{})}
	go q.run()
	return q
}

func (q *membershipQueue) Enqueue(ids []int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil {
		return
	}
	for _, id := range ids {
		if id > 0 {
			q.pending[id] = struct{}{}
		}
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *membershipQueue) Close() {
	q.cancel()
	<-q.done
}

func (q *membershipQueue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		}
		for q.ctx.Err() == nil {
			q.mu.Lock()
			ids := make([]int64, 0, len(q.pending))
			for id := range q.pending {
				ids = append(ids, id)
			}
			clear(q.pending)
			q.mu.Unlock()
			if len(ids) == 0 {
				break
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			ctx, cancel := context.WithTimeout(q.ctx, 15*time.Second)
			err := q.apply(ctx, ids)
			cancel()
			if err == nil {
				continue
			}
			q.Enqueue(ids)
			if q.ctx.Err() != nil {
				return
			}
			log.Printf("[membership] reconciliation pending, retrying: %v", err)
			timer := time.NewTimer(q.retry)
			select {
			case <-q.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
