package nodetag

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Queue defaults. The debounce collapses one detection sweep — which finishes a
// few hundred nodes over a few seconds — into a single recompute; the sweep
// interval exists because a rule carrying max_age_seconds changes its answer
// with the passage of time alone, so something has to be time-driven.
const (
	DefaultDebounce         = 2 * time.Second
	DefaultSweepInterval    = 10 * time.Minute
	DefaultRecomputeTimeout = 5 * time.Minute
	defaultQueueBuffer      = 1024
)

// allNodes is the sentinel a full-recompute request travels as, so ordering
// against per-node requests is preserved by the one channel.
const allNodes = int64(0)

// maxWaitAbsolute caps how long a tag batch may age before it flushes even
// though events keep arriving. It bounds the continuous-stream degradation of
// the per-event debounce reset.
const maxWaitAbsolute = 30 * time.Second

// Recomputer is the half of Service a queue drives.
type Recomputer interface {
	Recompute(ctx context.Context, nodeIDs []int64) ([]int64, error)
	RecomputeAll(ctx context.Context) ([]int64, error)
}

// Queue coalesces recompute requests coming off the detection pipeline.
//
// Two properties matter more than throughput. Sends never block, because the
// callers are probe goroutines: when the buffer is full the queue degrades to a
// full recompute instead of dropping node IDs. And a coalesced request is never
// lost on shutdown, because Close flushes what is pending before returning.
type Queue struct {
	service  Recomputer
	events   chan int64
	debounce time.Duration
	sweep    time.Duration
	timeout  time.Duration
	logf     func(string, ...any)
	overflow atomic.Bool

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// QueueOption configures a Queue.
type QueueOption func(*Queue)

// WithDebounce sets the quiet period a request waits out before it runs.
func WithDebounce(debounce time.Duration) QueueOption {
	return func(q *Queue) {
		if debounce > 0 {
			q.debounce = debounce
		}
	}
}

// WithSweepInterval sets how often a full recompute runs unprompted.
func WithSweepInterval(interval time.Duration) QueueOption {
	return func(q *Queue) {
		if interval > 0 {
			q.sweep = interval
		}
	}
}

// WithQueueBuffer sets the request buffer. A full buffer escalates to a full
// recompute, so a small buffer costs work but never correctness.
func WithQueueBuffer(size int) QueueOption {
	return func(q *Queue) {
		if size > 0 {
			q.events = make(chan int64, size)
		}
	}
}

// WithQueueLogf overrides the queue's log sink.
func WithQueueLogf(logf func(string, ...any)) QueueOption {
	return func(q *Queue) {
		if logf != nil {
			q.logf = logf
		}
	}
}

// NewQueue starts a queue driving service. Close it to stop the worker.
func NewQueue(service Recomputer, options ...QueueOption) *Queue {
	queue := &Queue{
		service:  service,
		events:   make(chan int64, defaultQueueBuffer),
		debounce: DefaultDebounce,
		sweep:    DefaultSweepInterval,
		timeout:  DefaultRecomputeTimeout,
		logf:     log.Printf,
	}
	for _, option := range options {
		option(queue)
	}
	queue.wg.Add(1)
	go queue.run()
	return queue
}

// Enqueue requests a recompute of the given nodes. It never blocks and is safe
// after Close, which is what lets probe goroutines call it without knowing the
// shutdown order.
func (q *Queue) Enqueue(nodeIDs ...int64) {
	if q == nil || len(nodeIDs) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	for _, nodeID := range nodeIDs {
		if nodeID <= 0 {
			continue
		}
		select {
		case q.events <- nodeID:
		default:
			// The buffer is full. The worker is draining it, so it will observe
			// this flag and widen the next run rather than lose this node.
			q.overflow.Store(true)
		}
	}
}

// EnqueueAll requests a recompute of every node.
func (q *Queue) EnqueueAll() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case q.events <- allNodes:
	default:
		q.overflow.Store(true)
	}
}

// Close stops accepting requests, runs whatever is still pending, and waits for
// the worker to finish.
func (q *Queue) Close() error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	close(q.events)
	q.mu.Unlock()
	q.wg.Wait()
	return nil
}

func (q *Queue) run() {
	defer q.wg.Done()
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	defer timer.Stop()
	sweep := time.NewTicker(q.sweep)
	defer sweep.Stop()

	// The debounce resets on every event, which under a continuous event stream
	// postpones the flush forever and degenerates into sweep-only recomputes.
	// maxWait bounds the total age of a batch: the flush runs at the earlier of
	// "quiet for debounce" and "first event is maxWait old". The bound is
	// proportional to the debounce (16 quiet periods' worth) but never more
	// than maxWaitAbsolute, so a very large debounce still flushes.
	maxWait := q.debounce * 16
	if maxWait > maxWaitAbsolute {
		maxWait = maxWaitAbsolute
	}
	var firstEvent time.Time

	pending := map[int64]struct{}{}
	full := false
	var deadline <-chan time.Time
	// flushBatch runs the recompute; on failure the batch is kept and the
	// deadline re-armed so it retries at debounce cadence. Returns the batch
	// state for the caller to continue with.
	flushBatch := func() {
		if q.flush(pending, full) {
			pending, full = map[int64]struct{}{}, false
			firstEvent = time.Time{}
			return
		}
		// Keep pending/full; retry after the debounce period.
		stopTimer(timer)
		timer.Reset(q.debounce)
		deadline = timer.C
	}
	for {
		select {
		case nodeID, open := <-q.events:
			if !open {
				q.flush(pending, full)
				return
			}
			if nodeID == allNodes {
				full = true
			} else {
				pending[nodeID] = struct{}{}
			}
			if q.overflow.Swap(false) {
				full = true
			}
			if firstEvent.IsZero() {
				firstEvent = time.Now()
			}
			stopTimer(timer)
			// Cap the debounce: once the batch has aged past maxWait, flush at
			// the debounce boundary regardless of further events.
			quiet := q.debounce
			if age := time.Since(firstEvent); age+quiet > maxWait {
				quiet = maxWait - age
				if quiet < time.Millisecond {
					quiet = time.Millisecond
				}
			}
			timer.Reset(quiet)
			deadline = timer.C
		case <-deadline:
			deadline = nil
			flushBatch()
		case <-sweep.C:
			if deadline != nil {
				stopTimer(timer)
				deadline = nil
			}
			// The sweep is the expiry path, so it is always a full run.
			if q.flush(pending, true) {
				pending, full = map[int64]struct{}{}, false
				firstEvent = time.Time{}
			} else {
				// Sweep failure: the batch (now marked full) retries via the
				// re-armed deadline below.
				full = true
				stopTimer(timer)
				timer.Reset(q.debounce)
				deadline = timer.C
			}
		}
	}
}

// stopTimer stops an armed timer and drains it if it already fired. Only run
// reads timer.C, so the non-blocking drain cannot miss a value.
func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// flush runs one recompute for the batch. On failure it re-arms the timer at
// debounce cadence with the same batch, so a transient store error retries
// within seconds instead of the batch silently waiting for the next sweep
// (up to the full sweep interval away).
func (q *Queue) flush(pending map[int64]struct{}, full bool) bool {
	if !full && len(pending) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), q.timeout)
	defer cancel()

	var (
		changed []int64
		err     error
	)
	if full {
		changed, err = q.service.RecomputeAll(ctx)
	} else {
		nodeIDs := make([]int64, 0, len(pending))
		for nodeID := range pending {
			nodeIDs = append(nodeIDs, nodeID)
		}
		sort.Slice(nodeIDs, func(first, second int) bool { return nodeIDs[first] < nodeIDs[second] })
		changed, err = q.service.Recompute(ctx, nodeIDs)
	}
	if err != nil {
		q.logf("nodetag: 自动标签重算失败: %v", err)
		return false
	}
	if len(changed) > 0 {
		q.logf("nodetag: %d 个节点的标签已更新", len(changed))
	}
	return true
}
