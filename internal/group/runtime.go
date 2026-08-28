package group

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// GroupInitialState restores the persisted state for one member.
type GroupInitialState struct {
	NodeID         int64
	FailureHistory []int64
	Evicted        bool
	LastError      string
	EvictedAt      time.Time
}

// GroupStateEvent is emitted after a state change so the application can
// persist it without coupling the outbound package to SQLite.
type GroupStateEvent struct {
	GroupID        int64
	NodeID         int64
	FailureHistory []int64
	Evicted        bool
	LastError      string
	EvictedAt      time.Time
	CurrentNodeID  int64
	CurrentChanged bool
	StateChanged   bool
	Recovered      bool
}

// GroupMemberSnapshot is the runtime view consumed by the management API.
type GroupMemberSnapshot struct {
	NodeID         int64     `json:"node_id"`
	Tag            string    `json:"tag"`
	Status         string    `json:"status"`
	FailureCount   int       `json:"failure_count"`
	FailureHistory []int64   `json:"failure_history,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	EvictedAt      time.Time `json:"evicted_at,omitempty"`
}

// GroupRuntimeSnapshot is a concurrency-safe group runtime view.
type GroupRuntimeSnapshot struct {
	GroupID       int64                 `json:"group_id"`
	CurrentNodeID int64                 `json:"current_node_id,omitempty"`
	CurrentTag    string                `json:"current_tag,omitempty"`
	Members       []GroupMemberSnapshot `json:"members"`
}

type groupMemberRuntime struct {
	nodeID         int64
	tag            string
	failureHistory []int64
	evicted        bool
	lastError      string
	evictedAt      time.Time
}

type groupRuntime struct {
	mu         sync.Mutex
	id         int64
	window     time.Duration
	threshold  int
	currentTag string
	members    map[string]*groupMemberRuntime
}

var (
	groupRuntimeStore  sync.Map // map[int64]*groupRuntime
	globalEvictedNodes sync.Map // map[int64]bool; an evicted node is excluded from every group
	groupObserverMu    sync.RWMutex
	groupObserver      func(GroupStateEvent)
	activationHandlers sync.Map // map[int64]func(int64) error
	stateSubscriberMu  sync.RWMutex
	stateSubscriberID  uint64
	stateSubscribers   = make(map[uint64]func(GroupStateEvent))
)

// SubscribeStateChanges registers a lightweight runtime observer. Unlike the
// single persistence observer, subscribers are intended for in-process caches
// such as pool candidate snapshots. Callbacks run after group locks are
// released and must return promptly.
func SubscribeStateChanges(subscriber func(GroupStateEvent)) func() {
	if subscriber == nil {
		return func() {}
	}
	stateSubscriberMu.Lock()
	stateSubscriberID++
	id := stateSubscriberID
	stateSubscribers[id] = subscriber
	stateSubscriberMu.Unlock()
	return func() {
		stateSubscriberMu.Lock()
		delete(stateSubscribers, id)
		stateSubscriberMu.Unlock()
	}
}

// RegisterActivationHandler wires the runtime group to its sing-box selector.
// The returned cleanup only removes the handler if it is still the active one.
func RegisterActivationHandler(groupID int64, handler func(int64) error) func() {
	if groupID == 0 || handler == nil {
		return func() {}
	}
	holder := &activationHandler{activate: handler}
	activationHandlers.Store(groupID, holder)
	return func() { activationHandlers.CompareAndDelete(groupID, holder) }
}

type activationHandler struct{ activate func(int64) error }

// ActivateMember performs a selector-only manual hot switch.
func ActivateMember(groupID, nodeID int64) error {
	value, ok := activationHandlers.Load(groupID)
	if !ok {
		return errors.New("group runtime not found")
	}
	return value.(*activationHandler).activate(nodeID)
}

// SetGroupStateObserver installs the persistence callback.
func SetGroupStateObserver(observer func(GroupStateEvent)) {
	groupObserverMu.Lock()
	groupObserver = observer
	groupObserverMu.Unlock()
}

func notifyGroupState(event GroupStateEvent) {
	groupObserverMu.RLock()
	observer := groupObserver
	groupObserverMu.RUnlock()
	if observer != nil {
		observer(event)
	}
	stateSubscriberMu.RLock()
	subscribers := make([]func(GroupStateEvent), 0, len(stateSubscribers))
	for _, subscriber := range stateSubscribers {
		subscribers = append(subscribers, subscriber)
	}
	stateSubscriberMu.RUnlock()
	for _, subscriber := range subscribers {
		subscriber(event)
	}
}

func Register(groupID int64, failureWindow time.Duration, failureThreshold int, currentTag string, members map[string]GroupInitialState) func() {
	if groupID == 0 {
		return func() {}
	}
	preferredTag := currentTag
	runtime := &groupRuntime{
		id: groupID, window: failureWindow, threshold: failureThreshold,
		currentTag: currentTag, members: make(map[string]*groupMemberRuntime, len(members)),
	}
	if runtime.window <= 0 {
		runtime.window = 5 * time.Minute
	}
	if runtime.threshold <= 0 {
		runtime.threshold = 3
	}
	for tag, initial := range members {
		if initial.Evicted {
			globalEvictedNodes.Store(initial.NodeID, true)
		}
		_, globallyEvicted := globalEvictedNodes.Load(initial.NodeID)
		runtime.members[tag] = &groupMemberRuntime{
			nodeID: initial.NodeID, tag: tag, failureHistory: append([]int64(nil), initial.FailureHistory...),
			evicted: initial.Evicted || globallyEvicted, lastError: initial.LastError, evictedAt: initial.EvictedAt,
		}
	}
	if current := runtime.members[runtime.currentTag]; current == nil || current.evicted || len(current.failureHistory) > 0 {
		runtime.currentTag = ""
	}
	groupRuntimeStore.Store(groupID, runtime)
	if current := runtime.members[runtime.currentTag]; current != nil {
		notifyGroupState(GroupStateEvent{GroupID: groupID, NodeID: current.nodeID, CurrentNodeID: current.nodeID, CurrentChanged: true})
	} else if preferredTag != "" {
		notifyGroupState(GroupStateEvent{GroupID: groupID, CurrentChanged: true})
	}
	for _, member := range runtime.members {
		if member.evicted {
			propagateEviction(member.nodeID, member.lastError, member.evictedAt)
		}
	}
	return func() {
		groupRuntimeStore.CompareAndDelete(groupID, runtime)
	}
}

func MemberAvailable(groupID int64, tag string) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return true
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	member := runtime.members[tag]
	if member != nil && !member.evicted {
		pruneFailures(member, runtime.window, time.Now())
	}
	return member != nil && !member.evicted && len(member.failureHistory) == 0
}

func CurrentTag(groupID int64) string {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return ""
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	if member := runtime.members[runtime.currentTag]; member != nil && !member.evicted {
		pruneFailures(member, runtime.window, time.Now())
		if len(member.failureHistory) == 0 {
			tag := runtime.currentTag
			runtime.mu.Unlock()
			return tag
		}
	}
	if runtime.currentTag == "" {
		runtime.mu.Unlock()
		return ""
	}
	runtime.currentTag = ""
	runtime.mu.Unlock()
	notifyGroupState(GroupStateEvent{GroupID: groupID, CurrentChanged: true})
	return ""
}

func SetCurrentTag(groupID int64, tag string) {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	if runtime.currentTag == tag {
		runtime.mu.Unlock()
		return
	}
	runtime.currentTag = tag
	member := runtime.members[tag]
	event := GroupStateEvent{GroupID: groupID}
	if member != nil {
		event.NodeID = member.nodeID
		event.CurrentNodeID = member.nodeID
	}
	event.CurrentChanged = true
	runtime.mu.Unlock()
	notifyGroupState(event)
}

func RecordFailure(groupID int64, tag string, cause error, at time.Time) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return false
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	member := runtime.members[tag]
	if member == nil || member.evicted {
		runtime.mu.Unlock()
		return member != nil && member.evicted
	}
	cutoff := at.Add(-runtime.window).Unix()
	history := member.failureHistory[:0]
	for _, ts := range member.failureHistory {
		if ts >= cutoff {
			history = append(history, ts)
		}
	}
	member.failureHistory = append(history, at.Unix())
	if cause != nil {
		member.lastError = cause.Error()
	}
	currentChanged := false
	if runtime.currentTag == tag {
		runtime.currentTag = ""
		currentChanged = true
	}
	newlyEvicted := len(member.failureHistory) >= runtime.threshold
	if newlyEvicted {
		member.evicted = true
		member.evictedAt = at
	}
	event := GroupStateEvent{GroupID: groupID, NodeID: member.nodeID,
		FailureHistory: append([]int64(nil), member.failureHistory...), Evicted: member.evicted,
		LastError: member.lastError, EvictedAt: member.evictedAt, CurrentChanged: currentChanged, StateChanged: true}
	runtime.mu.Unlock()
	notifyGroupState(event)
	if newlyEvicted {
		globalEvictedNodes.Store(event.NodeID, true)
		propagateEviction(event.NodeID, event.LastError, event.EvictedAt)
	}
	return event.Evicted
}

// RecordHealthSuccess clears a non-evicted member's consecutive failure
// sequence. Permanently evicted members still require an explicit restore.
func RecordHealthSuccess(groupID int64, tag string) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return false
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	member := runtime.members[tag]
	if member == nil || member.evicted || len(member.failureHistory) == 0 {
		runtime.mu.Unlock()
		return member != nil && !member.evicted
	}
	member.failureHistory = nil
	member.lastError = ""
	event := GroupStateEvent{GroupID: groupID, NodeID: member.nodeID, StateChanged: true, Recovered: true}
	runtime.mu.Unlock()
	notifyGroupState(event)
	return true
}

// RecordGroupHealthFailure feeds a failed background probe into every group
// containing the member. Repeated reports with the same second are de-duplicated.
func RecordGroupHealthFailure(groupID int64, tag string, cause error, at time.Time) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if ok {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		member := runtime.members[tag]
		if member != nil && len(member.failureHistory) > 0 && member.failureHistory[len(member.failureHistory)-1] == at.Unix() {
			evicted := member.evicted
			runtime.mu.Unlock()
			return evicted
		}
		runtime.mu.Unlock()
	}
	return RecordFailure(groupID, tag, cause, at)
}

// RestoreGroupMember clears a permanent eviction after an explicit user action.
func RestoreGroupMember(groupID, nodeID int64) error {
	_, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return errors.New("group runtime not found")
	}
	found := false
	globalEvictedNodes.Delete(nodeID)
	groupRuntimeStore.Range(func(_, value any) bool {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		for _, member := range runtime.members {
			if member.nodeID != nodeID {
				continue
			}
			found = true
			member.evicted = false
			member.evictedAt = time.Time{}
			member.failureHistory = nil
			member.lastError = ""
			event := GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, StateChanged: true, Recovered: true}
			runtime.mu.Unlock()
			notifyGroupState(event)
			return true
		}
		runtime.mu.Unlock()
		return true
	})
	if !found {
		return errors.New("group member not found")
	}
	return nil
}

// GroupRuntimeSnapshots returns all live group states.
func GroupRuntimeSnapshots() map[int64]GroupRuntimeSnapshot {
	result := make(map[int64]GroupRuntimeSnapshot)
	groupRuntimeStore.Range(func(key, value any) bool {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		var expiredEvents []GroupStateEvent
		snapshot := GroupRuntimeSnapshot{GroupID: runtime.id, CurrentTag: runtime.currentTag}
		if current := runtime.members[runtime.currentTag]; current != nil {
			snapshot.CurrentNodeID = current.nodeID
		}
		for _, member := range runtime.members {
			if !member.evicted && pruneFailures(member, runtime.window, time.Now()) {
				expiredEvents = append(expiredEvents, GroupStateEvent{GroupID: runtime.id, NodeID: member.nodeID,
					FailureHistory: append([]int64(nil), member.failureHistory...), LastError: member.lastError, StateChanged: true})
			}
			status := "ALIVE"
			if member.evicted {
				status = "EVICTED"
			} else if len(member.failureHistory) > 0 {
				status = "SUSPECT"
			}
			snapshot.Members = append(snapshot.Members, GroupMemberSnapshot{NodeID: member.nodeID, Tag: member.tag,
				Status: status, FailureCount: len(member.failureHistory), FailureHistory: append([]int64(nil), member.failureHistory...),
				LastError: member.lastError, EvictedAt: member.evictedAt})
		}
		runtime.mu.Unlock()
		for _, event := range expiredEvents {
			notifyGroupState(event)
		}
		sort.Slice(snapshot.Members, func(i, j int) bool { return snapshot.Members[i].NodeID < snapshot.Members[j].NodeID })
		result[runtime.id] = snapshot
		return true
	})
	return result
}

func Reset() {
	groupRuntimeStore.Range(func(key, _ any) bool {
		groupRuntimeStore.Delete(key)
		return true
	})
	globalEvictedNodes.Range(func(key, _ any) bool { globalEvictedNodes.Delete(key); return true })
	activationHandlers.Range(func(key, _ any) bool { activationHandlers.Delete(key); return true })
	stateSubscriberMu.Lock()
	stateSubscribers = make(map[uint64]func(GroupStateEvent))
	stateSubscriberMu.Unlock()
}

func pruneFailures(member *groupMemberRuntime, window time.Duration, now time.Time) bool {
	if len(member.failureHistory) == 0 {
		return false
	}
	cutoff := now.Add(-window).Unix()
	original := len(member.failureHistory)
	history := member.failureHistory[:0]
	for _, ts := range member.failureHistory {
		if ts >= cutoff {
			history = append(history, ts)
		}
	}
	member.failureHistory = history
	if len(history) == 0 {
		member.lastError = ""
	}
	return len(history) != original
}

func propagateEviction(nodeID int64, lastError string, evictedAt time.Time) {
	if nodeID == 0 {
		return
	}
	if evictedAt.IsZero() {
		evictedAt = time.Now()
	}
	groupRuntimeStore.Range(func(_, value any) bool {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		for _, member := range runtime.members {
			if member.nodeID != nodeID || member.evicted {
				continue
			}
			member.evicted = true
			member.evictedAt = evictedAt
			member.lastError = lastError
			currentChanged := false
			if runtime.currentTag == member.tag {
				runtime.currentTag = ""
				currentChanged = true
			}
			event := GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, FailureHistory: append([]int64(nil), member.failureHistory...),
				Evicted: true, LastError: lastError, EvictedAt: evictedAt, CurrentChanged: currentChanged, StateChanged: true}
			runtime.mu.Unlock()
			notifyGroupState(event)
			return true
		}
		runtime.mu.Unlock()
		return true
	})
}
