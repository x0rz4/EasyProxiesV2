package monitor

import (
	"sync"
	"time"
)

// nodeRegistry owns runtime identity and reload-generation bookkeeping. It
// maintains both indexes atomically so resolving a stable Node ID never needs
// to scan the complete runtime-tag map.
type nodeRegistry struct {
	manager *Manager
	mu      sync.RWMutex
	byTag   map[string]*entry
	byID    map[int64]*entry
	reload  uint64
}

func newNodeRegistry(manager *Manager) *nodeRegistry {
	return &nodeRegistry{
		manager: manager,
		byTag:   make(map[string]*entry),
		byID:    make(map[int64]*entry),
	}
}

func (r *nodeRegistry) tags() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tags := make([]string, 0, len(r.byTag))
	for tag := range r.byTag {
		tags = append(tags, tag)
	}
	return tags
}

func (r *nodeRegistry) entries() []*entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]*entry, 0, len(r.byTag))
	for _, item := range r.byTag {
		entries = append(entries, item)
	}
	return entries
}

func (r *nodeRegistry) entriesByTag() map[string]*entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make(map[string]*entry, len(r.byTag))
	for tag, item := range r.byTag {
		entries[tag] = item
	}
	return entries
}

func (r *nodeRegistry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byTag)
}

func (r *nodeRegistry) byTagEntry(tag string) *entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byTag[tag]
}

func (r *nodeRegistry) byNodeID(nodeID int64) *entry {
	if nodeID == 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[nodeID]
}

func (r *nodeRegistry) beginReload() {
	r.mu.Lock()
	r.reload++
	r.mu.Unlock()
}

func (r *nodeRegistry) sweepStale() {
	r.mu.Lock()
	for tag, item := range r.byTag {
		item.mu.RLock()
		generation := item.reloadGen
		nodeID := item.info.NodeID
		item.mu.RUnlock()
		if generation == r.reload {
			continue
		}
		delete(r.byTag, tag)
		if nodeID != 0 && r.byID[nodeID] == item {
			delete(r.byID, nodeID)
		}
	}
	r.mu.Unlock()
}

func (r *nodeRegistry) clear() {
	r.mu.Lock()
	r.byTag = make(map[string]*entry)
	r.byID = make(map[int64]*entry)
	r.mu.Unlock()
}

func (r *nodeRegistry) register(info NodeInfo) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.byTag[info.Tag]
	if item == nil {
		item = &entry{
			info:         info,
			timeline:     make([]TimelineEvent, 0, maxTimelineSize),
			healthSource: "none",
			reloadGen:    r.reload,
			onTimeline:   r.manager.publishDebugLog,
		}
		r.byTag[info.Tag] = item
	} else {
		item.mu.Lock()
		oldNodeID := item.info.NodeID
		item.info = info
		item.reloadGen = r.reload
		item.onTimeline = r.manager.publishDebugLog
		item.mu.Unlock()
		if oldNodeID != 0 && oldNodeID != info.NodeID && r.byID[oldNodeID] == item {
			delete(r.byID, oldNodeID)
		}
	}
	if info.NodeID != 0 {
		r.byID[info.NodeID] = item
	}
	return item
}

func (r *nodeRegistry) migrate(nodeID int64, info NodeInfo) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if target := r.byTag[info.Tag]; target != nil {
		target.mu.Lock()
		oldNodeID := target.info.NodeID
		target.info = info
		target.reloadGen = r.reload
		target.onTimeline = r.manager.publishDebugLog
		target.mu.Unlock()
		if oldNodeID != 0 && oldNodeID != info.NodeID && r.byID[oldNodeID] == target {
			delete(r.byID, oldNodeID)
		}
		if info.NodeID != 0 {
			r.byID[info.NodeID] = target
		}
		return target
	}

	item := r.byID[nodeID]
	oldTag := ""
	if item != nil {
		item.mu.RLock()
		oldTag = item.info.Tag
		item.mu.RUnlock()
		delete(r.byTag, oldTag)
	} else {
		item = &entry{timeline: make([]TimelineEvent, 0, maxTimelineSize), healthSource: "none", onTimeline: r.manager.publishDebugLog}
	}
	item.mu.Lock()
	item.info = info
	item.reloadGen = r.reload
	item.onTimeline = r.manager.publishDebugLog
	if oldTag != "" && oldTag != info.Tag {
		// Health belongs to the concrete outbound generation. Preserve history,
		// but require the replacement runtime to prove current availability.
		item.initialCheckDone = false
		item.available = false
		item.lastHealthCheck = time.Time{}
		if item.healthSource != "none" {
			item.healthSource = "previous_generation"
		}
	}
	item.mu.Unlock()
	r.byTag[info.Tag] = item
	if nodeID != 0 && nodeID != info.NodeID && r.byID[nodeID] == item {
		delete(r.byID, nodeID)
	}
	if info.NodeID != 0 {
		r.byID[info.NodeID] = item
	}
	return item
}

func (r *nodeRegistry) unregister(tag string) {
	r.mu.Lock()
	if item := r.byTag[tag]; item != nil {
		item.mu.RLock()
		currentTag := item.info.Tag
		nodeID := item.info.NodeID
		item.mu.RUnlock()
		if currentTag == tag {
			delete(r.byTag, tag)
			if nodeID != 0 && r.byID[nodeID] == item {
				delete(r.byID, nodeID)
			}
		}
	}
	r.mu.Unlock()
}
