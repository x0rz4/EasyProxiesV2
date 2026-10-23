package monitor

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/store"
)

const (
	// healthEventReadBatch bounds one journal read. The consumer pulls the next
	// batch only after the previous callbacks returned, so a wedged consumer
	// stops at one batch instead of buffering the whole journal in memory.
	healthEventReadBatch = 64
	// healthEventConsumedRetention is how long an event that every active
	// consumer has already passed stays readable. Journal size in steady state is
	// rate × this window, so it also sets the sustainable probe rate: 8192 rows
	// over 5 minutes is ~27 measurements/second, far above any configured probe
	// interval. A longer window would let a normally-behaving deployment fill the
	// journal and start rejecting measurements even with every consumer caught
	// up.
	healthEventConsumedRetention = 5 * time.Minute
	// healthEventRetention applies when no consumer is registered: nothing owes
	// the journal, so it is reclaimed by age alone.
	healthEventRetention     = 30 * time.Minute
	healthEventPruneInterval = time.Minute
	healthEventRetryDelay    = time.Second
	// healthEventInlineLimit bounds the compatibility path used when no store is
	// configured (embedders and unit tests). It is deliberately still lossy and
	// MUST NOT be read as the production contract: production delivery is the
	// journal plus cursor path.
	healthEventInlineLimit = 1024
)

// healthEventSubscription is one consumer of the health journal. The journal is
// the authoritative ordered stream; wakeups here only say "there may be new
// rows", so coalesced or lost wakeups cost latency, never a transition.
type healthEventSubscription struct {
	callback func(HealthResultEvent)
	tags     map[string]struct{}
	nodeIDs  map[int64]struct{}
	global   bool
	// consumer is the durable cursor key. Empty means an ephemeral observer
	// (HTTP view, test) whose progress lives only in this process.
	consumer string
	journal  func() store.Store

	mu            sync.Mutex
	cursor        int64
	pendingCursor int64
	dirty         bool
	refresh       bool
	// cursorBlocked is set when a cursor write failed. Delivery stops until it
	// clears: the in-process replay ring only covers one batch, so advancing
	// past unpersisted progress would let a restart replay events whose
	// ProbeIDs have already been evicted from that ring and count them twice.
	cursorBlocked bool

	inline         []HealthResultEvent
	closed         bool
	delivered      int64
	gaps           int64
	dropped        int64
	readFailures   int64
	cursorFailures int64
	retryScheduled bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newHealthEventSubscription(callback func(HealthResultEvent), tags []string, nodeIDs []int64, global bool, consumer string, journal func() store.Store) *healthEventSubscription {
	s := &healthEventSubscription{
		callback: callback, global: global, consumer: consumer, journal: journal,
		tags: make(map[string]struct{}, len(tags)), nodeIDs: make(map[int64]struct{}, len(nodeIDs)),
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	for _, tag := range tags {
		if tag != "" {
			s.tags[tag] = struct{}{}
		}
	}
	for _, nodeID := range nodeIDs {
		if nodeID != 0 {
			s.nodeIDs[nodeID] = struct{}{}
		}
	}
	s.initCursor()
	go s.run()
	return s
}

// initCursor resumes a durable consumer where it stopped. A consumer with no
// persisted cursor starts at the journal tail: replaying the whole journal on a
// first start would turn startup into a burst of stale evictions and recoveries.
func (s *healthEventSubscription) initCursor() {
	db := s.journal()
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.consumer != "" {
		seq, found, err := db.GetHealthEventCursor(ctx, s.consumer)
		if err != nil {
			log.Printf("[monitor] health cursor %s: %v", s.consumer, err)
		} else if found {
			s.mu.Lock()
			s.cursor = seq
			s.mu.Unlock()
			return
		}
	}
	tail, err := db.MaxHealthEventSeq(ctx)
	if err != nil {
		log.Printf("[monitor] health journal tail: %v", err)
		return
	}
	s.mu.Lock()
	s.cursor = tail
	s.mu.Unlock()
	if s.consumer != "" {
		if err := db.PutHealthEventCursor(ctx, s.consumer, tail); err != nil {
			log.Printf("[monitor] persist initial health cursor %s: %v", s.consumer, err)
		}
	}
}

// notify records that work is available. SnapshotOnly is a mergeable refresh
// request, not a health fact: it is never counted as a success or failure.
func (s *healthEventSubscription) notify(event HealthResultEvent) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	switch {
	case event.SnapshotOnly:
		s.refresh = true
	case event.Seq > 0 && s.journal() != nil:
		s.dirty = true
	default:
		// Compatibility path: events that never entered a journal (manual
		// injection, no-store embedders). Explicitly best effort.
		if len(s.inline) >= healthEventInlineLimit {
			s.dropped++
			s.mu.Unlock()
			return
		}
		s.inline = append(s.inline, event)
	}
	s.mu.Unlock()
	s.wakeup()
}

func (s *healthEventSubscription) wakeup() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *healthEventSubscription) run() {
	defer close(s.done)
	for {
		select {
		case <-s.wake:
			for s.step() {
			}
		case <-s.stop:
			return
		}
	}
}

// step delivers at most one inline event, one journal batch or one coalesced
// refresh, and reports whether more work may remain. Callbacks run without any
// subscription lock held.
func (s *healthEventSubscription) step() bool {
	// Parked on an unpersisted cursor: retry that first and deliver nothing
	// else, so the replay gap cannot widen while the write is failing.
	if s.cursorIsBlocked() {
		s.flushCursor()
		return !s.cursorIsBlocked()
	}
	if event, ok := s.popInline(); ok {
		s.deliver(event)
		return true
	}
	if s.flushCursor() {
		return true
	}
	if more := s.readJournal(); more {
		return true
	}
	if s.takeRefresh() {
		s.deliver(HealthResultEvent{SnapshotOnly: true, CheckedAt: time.Now()})
		return true
	}
	return false
}

func (s *healthEventSubscription) popInline() (HealthResultEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.inline) == 0 {
		return HealthResultEvent{}, false
	}
	event := s.inline[0]
	s.inline[0] = HealthResultEvent{}
	s.inline = s.inline[1:]
	return event, true
}

func (s *healthEventSubscription) takeRefresh() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.refresh {
		return false
	}
	s.refresh = false
	return true
}

// flushCursor persists progress that was made but not yet committed. Delivery
// stops until it succeeds: advancing the journal past an unpersisted cursor
// would silently replay applied transitions after a restart.
func (s *healthEventSubscription) flushCursor() bool {
	s.mu.Lock()
	pending, consumer := s.pendingCursor, s.consumer
	if s.closed || pending == 0 || consumer == "" {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	db := s.journal()
	if db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PutHealthEventCursor(ctx, consumer, pending); err != nil {
		s.mu.Lock()
		s.cursorFailures++
		s.cursorBlocked = true
		s.mu.Unlock()
		log.Printf("[monitor] persist health cursor %s=%d: %v", consumer, pending, err)
		s.scheduleRetry()
		return false
	}
	s.mu.Lock()
	if s.pendingCursor == pending {
		s.pendingCursor = 0
	}
	s.cursorBlocked = false
	s.mu.Unlock()
	return true
}

// cursorIsBlocked reports whether delivery must stay parked on an unpersisted
// cursor. It is checked before every journal read so a failed flush cannot be
// bypassed by the next wakeup.
func (s *healthEventSubscription) cursorIsBlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursorBlocked
}

// readJournal pulls the next ordered batch and applies it. It returns true when
// a full batch was consumed, so the loop keeps draining without another wakeup.
func (s *healthEventSubscription) readJournal() bool {
	db := s.journal()
	if db == nil {
		return false
	}
	s.mu.Lock()
	if s.closed || !s.dirty {
		s.mu.Unlock()
		return false
	}
	// A cursor that failed to persist must be retried before more rows are
	// consumed. Read failures are separate: they never moved the cursor.
	if s.cursorBlocked {
		s.mu.Unlock()
		return false
	}
	s.dirty = false
	cursor := s.cursor
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := db.ListHealthEventsAfter(ctx, cursor, healthEventReadBatch)
	if err != nil {
		s.mu.Lock()
		s.readFailures++
		s.dirty = true
		s.mu.Unlock()
		log.Printf("[monitor] read health journal after %d: %v", cursor, err)
		s.scheduleRetry()
		return false
	}
	if len(events) == 0 {
		return false
	}
	for _, event := range events {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return false
		}
		// Records removed by retention leave a hole the consumer cannot read.
		// Count it instead of pretending delivery was complete.
		if event.Seq > s.currentCursor()+1 {
			s.mu.Lock()
			s.gaps++
			s.mu.Unlock()
		}
		if s.matches(event) {
			s.deliver(healthResultEvent(event))
		}
		// Filtered events still advance the cursor, otherwise a subscription
		// could stall forever on rows meant for another consumer.
		s.mu.Lock()
		if event.Seq > s.cursor {
			s.cursor = event.Seq
			if s.consumer != "" {
				// Only durable consumers persist progress; an ephemeral
				// observer would otherwise report a cursor it never wrote.
				s.pendingCursor = event.Seq
			}
		}
		s.mu.Unlock()
	}
	// A full batch means the journal probably holds more; keep draining without
	// waiting for another wakeup, which may have already been coalesced.
	if len(events) == healthEventReadBatch {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return true
	}
	return false
}

func (s *healthEventSubscription) currentCursor() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *healthEventSubscription) matches(event store.HealthEvent) bool {
	if s.global {
		return true
	}
	if _, ok := s.tags[event.Tag]; ok {
		return true
	}
	if event.NodeID != 0 {
		if _, ok := s.nodeIDs[event.NodeID]; ok {
			return true
		}
	}
	return false
}

func (s *healthEventSubscription) deliver(event HealthResultEvent) {
	s.callback(event)
	s.mu.Lock()
	s.delivered++
	s.mu.Unlock()
}

func (s *healthEventSubscription) scheduleRetry() {
	s.mu.Lock()
	if s.closed || s.retryScheduled {
		s.mu.Unlock()
		return
	}
	s.retryScheduled = true
	s.mu.Unlock()
	time.AfterFunc(healthEventRetryDelay, func() {
		s.mu.Lock()
		s.retryScheduled = false
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			s.wakeup()
		}
	})
}

// Dropped reports events shed on the compatibility path. Production delivery
// (journal + cursor) never drops a committed event.
func (s *healthEventSubscription) Dropped() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *healthEventSubscription) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		s.inline = nil
	}
	s.mu.Unlock()
}

// HealthEventConsumerStats is one consumer's delivery progress. Gaps counts
// rows removed by retention before this consumer read them; Dropped is non-zero
// only on the no-store compatibility path.
type HealthEventConsumerStats struct {
	Consumer       string `json:"consumer"`
	Cursor         int64  `json:"cursor"`
	PendingCursor  int64  `json:"pending_cursor,omitempty"`
	Delivered      int64  `json:"delivered"`
	Gaps           int64  `json:"gaps"`
	Dropped        int64  `json:"dropped"`
	ReadFailures   int64  `json:"read_failures"`
	CursorFailures int64  `json:"cursor_failures"`
	Backlog        int64  `json:"backlog"`
}

// HealthEventStats is the observability surface for health delivery. Backlog is
// the number of journal rows the slowest consumer still owes.
// HealthEventStats is the JSON surface of GET /api/operations/health-events.
type HealthEventStats struct {
	Capacity             int                        `json:"capacity"`
	Count                int64                      `json:"count"`
	MaxSeq               int64                      `json:"max_seq"`
	Backlog              int64                      `json:"backlog"`
	OldestAgeSeconds     int64                      `json:"oldest_age_seconds"`
	RejectedMeasurements int64                      `json:"rejected_measurements"`
	Consumers            []HealthEventConsumerStats `json:"consumers"`
}

type healthEventHub struct {
	mu       sync.RWMutex
	nextID   atomic.Uint64
	byID     map[uint64]*healthEventSubscription
	global   map[uint64]struct{}
	byTag    map[string]map[uint64]struct{}
	byNodeID map[int64]map[uint64]struct{}
	journal  func() store.Store
	rejected atomic.Int64
}

func newHealthEventHub(journal func() store.Store) *healthEventHub {
	return &healthEventHub{byID: make(map[uint64]*healthEventSubscription), global: make(map[uint64]struct{}),
		byTag: make(map[string]map[uint64]struct{}), byNodeID: make(map[int64]map[uint64]struct{}), journal: journal}
}

// durableConsumers reports the keys of currently registered durable consumers.
// Pruning is bounded by these; a cursor row whose consumer is no longer
// registered must not hold the journal open.
func (h *healthEventHub) durableConsumers() []string {
	h.mu.RLock()
	consumers := make([]string, 0, len(h.byID))
	for _, subscription := range h.byID {
		if subscription.consumer != "" {
			consumers = append(consumers, subscription.consumer)
		}
	}
	h.mu.RUnlock()
	sort.Strings(consumers)
	return consumers
}

// subscribe installs an observer. An empty consumer key keeps the process-local
// cursor that the compatibility and UI paths have always used.
func (h *healthEventHub) subscribe(tags []string, nodeIDs []int64, global bool, callback func(HealthResultEvent)) func() {
	return h.subscribeAs("", tags, nodeIDs, global, callback)
}

func (h *healthEventHub) subscribeAs(consumer string, tags []string, nodeIDs []int64, global bool, callback func(HealthResultEvent)) func() {
	if callback == nil {
		return func() {}
	}
	return h.register(consumer, tags, nodeIDs, global, callback)
}

func (h *healthEventHub) register(consumer string, tags []string, nodeIDs []int64, global bool, callback func(HealthResultEvent)) func() {
	subscription := newHealthEventSubscription(callback, tags, nodeIDs, global, consumer, h.journal)
	id := h.nextID.Add(1)
	h.mu.Lock()
	h.byID[id] = subscription
	if global {
		h.global[id] = struct{}{}
	}
	for _, tag := range subscription.sortedTags() {
		index := h.byTag[tag]
		if index == nil {
			index = make(map[uint64]struct{})
			h.byTag[tag] = index
		}
		index[id] = struct{}{}
	}
	for _, nodeID := range subscription.sortedNodeIDs() {
		index := h.byNodeID[nodeID]
		if index == nil {
			index = make(map[uint64]struct{})
			h.byNodeID[nodeID] = index
		}
		index[id] = struct{}{}
	}
	h.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { h.unsubscribe(id) }) }
}

func (h *healthEventHub) unsubscribe(id uint64) {
	h.mu.Lock()
	subscription := h.byID[id]
	if subscription == nil {
		h.mu.Unlock()
		return
	}
	delete(h.byID, id)
	delete(h.global, id)
	for _, tag := range subscription.sortedTags() {
		delete(h.byTag[tag], id)
		if len(h.byTag[tag]) == 0 {
			delete(h.byTag, tag)
		}
	}
	for _, nodeID := range subscription.sortedNodeIDs() {
		delete(h.byNodeID[nodeID], id)
		if len(h.byNodeID[nodeID]) == 0 {
			delete(h.byNodeID, nodeID)
		}
	}
	h.mu.Unlock()
	subscription.close()
}

func (h *healthEventHub) publish(event HealthResultEvent) {
	for _, subscription := range h.match(event) {
		subscription.notify(event)
	}
}

func (h *healthEventHub) broadcast(event HealthResultEvent) {
	h.mu.RLock()
	subscribers := make([]*healthEventSubscription, 0, len(h.byID))
	for _, subscription := range h.byID {
		subscribers = append(subscribers, subscription)
	}
	h.mu.RUnlock()
	for _, subscription := range subscribers {
		subscription.notify(event)
	}
}

func (h *healthEventHub) match(event HealthResultEvent) []*healthEventSubscription {
	h.mu.RLock()
	matched := make(map[uint64]struct{}, len(h.global)+2)
	for id := range h.global {
		matched[id] = struct{}{}
	}
	for id := range h.byTag[event.Tag] {
		matched[id] = struct{}{}
	}
	if event.NodeID != 0 {
		for id := range h.byNodeID[event.NodeID] {
			matched[id] = struct{}{}
		}
	}
	subscribers := make([]*healthEventSubscription, 0, len(matched))
	for id := range matched {
		if subscription := h.byID[id]; subscription != nil {
			subscribers = append(subscribers, subscription)
		}
	}
	h.mu.RUnlock()
	return subscribers
}

func (h *healthEventHub) close() {
	h.mu.Lock()
	subscriptions := make([]*healthEventSubscription, 0, len(h.byID))
	for _, subscription := range h.byID {
		subscriptions = append(subscriptions, subscription)
	}
	h.byID = make(map[uint64]*healthEventSubscription)
	h.global = make(map[uint64]struct{})
	h.byTag = make(map[string]map[uint64]struct{})
	h.byNodeID = make(map[int64]map[uint64]struct{})
	h.mu.Unlock()
	for _, subscription := range subscriptions {
		subscription.close()
	}
}

// stats merges journal capacity with per-consumer progress. Journal rows beyond
// the slowest durable cursor are the backlog that still owes the state machine.
func (h *healthEventHub) stats(ctx context.Context) (HealthEventStats, error) {
	stats := HealthEventStats{Capacity: store.HealthEventJournalLimit, RejectedMeasurements: h.rejected.Load(), Consumers: []HealthEventConsumerStats{}}
	h.mu.RLock()
	subscriptions := make([]*healthEventSubscription, 0, len(h.byID))
	for _, subscription := range h.byID {
		subscriptions = append(subscriptions, subscription)
	}
	h.mu.RUnlock()
	db := h.journal()
	var journalStats store.HealthEventJournalStats
	if db != nil {
		var err error
		journalStats, err = db.HealthEventJournalStats(ctx)
		if err != nil {
			return stats, err
		}
		stats.Count = journalStats.Count
		stats.MaxSeq = journalStats.MaxSeq
		if !journalStats.OldestRecordedAt.IsZero() {
			stats.OldestAgeSeconds = int64(time.Since(journalStats.OldestRecordedAt).Seconds())
		}
	}
	slowest := journalStats.MaxSeq
	seenDurable := false
	for _, subscription := range subscriptions {
		subscription.mu.Lock()
		entry := HealthEventConsumerStats{
			Consumer: subscription.consumer, Cursor: subscription.cursor, PendingCursor: subscription.pendingCursor,
			Delivered: subscription.delivered, Gaps: subscription.gaps,
			Dropped: subscription.dropped, ReadFailures: subscription.readFailures, CursorFailures: subscription.cursorFailures,
		}
		subscription.mu.Unlock()
		entry.Backlog = journalStats.MaxSeq - entry.Cursor
		if entry.Backlog < 0 {
			entry.Backlog = 0
		}
		if entry.Consumer != "" {
			seenDurable = true
			if entry.Cursor < slowest {
				slowest = entry.Cursor
			}
		}
		stats.Consumers = append(stats.Consumers, entry)
	}
	if !seenDurable {
		// Nothing is consuming the journal: every retained row is backlog.
		slowest = journalStats.MinSeq - 1
	}
	if slowest < 0 {
		slowest = 0
	}
	stats.Backlog = stats.MaxSeq - slowest
	if stats.Backlog < 0 {
		stats.Backlog = 0
	}
	sort.Slice(stats.Consumers, func(i, j int) bool { return stats.Consumers[i].Consumer < stats.Consumers[j].Consumer })
	return stats, nil
}

func (s *healthEventSubscription) sortedTags() []string {
	tags := make([]string, 0, len(s.tags))
	for tag := range s.tags {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func (s *healthEventSubscription) sortedNodeIDs() []int64 {
	nodeIDs := make([]int64, 0, len(s.nodeIDs))
	for nodeID := range s.nodeIDs {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Slice(nodeIDs, func(i, j int) bool { return nodeIDs[i] < nodeIDs[j] })
	return nodeIDs
}

// result converts a journal row into the event shape the pool consumer already
// understands, so the transport change does not alter routing behaviour.
func healthResultEvent(e store.HealthEvent) HealthResultEvent {
	return HealthResultEvent{
		Tag: e.Tag, NodeID: e.NodeID, Success: e.Success, Latency: time.Duration(e.LatencyMs) * time.Millisecond,
		Error: e.Error, CheckedAt: e.CheckedAt, Seq: e.Seq, ProbeID: e.ProbeID,
	}
}
