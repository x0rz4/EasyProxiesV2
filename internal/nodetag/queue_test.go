package nodetag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is the Recomputer half of the queue's contract. Calls arrive on a
// channel rather than a slice so a test waits for work instead of sleeping for
// it; a nil element means the queue asked for a full recompute.
type recorder struct {
	calls chan []int64
	// hold blocks every Recompute until the test closes it, which is how the
	// buffer-overflow case pins the worker inside a flush.
	hold chan struct{}
	err  error
}

func newRecorder() *recorder {
	return &recorder{calls: make(chan []int64, 16)}
}

func (r *recorder) Recompute(_ context.Context, nodeIDs []int64) ([]int64, error) {
	r.calls <- append([]int64{}, nodeIDs...)
	if r.hold != nil {
		<-r.hold
	}
	return nodeIDs, r.err
}

func (r *recorder) RecomputeAll(_ context.Context) ([]int64, error) {
	r.calls <- nil
	return nil, r.err
}

func (r *recorder) next(t *testing.T) []int64 {
	t.Helper()
	select {
	case call := <-r.calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("no recompute happened")
		return nil
	}
}

// quiet asserts that nothing else runs for a while, which is how "coalesced into
// one run" is told apart from "ran once and then ran again".
func (r *recorder) quiet(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case call := <-r.calls:
		t.Fatalf("an extra recompute ran: %v", call)
	case <-time.After(window):
	}
}

func TestQueueCoalescesRepeatedRequests(t *testing.T) {
	recorder := newRecorder()
	queue := NewQueue(recorder,
		WithDebounce(50*time.Millisecond), WithSweepInterval(time.Hour))
	defer queue.Close()

	// One detection sweep touching the same node over and over is one recompute.
	for attempt := 0; attempt < 100; attempt++ {
		queue.Enqueue(7)
	}
	// A node ID that is not a node ID cannot reach the service.
	queue.Enqueue(9, 0, -1)

	assertTagIDs(t, recorder.next(t), 7, 9)
	recorder.quiet(t, 200*time.Millisecond)
}

func TestQueueLetsAFullRequestSupersedePendingNodes(t *testing.T) {
	recorder := newRecorder()
	queue := NewQueue(recorder,
		WithDebounce(50*time.Millisecond), WithSweepInterval(time.Hour))
	defer queue.Close()

	queue.Enqueue(1, 2)
	queue.EnqueueAll()
	if call := recorder.next(t); call != nil {
		t.Fatalf("want one full recompute, got %v", call)
	}
	// The pending IDs were absorbed by the full run, not run again after it.
	recorder.quiet(t, 200*time.Millisecond)
}

// TestQueueEscalatesWhenTheBufferFills is the degradation guarantee: a probe
// goroutine's send never blocks, so a full buffer has to cost work rather than
// lose a node.
func TestQueueEscalatesWhenTheBufferFills(t *testing.T) {
	recorder := newRecorder()
	recorder.hold = make(chan struct{})
	queue := NewQueue(recorder, WithDebounce(20*time.Millisecond),
		WithSweepInterval(time.Hour), WithQueueBuffer(1))
	defer queue.Close()

	queue.Enqueue(1)
	// The worker is now inside a recompute, so nothing is draining the buffer.
	assertTagIDs(t, recorder.next(t), 1)
	for nodeID := int64(2); nodeID <= 12; nodeID++ {
		queue.Enqueue(nodeID)
	}
	close(recorder.hold)

	if call := recorder.next(t); call != nil {
		t.Fatalf("an overflowing buffer must widen the next run, got %v", call)
	}
}

func TestQueueFlushesPendingWorkOnClose(t *testing.T) {
	recorder := newRecorder()
	// A debounce this long never elapses: only Close can get the work done.
	queue := NewQueue(recorder,
		WithDebounce(time.Hour), WithSweepInterval(time.Hour))
	queue.Enqueue(5)
	if err := queue.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertTagIDs(t, recorder.next(t), 5)

	// Probe goroutines do not know the shutdown order, so a late request is
	// dropped rather than a panic on a closed channel.
	queue.Enqueue(6)
	queue.EnqueueAll()
	if err := queue.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	recorder.quiet(t, 100*time.Millisecond)

	var absent *Queue
	absent.Enqueue(1)
	absent.EnqueueAll()
	if err := absent.Close(); err != nil {
		t.Fatalf("a nil queue must be usable: %v", err)
	}
}

// TestQueueSweepsAndSurvivesFailures covers the time-driven half: a rule carrying
// max_age_seconds changes its answer with no event at all, so the sweep must run
// unprompted — and keep running after a failed recompute.
func TestQueueSweepsAndSurvivesFailures(t *testing.T) {
	recorder := newRecorder()
	recorder.err = errors.New("数据库忙")
	var (
		mu     sync.Mutex
		logged []string
	)
	queue := NewQueue(recorder,
		WithDebounce(time.Hour), WithSweepInterval(20*time.Millisecond),
		WithQueueLogf(func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logged = append(logged, fmt.Sprintf(format, args...))
		}))
	defer queue.Close()

	for attempt := 0; attempt < 2; attempt++ {
		if call := recorder.next(t); call != nil {
			t.Fatalf("the sweep is the expiry path and must be full, got %v", call)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) < 2 || !strings.Contains(logged[0], "数据库忙") {
		t.Fatalf("a failed recompute must be reported: %v", logged)
	}
}
