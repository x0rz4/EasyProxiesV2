package monitor

import (
	"sync"
	"sync/atomic"
)

// Each subscription owns an unbounded FIFO. Publishing only appends to the
// queue, so slow control-plane callbacks cannot block probe workers.
type healthEventSubscription struct {
	callback func(HealthResultEvent)
	tags     []string
	nodeIDs  []int64
	global   bool
	mu       sync.Mutex
	queue    []HealthResultEvent
	wake     chan struct{}
	stop     chan struct{}
	closed   bool
}

func newHealthEventSubscription(callback func(HealthResultEvent), tags []string, nodeIDs []int64, global bool) *healthEventSubscription {
	s := &healthEventSubscription{callback: callback, tags: tags, nodeIDs: nodeIDs, global: global, wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go s.run()
	return s
}

func (s *healthEventSubscription) enqueue(event HealthResultEvent) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, event)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *healthEventSubscription) run() {
	for {
		select {
		case <-s.wake:
			for {
				s.mu.Lock()
				if len(s.queue) == 0 {
					s.mu.Unlock()
					break
				}
				event := s.queue[0]
				s.queue[0] = HealthResultEvent{}
				s.queue = s.queue[1:]
				s.mu.Unlock()
				s.callback(event)
			}
		case <-s.stop:
			return
		}
	}
}

func (s *healthEventSubscription) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
		s.queue = nil
	}
	s.mu.Unlock()
}

type healthEventHub struct {
	mu       sync.RWMutex
	nextID   atomic.Uint64
	byID     map[uint64]*healthEventSubscription
	global   map[uint64]struct{}
	byTag    map[string]map[uint64]struct{}
	byNodeID map[int64]map[uint64]struct{}
}

func newHealthEventHub() *healthEventHub {
	return &healthEventHub{byID: make(map[uint64]*healthEventSubscription), global: make(map[uint64]struct{}), byTag: make(map[string]map[uint64]struct{}), byNodeID: make(map[int64]map[uint64]struct{})}
}

func (h *healthEventHub) subscribe(tags []string, nodeIDs []int64, global bool, callback func(HealthResultEvent)) func() {
	if callback == nil {
		return func() {}
	}
	tagSet := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag != "" {
			tagSet[tag] = struct{}{}
		}
	}
	nodeSet := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID != 0 {
			nodeSet[nodeID] = struct{}{}
		}
	}
	normalizedTags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		normalizedTags = append(normalizedTags, tag)
	}
	normalizedNodeIDs := make([]int64, 0, len(nodeSet))
	for nodeID := range nodeSet {
		normalizedNodeIDs = append(normalizedNodeIDs, nodeID)
	}
	subscription := newHealthEventSubscription(callback, normalizedTags, normalizedNodeIDs, global)
	id := h.nextID.Add(1)
	h.mu.Lock()
	h.byID[id] = subscription
	if global {
		h.global[id] = struct{}{}
	}
	for _, tag := range normalizedTags {
		index := h.byTag[tag]
		if index == nil {
			index = make(map[uint64]struct{})
			h.byTag[tag] = index
		}
		index[id] = struct{}{}
	}
	for _, nodeID := range normalizedNodeIDs {
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
	for _, tag := range subscription.tags {
		delete(h.byTag[tag], id)
		if len(h.byTag[tag]) == 0 {
			delete(h.byTag, tag)
		}
	}
	for _, nodeID := range subscription.nodeIDs {
		delete(h.byNodeID[nodeID], id)
		if len(h.byNodeID[nodeID]) == 0 {
			delete(h.byNodeID, nodeID)
		}
	}
	h.mu.Unlock()
	subscription.close()
}

func (h *healthEventHub) publish(event HealthResultEvent) {
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
	for _, subscription := range subscribers {
		subscription.enqueue(event)
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
		subscription.enqueue(event)
	}
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
