package group

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// GroupInitialState restores the persisted state for one member.
type GroupInitialState struct {
	Standby        bool
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
	Standby        bool      `json:"standby"`
	NodeID         int64     `json:"node_id"`
	Tag            string    `json:"tag"`
	Status         string    `json:"status"`
	FailureCount   int       `json:"failure_count"`
	FailureHistory []int64   `json:"failure_history,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	EvictedAt      time.Time `json:"evicted_at,omitempty"`
	// Recovery* describe the automatic re-admission progress of an evicted
	// member so the management UI can show "2/3" instead of an opaque "已踢出".
	RecoverySuccesses   int       `json:"recovery_successes,omitempty"`
	RecoveryThreshold   int       `json:"recovery_threshold,omitempty"`
	RecoveryCooldownEnd time.Time `json:"recovery_cooldown_end,omitempty"`
}

// RecoveryPolicy controls automatic re-admission of an evicted member.
// Eviction is a global fact about a NodeID, so a recovery confirmed by any
// group clears the eviction for every group holding that node.
type RecoveryPolicy struct {
	Enabled bool
	// SuccessThreshold is the number of consecutive successful health probes
	// required after the cooldown has elapsed.
	SuccessThreshold int
	// Cooldown is the quiet period after eviction during which successes are
	// not counted at all. It keeps a flapping node from bouncing straight back
	// into rotation.
	Cooldown time.Duration
}

const (
	defaultRecoverySuccessThreshold = 3
	defaultRecoveryCooldown         = 5 * time.Minute
)

// DefaultRecoveryPolicy is used when a caller registers a group without an
// explicit policy.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		Enabled:          true,
		SuccessThreshold: defaultRecoverySuccessThreshold,
		Cooldown:         defaultRecoveryCooldown,
	}
}

// NormalizeRecoveryPolicy only fills in missing numbers; it never flips
// Enabled, so an explicitly disabled policy stays disabled. Callers outside
// this package use it to guarantee a persisted policy is always "provided"
// (see recoveryPolicyProvided).
func NormalizeRecoveryPolicy(policy RecoveryPolicy) RecoveryPolicy {
	if policy.SuccessThreshold <= 0 {
		policy.SuccessThreshold = defaultRecoverySuccessThreshold
	}
	if policy.Cooldown < 0 {
		policy.Cooldown = 0
	}
	return policy
}

// recoveryPolicyProvided distinguishes "the caller left this unset" from an
// explicit policy. ReconcileSilent keeps the live policy when it is unset.
func recoveryPolicyProvided(policy RecoveryPolicy) bool {
	return policy.Enabled || policy.SuccessThreshold != 0 || policy.Cooldown != 0
}

// GroupRuntimeSnapshot is a concurrency-safe group runtime view.
type GroupRuntimeSnapshot struct {
	GroupID       int64                 `json:"group_id"`
	CurrentNodeID int64                 `json:"current_node_id,omitempty"`
	CurrentTag    string                `json:"current_tag,omitempty"`
	Members       []GroupMemberSnapshot `json:"members"`
}

type groupMemberRuntime struct {
	standby        bool
	nodeID         int64
	tag            string
	failureHistory []int64
	evicted        bool
	lastError      string
	evictedAt      time.Time
	// recoverySuccesses counts consecutive successful health probes since the
	// eviction cooldown elapsed. Any observed failure resets it to zero.
	recoverySuccesses int
}

type groupRuntime struct {
	mu         sync.Mutex
	id         int64
	window     time.Duration
	threshold  int
	recovery   RecoveryPolicy
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

// RuntimeUpdate describes a complete group state topology. It is consumed by
// the pool control plane after outbound validation has succeeded.
type RuntimeUpdate struct {
	FailureWindow    time.Duration
	FailureThreshold int
	PreferredNodeID  int64
	Members          map[string]GroupInitialState
	// Recovery is optional; a zero value keeps the live policy untouched.
	Recovery RecoveryPolicy
}

// ReconcileSilent migrates mutable member state by stable NodeID without
// notifying observers. The caller publishes its matching pool snapshot before
// passing the returned events to NotifyEvents.
func ReconcileSilent(groupID int64, update RuntimeUpdate) []GroupStateEvent {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return nil
	}
	runtime := value.(*groupRuntime)
	window := update.FailureWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	threshold := update.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	runtime.mu.Lock()
	oldByNodeID := make(map[int64]*groupMemberRuntime, len(runtime.members))
	for _, member := range runtime.members {
		oldByNodeID[member.nodeID] = member
	}
	currentNodeID := int64(0)
	if current := runtime.members[runtime.currentTag]; current != nil {
		currentNodeID = current.nodeID
	}
	next := make(map[string]*groupMemberRuntime, len(update.Members))
	events := make([]GroupStateEvent, 0)
	now := time.Now()
	for tag, initial := range update.Members {
		member := &groupMemberRuntime{nodeID: initial.NodeID, tag: tag, standby: initial.Standby}
		if old := oldByNodeID[initial.NodeID]; old != nil {
			member.failureHistory = append([]int64(nil), old.failureHistory...)
			member.evicted, member.lastError, member.evictedAt = old.evicted, old.lastError, old.evictedAt
			member.recoverySuccesses = old.recoverySuccesses
		} else {
			member.failureHistory = append([]int64(nil), initial.FailureHistory...)
			member.evicted, member.lastError, member.evictedAt = initial.Evicted, initial.LastError, initial.EvictedAt
		}
		if _, globallyEvicted := globalEvictedNodes.Load(member.nodeID); globallyEvicted {
			member.evicted = true
		}
		pruned := pruneFailures(member, window, now)
		newlyEvicted := !member.evicted && len(member.failureHistory) >= threshold
		if newlyEvicted {
			member.evicted = true
			member.evictedAt = now
			globalEvictedNodes.Store(member.nodeID, true)
		}
		if member.evicted && member.evictedAt.IsZero() {
			// A restored-but-undated eviction would make the cooldown
			// unmeasurable; anchor it so the policy stays well defined.
			member.evictedAt = now
		}
		old := oldByNodeID[initial.NodeID]
		policyChanged := recoveryPolicyProvided(update.Recovery) && NormalizeRecoveryPolicy(update.Recovery) != runtime.recovery
		if member.evicted && member.recoverySuccesses > 0 && (policyChanged || old != nil && old.tag != tag) {
			// The policy or topology just changed, so the old streak no longer
			// describes this generation of the member.
			member.recoverySuccesses = 0
		}
		if pruned || newlyEvicted {
			events = append(events, GroupStateEvent{GroupID: groupID, NodeID: member.nodeID,
				FailureHistory: append([]int64(nil), member.failureHistory...), Evicted: member.evicted,
				LastError: member.lastError, EvictedAt: member.evictedAt, StateChanged: true})
		}
		next[tag] = member
	}
	runtime.window, runtime.threshold, runtime.members = window, threshold, next
	if recoveryPolicyProvided(update.Recovery) {
		runtime.recovery = NormalizeRecoveryPolicy(update.Recovery)
	}
	preferredNodeID := currentNodeID
	if preferredNodeID == 0 {
		preferredNodeID = update.PreferredNodeID
	}
	runtime.currentTag = ""
	for tag, member := range next {
		if member.nodeID == preferredNodeID && !member.standby && !member.evicted && len(member.failureHistory) == 0 {
			runtime.currentTag = tag
			break
		}
	}
	nextCurrentNodeID := int64(0)
	if current := next[runtime.currentTag]; current != nil {
		nextCurrentNodeID = current.nodeID
	}
	if nextCurrentNodeID != currentNodeID {
		events = append(events, GroupStateEvent{GroupID: groupID, NodeID: nextCurrentNodeID,
			CurrentNodeID: nextCurrentNodeID, CurrentChanged: true})
	}
	runtime.mu.Unlock()
	return events
}

// NotifyEvents completes a silent reconciliation after the matching pool
// snapshot has become visible.
func NotifyEvents(events []GroupStateEvent) {
	for _, event := range events {
		notifyGroupState(event)
		if event.Evicted {
			propagateEviction(event.NodeID, event.LastError, event.EvictedAt)
		}
	}
}

// Register installs a group runtime. The optional recovery policy defaults to
// DefaultRecoveryPolicy; callers pass it explicitly to follow persisted group
// settings.
func Register(groupID int64, failureWindow time.Duration, failureThreshold int, currentTag string,
	members map[string]GroupInitialState, recovery ...RecoveryPolicy) func() {
	if groupID == 0 {
		return func() {}
	}
	preferredTag := currentTag
	policy := DefaultRecoveryPolicy()
	if len(recovery) > 0 {
		policy = NormalizeRecoveryPolicy(recovery[0])
	}
	runtime := &groupRuntime{
		id: groupID, window: failureWindow, threshold: failureThreshold,
		recovery: policy, currentTag: currentTag, members: make(map[string]*groupMemberRuntime, len(members)),
	}
	if runtime.window <= 0 {
		runtime.window = 5 * time.Minute
	}
	if runtime.threshold <= 0 {
		runtime.threshold = 3
	}
	now := time.Now()
	for tag, initial := range members {
		if initial.Evicted {
			globalEvictedNodes.Store(initial.NodeID, true)
		}
		_, globallyEvicted := globalEvictedNodes.Load(initial.NodeID)
		evicted := initial.Evicted || globallyEvicted
		evictedAt := initial.EvictedAt
		if evicted && evictedAt.IsZero() {
			// Persisted state may carry Evicted without a timestamp; anchoring
			// it keeps the recovery cooldown measurable across restarts.
			evictedAt = now
		}
		runtime.members[tag] = &groupMemberRuntime{
			standby: initial.Standby,
			nodeID:  initial.NodeID, tag: tag, failureHistory: append([]int64(nil), initial.FailureHistory...),
			evicted: evicted, lastError: initial.LastError, evictedAt: evictedAt,
		}
	}
	if current := runtime.members[runtime.currentTag]; current == nil || current.standby || current.evicted || len(current.failureHistory) > 0 {
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
	return member != nil && !member.standby && !member.evicted && len(member.failureHistory) == 0
}

// MemberNotEvicted is the optimistic-startup gate. Pending members may ignore
// a recoverable failure history, but an explicit/permanent eviction is always
// authoritative.
func MemberNotEvicted(groupID int64, tag string) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return true
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	member := runtime.members[tag]
	return member != nil && !member.standby && !member.evicted
}

func CurrentTag(groupID int64) string {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return ""
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	if member := runtime.members[runtime.currentTag]; member != nil && !member.standby && !member.evicted {
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
	if member := runtime.members[tag]; tag != "" && (member == nil || member.standby || member.evicted) {
		runtime.mu.Unlock()
		return
	}
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
		if member != nil {
			// A fresh failure invalidates any recovery streak, including one
			// observed while the member was still evicted.
			member.recoverySuccesses = 0
		}
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
	member.recoverySuccesses = 0
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

// RecordHealthSuccess feeds one successful health probe into the group state.
//
// A healthy member simply clears its consecutive-failure streak. An evicted
// member counts consecutive successes instead and is re-admitted once the
// recovery policy is satisfied, which is what makes eviction recoverable
// without operator intervention. Recovery progress is deliberately not
// announced: the candidate topology has not changed yet, and the management API
// reads the progress from the runtime snapshot.
func RecordHealthSuccess(groupID int64, tag string) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if !ok {
		return false
	}
	runtime := value.(*groupRuntime)
	runtime.mu.Lock()
	member := runtime.members[tag]
	if member == nil {
		runtime.mu.Unlock()
		return false
	}
	if member.evicted {
		if !runtime.recovery.Enabled {
			if member.recoverySuccesses != 0 {
				member.recoverySuccesses = 0
			}
			runtime.mu.Unlock()
			return false
		}
		if time.Since(member.evictedAt) < runtime.recovery.Cooldown {
			runtime.mu.Unlock()
			return false
		}
		member.recoverySuccesses++
		if member.recoverySuccesses < runtime.recovery.SuccessThreshold {
			runtime.mu.Unlock()
			return false
		}
		nodeID := member.nodeID
		clearMemberEviction(member)
		runtime.mu.Unlock()
		globalEvictedNodes.Delete(nodeID)
		notifyGroupState(GroupStateEvent{GroupID: groupID, NodeID: nodeID, StateChanged: true, Recovered: true})
		recoverEvictedNodeEverywhere(nodeID, groupID)
		return true
	}
	if len(member.failureHistory) == 0 {
		member.recoverySuccesses = 0
		runtime.mu.Unlock()
		return true
	}
	member.failureHistory = nil
	member.lastError = ""
	member.recoverySuccesses = 0
	event := GroupStateEvent{GroupID: groupID, NodeID: member.nodeID, StateChanged: true, Recovered: true}
	runtime.mu.Unlock()
	notifyGroupState(event)
	return true
}

// clearMemberEviction returns a member to a clean, never-failed state. It must
// be called with the owning runtime lock held.
func clearMemberEviction(member *groupMemberRuntime) {
	member.evicted = false
	member.evictedAt = time.Time{}
	member.failureHistory = nil
	member.lastError = ""
	member.recoverySuccesses = 0
}

// recoverEvictedNodeEverywhere clears the same NodeID's eviction in every other
// group runtime. Eviction is propagated globally, so a recovery proved by one
// group has to be global too; otherwise the node would stay banned in every
// group whose own probe schedule happened to lag behind.
func recoverEvictedNodeEverywhere(nodeID, originGroupID int64) {
	if nodeID == 0 {
		return
	}
	globalEvictedNodes.Delete(nodeID)
	groupRuntimeStore.Range(func(_, value any) bool {
		runtime := value.(*groupRuntime)
		if runtime.id == originGroupID {
			return true
		}
		runtime.mu.Lock()
		recovered := false
		for _, member := range runtime.members {
			if member.nodeID != nodeID || !member.evicted {
				continue
			}
			clearMemberEviction(member)
			recovered = true
		}
		runtime.mu.Unlock()
		if recovered {
			notifyGroupState(GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, StateChanged: true, Recovered: true})
		}
		return true
	})
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
// It is the manual counterpart of the automatic recovery driven by
// RecordHealthSuccess.
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
		restored := false
		for _, member := range runtime.members {
			if member.nodeID != nodeID {
				continue
			}
			found = true
			clearMemberEviction(member)
			restored = true
		}
		runtime.mu.Unlock()
		if restored {
			notifyGroupState(GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, StateChanged: true, Recovered: true})
		}
		return true
	})
	if !found {
		return errors.New("group member not found")
	}
	return nil
}

// GroupRuntimeSnapshots returns all live group states.
func GroupRuntimeSnapshots() map[int64]GroupRuntimeSnapshot {
	return groupRuntimeSnapshots(true)
}

// GroupRuntimeSnapshotsReadonly never prunes state or emits events. Dry-run
// planning must remain an observation even when a failure window has expired.
func GroupRuntimeSnapshotsReadonly() map[int64]GroupRuntimeSnapshot {
	return groupRuntimeSnapshots(false)
}

func groupRuntimeSnapshots(prune bool) map[int64]GroupRuntimeSnapshot {
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
			if prune && !member.evicted && pruneFailures(member, runtime.window, time.Now()) {
				expiredEvents = append(expiredEvents, GroupStateEvent{GroupID: runtime.id, NodeID: member.nodeID,
					FailureHistory: append([]int64(nil), member.failureHistory...), LastError: member.lastError, StateChanged: true})
			}
			status := "ALIVE"
			if member.evicted {
				status = "EVICTED"
			} else if len(member.failureHistory) > 0 {
				status = "SUSPECT"
			}
			snapshotMember := GroupMemberSnapshot{NodeID: member.nodeID, Tag: member.tag,
				Standby: member.standby,
				Status:  status, FailureCount: len(member.failureHistory), FailureHistory: append([]int64(nil), member.failureHistory...),
				LastError: member.lastError, EvictedAt: member.evictedAt}
			if member.evicted && runtime.recovery.Enabled {
				snapshotMember.RecoverySuccesses = member.recoverySuccesses
				snapshotMember.RecoveryThreshold = runtime.recovery.SuccessThreshold
				if cooldownEnd := member.evictedAt.Add(runtime.recovery.Cooldown); cooldownEnd.After(time.Now()) {
					snapshotMember.RecoveryCooldownEnd = cooldownEnd
				}
			}
			snapshot.Members = append(snapshot.Members, snapshotMember)
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
			member.recoverySuccesses = 0
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
