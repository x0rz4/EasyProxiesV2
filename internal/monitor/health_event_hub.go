package monitor

import (
	"sync"
	"sync/atomic"
)

type healthEventSubscription struct {
	callback func(HealthResultEvent)
	tags     []string
	nodeIDs  []int64
	global   bool
}

// healthEventHub indexes observers by concrete runtime tag and stable Node ID.
// Publishing cost is proportional to affected pools instead of all pools.
type healthEventHub struct {
	mu       sync.RWMutex
	nextID   atomic.Uint64
	byID     map[uint64]healthEventSubscription
	global   map[uint64]struct{}
	byTag    map[string]map[uint64]struct{}
	byNodeID map[int64]map[uint64]struct{}
}

func newHealthEventHub() *healthEventHub {
	return &healthEventHub{
		byID:     make(map[uint64]healthEventSubscription),
		global:   make(map[uint64]struct{}),
		byTag:    make(map[string]map[uint64]struct{}),
		byNodeID: make(map[int64]map[uint64]struct{}),
	}
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

	id := h.nextID.Add(1)
	h.mu.Lock()
	h.byID[id] = healthEventSubscription{callback: callback, tags: normalizedTags, nodeIDs: normalizedNodeIDs, global: global}
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
	return func() {
		once.Do(func() { h.unsubscribe(id) })
	}
}

func (h *healthEventHub) unsubscribe(id uint64) {
	h.mu.Lock()
	subscription, exists := h.byID[id]
	if !exists {
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
	callbacks := make([]func(HealthResultEvent), 0, len(matched))
	for id := range matched {
		if subscription, exists := h.byID[id]; exists {
			callbacks = append(callbacks, subscription.callback)
		}
	}
	h.mu.RUnlock()
	for _, callback := range callbacks {
		callback(event)
	}
}
