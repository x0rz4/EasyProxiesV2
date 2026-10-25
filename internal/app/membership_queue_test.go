package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMembershipQueueMergesPendingAndRetriesFailures(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	applied := make(chan []int64, 1)
	calls := 0
	q := newMembershipQueue(t.Context(), func(ctx context.Context, ids []int64) error {
		calls++
		if calls == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return errors.New("transient failure")
		}
		applied <- ids
		return nil
	}, 0)
	defer q.Close()
	q.Enqueue([]int64{1})
	<-started
	q.Enqueue([]int64{2, 2, 3, -1})
	close(release)
	if ids := <-applied; !reflect.DeepEqual(ids, []int64{1, 2, 3}) {
		t.Fatalf("retried nodes=%v", ids)
	}
}

func TestMembershipQueueCloseCancelsRunningApply(t *testing.T) {
	started := make(chan struct{})
	q := newMembershipQueue(t.Context(), func(ctx context.Context, _ []int64) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, 0)
	q.Enqueue([]int64{1})
	<-started
	q.Close()
	q.Close()
	q.Enqueue([]int64{2})
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 0 {
		t.Fatalf("closed queue retained %v", q.pending)
	}
}
