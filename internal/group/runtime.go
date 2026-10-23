package group

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
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
	// Revision is the ordering token this state was last written with, restored
	// from disk so a post-restart change still outranks what it replaces.
	Revision int64
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
	// Revision is the order this change was made in. Persisters must write it
	// through so a slow writer holding an older snapshot cannot overwrite a
	// newer transition.
	Revision int64
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
	// appliedProbes is a ring of the health ProbeIDs already applied to this
	// member. The health journal is delivered at least once (a crash between
	// callback return and cursor commit replays one batch), so applying the
	// same measurement twice must not advance a recovery streak twice.
	appliedProbes []string
	// revision stamps the durable fields above with the order in which this
	// runtime changed them. Two writers persist member state (the synchronous
	// health-delivery transaction and the asynchronous coalescing persister),
	// so without an ordering token a stalled writer could overwrite a recovery
	// with the eviction that preceded it.
	revision int64
}

// stateRevision assigns the next global state revision. It is global rather
// than per-member so revisions stay comparable across groups: an eviction
// propagates to every group holding the node.
var stateRevision atomic.Int64

// touch bumps the member revision. Callers must hold the owning runtime lock, so
// the assigned order matches the order the mutations actually happened in.
func (m *groupMemberRuntime) touch() int64 {
	m.revision = stateRevision.Add(1)
	return m.revision
}

// advanceStateRevision raises the global clock to at least revision. Restored
// state carries the value it was written with; without this the clock would
// restart at zero and every new write would be rejected as stale.
func advanceStateRevision(revision int64) {
	for {
		current := stateRevision.Load()
		if current >= revision || stateRevision.CompareAndSwap(current, revision) {
			return
		}
	}
}

// groupProbeRing bounds the per-member replay window. Delivery is in journal
// order and a resumed consumer restarts at its persisted cursor, so a replay is
// never deeper than one delivery batch (monitor.healthEventReadBatch).
const groupProbeRing = 64

// alreadyAppliedProbe reports whether probeID was already applied to this
// member, recording it when it is new. Must be called with the runtime lock
// held. An empty probeID means the caller has no event identity (manual or
// legacy path) and is never de-duplicated.
func (m *groupMemberRuntime) alreadyAppliedProbe(probeID string) bool {
	if probeID == "" {
		return false
	}
	for _, applied := range m.appliedProbes {
		if applied == probeID {
			return true
		}
	}
	if len(m.appliedProbes) >= groupProbeRing {
		m.appliedProbes = append(m.appliedProbes[:0], m.appliedProbes[1:]...)
	}
	m.appliedProbes = append(m.appliedProbes, probeID)
	return false
}

type groupRuntime struct {
	mu         sync.Mutex
	id         int64
	window     time.Duration
	threshold  int
	recovery   RecoveryPolicy
	currentTag string
	members    map[string]*groupMemberRuntime

	// Current-tag notification coalescing. Sequential/balanced/random dispatch
	// update currentTag on every dial; the value itself is always current in
	// memory, but the event fan-out (persistence observer + subscribers) must
	// not run on the per-connection path at full rate. Guarded by mu.
	lastCurrentNotify    time.Time
	currentNotifyPending bool
	currentPendingNodeID int64
}

// currentTagNotifyInterval caps how often CurrentChanged events are fanned out
// per group. Anything shorter only delays an informational update; failure and
// recovery events are never throttled.
const currentTagNotifyInterval = 500 * time.Millisecond

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
	// Subscriber callbacks run on data-plane paths (dial failure, dial
	// success); a panicking subscriber must not take the connection loop down.
	groupObserverMu.RLock()
	observer := groupObserver
	groupObserverMu.RUnlock()
	if observer != nil {
		runStateCallback(observer, event)
	}
	stateSubscriberMu.RLock()
	subscribers := make([]func(GroupStateEvent), 0, len(stateSubscribers))
	for _, subscriber := range stateSubscribers {
		subscribers = append(subscribers, subscriber)
	}
	stateSubscriberMu.RUnlock()
	for _, subscriber := range subscribers {
		runStateCallback(subscriber, event)
	}
}

func runStateCallback(callback func(GroupStateEvent), event GroupStateEvent) {
	defer func() { _ = recover() }()
	callback(event)
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
	removedNodeIDs := make([]int64, 0, len(oldByNodeID))
	updateMembersByNodeID := make(map[int64]struct{}, len(update.Members))
	for _, initial := range update.Members {
		updateMembersByNodeID[initial.NodeID] = struct{}{}
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
			// Carry the ordering token: this member's durable state did not
			// change, so it must not look newer (or older) than it is.
			member.revision = old.revision
		} else {
			member.failureHistory = append([]int64(nil), initial.FailureHistory...)
			member.evicted, member.lastError, member.evictedAt = initial.Evicted, initial.LastError, initial.EvictedAt
			if initial.Revision > 0 {
				// State restored from disk: resume its ordering token so the next
				// change outranks it and a stale writer cannot overwrite it.
				member.revision = initial.Revision
				advanceStateRevision(initial.Revision)
			}
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
				LastError: member.lastError, EvictedAt: member.evictedAt, StateChanged: true,
				Revision: member.touch()})
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
	for nodeID := range oldByNodeID {
		if _, stillMember := updateMembersByNodeID[nodeID]; !stillMember {
			removedNodeIDs = append(removedNodeIDs, nodeID)
		}
	}
	runtime.mu.Unlock()
	pruneGlobalEvictions(removedNodeIDs)
	return events
}

// pruneGlobalEvictions drops global eviction marks for nodes that are no
// longer members of any group runtime. A node removed from every group can
// never be probed back to health, so its mark would live forever — and
// re-adding the node later would immediately force it back into evicted
// state (Register and ReconcileSilent both honor the mark).
func pruneGlobalEvictions(removedNodeIDs []int64) {
	for _, nodeID := range removedNodeIDs {
		if _, marked := globalEvictedNodes.Load(nodeID); !marked {
			continue
		}
		stillEvictedSomewhere := false
		groupRuntimeStore.Range(func(_, value any) bool {
			runtime := value.(*groupRuntime)
			runtime.mu.Lock()
			for _, member := range runtime.members {
				if member.nodeID == nodeID && member.evicted {
					stillEvictedSomewhere = true
					break
				}
			}
			runtime.mu.Unlock()
			return !stillEvictedSomewhere
		})
		if !stillEvictedSomewhere {
			globalEvictedNodes.Delete(nodeID)
		}
	}
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
		member := &groupMemberRuntime{
			standby: initial.Standby,
			nodeID:  initial.NodeID, tag: tag, failureHistory: append([]int64(nil), initial.FailureHistory...),
			evicted: evicted, lastError: initial.LastError, evictedAt: evictedAt,
		}
		if initial.Revision > 0 {
			// Restored state already on disk: resume from its revision so a
			// later change still outranks it, and the global clock never
			// rewinds below a value that is already stored.
			member.revision = initial.Revision
			advanceStateRevision(initial.Revision)
		}
		runtime.members[tag] = member
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
			flush := runtime.currentNotifyPending && time.Since(runtime.lastCurrentNotify) >= currentTagNotifyInterval
			if flush {
				runtime.currentNotifyPending = false
				runtime.lastCurrentNotify = time.Now()
			}
			nodeID := runtime.currentPendingNodeID
			runtime.mu.Unlock()
			if flush {
				notifyGroupState(GroupStateEvent{GroupID: groupID, NodeID: nodeID, CurrentNodeID: nodeID, CurrentChanged: true})
			}
			return tag
		}
	}
	if runtime.currentTag == "" {
		runtime.mu.Unlock()
		return ""
	}
	runtime.currentTag = ""
	runtime.currentNotifyPending = false
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
	// Sequential/balanced/random dispatch hit this on every dial. The in-memory
	// value is always fresh, but the synchronous fan-out (persistence observer,
	// candidate rebuilds) must not run per connection: coalesce CurrentChanged
	// notifications to at most one per interval. Failure/recovery paths never
	// throttle.
	now := time.Now()
	if !runtime.lastCurrentNotify.IsZero() && now.Sub(runtime.lastCurrentNotify) < currentTagNotifyInterval {
		runtime.currentNotifyPending = true
		runtime.currentPendingNodeID = event.CurrentNodeID
		runtime.mu.Unlock()
		return
	}
	runtime.lastCurrentNotify = now
	runtime.currentNotifyPending = false
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
	// Concurrent dials against the same dead member report failures within one
	// wall-clock second. Counting each one evicts a member from a single burst
	// and diverges from the probe path's same-second dedup
	// (RecordGroupHealthFailure): one second is one evidence unit. The current
	// tag and recovery streak were already invalidated by the first failure of
	// this second, so an early return preserves those semantics.
	if len(history) > 0 && history[len(history)-1] == at.Unix() {
		evicted := member.evicted
		runtime.mu.Unlock()
		return evicted
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
		LastError: member.lastError, EvictedAt: member.evictedAt, CurrentChanged: currentChanged, StateChanged: true,
		Revision: member.touch()}
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
//
// probeID is the measurement identity carried by the health journal. A replay
// of an already-applied probe is a no-op, so a resumed consumer cannot count
// the same success twice.
func RecordHealthSuccess(groupID int64, tag, probeID string) bool {
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
	if member.alreadyAppliedProbe(probeID) {
		evicted := member.evicted
		runtime.mu.Unlock()
		return !evicted
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
		revision := member.touch()
		runtime.mu.Unlock()
		globalEvictedNodes.Delete(nodeID)
		notifyGroupState(GroupStateEvent{GroupID: groupID, NodeID: nodeID, StateChanged: true, Recovered: true, Revision: revision})
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
	event := GroupStateEvent{GroupID: groupID, NodeID: member.nodeID, StateChanged: true, Recovered: true,
		Revision: member.touch()}
	runtime.mu.Unlock()
	notifyGroupState(event)
	return true
}

// StateSnapshot is the durable part of one member's state, captured right after
// a health transition. The health consumer writes it in the same transaction as
// its delivery cursor: the cursor records that the transition was applied, so
// the state it produced must land with it. Commit one without the other and a
// crash either replays an applied eviction (deduplicated by ProbeID) or, worse,
// loses an eviction entirely so a bad node returns healthy after a restart.
type StateSnapshot struct {
	GroupID        int64
	NodeID         int64
	FailureHistory []int64
	Evicted        bool
	LastError      string
	EvictedAt      time.Time
	// Revision orders this snapshot against other writers of the same row.
	Revision int64
}

// SnapshotStates returns the durable state of every member the transition just
// touched, for the caller to persist atomically with its cursor. It reads the
// live runtimes after the transition, so the snapshot is always the state the
// transition produced.
//
// It scans every group, not just the one that observed the event: eviction and
// recovery are facts about a NodeID and propagate to every group holding it, so
// those rows are part of the same transition. Persisting them through the
// asynchronous path would let a crash keep the origin group's eviction and lose
// every other group's, which reintroduces the bad node in exactly the groups the
// operator is not looking at.
func SnapshotStates(nodeIDs []int64) []StateSnapshot {
	wanted := make(map[int64]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID != 0 {
			wanted[nodeID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	var states []StateSnapshot
	groupRuntimeStore.Range(func(_, value any) bool {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		seen := make(map[int64]struct{}, len(wanted))
		for _, member := range runtime.members {
			if _, ok := wanted[member.nodeID]; !ok {
				continue
			}
			// One node can appear under several runtime tags during a reload
			// window; collapse to the first so the caller writes one row per
			// group+node.
			if _, duplicate := seen[member.nodeID]; duplicate {
				continue
			}
			seen[member.nodeID] = struct{}{}
			states = append(states, StateSnapshot{
				GroupID: runtime.id, NodeID: member.nodeID,
				FailureHistory: append([]int64(nil), member.failureHistory...),
				Evicted:        member.evicted, LastError: member.lastError, EvictedAt: member.evictedAt,
				Revision: member.revision,
			})
		}
		runtime.mu.Unlock()
		return true
	})
	return states
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
		var revision int64
		for _, member := range runtime.members {
			if member.nodeID != nodeID || !member.evicted {
				continue
			}
			clearMemberEviction(member)
			revision = member.touch()
			recovered = true
		}
		runtime.mu.Unlock()
		if recovered {
			notifyGroupState(GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, StateChanged: true, Recovered: true, Revision: revision})
		}
		return true
	})
}

// RecordGroupHealthFailure feeds a failed background probe into every group
// containing the member. Repeated reports with the same second are de-duplicated,
// and a replayed probe (same probeID) is ignored entirely.
func RecordGroupHealthFailure(groupID int64, tag, probeID string, cause error, at time.Time) bool {
	value, ok := groupRuntimeStore.Load(groupID)
	if ok {
		runtime := value.(*groupRuntime)
		runtime.mu.Lock()
		member := runtime.members[tag]
		if member != nil && member.alreadyAppliedProbe(probeID) {
			evicted := member.evicted
			runtime.mu.Unlock()
			return evicted
		}
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
		var revision int64
		for _, member := range runtime.members {
			if member.nodeID != nodeID {
				continue
			}
			found = true
			clearMemberEviction(member)
			revision = member.touch()
			restored = true
		}
		runtime.mu.Unlock()
		if restored {
			notifyGroupState(GroupStateEvent{GroupID: runtime.id, NodeID: nodeID, StateChanged: true, Recovered: true, Revision: revision})
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
					FailureHistory: append([]int64(nil), member.failureHistory...), LastError: member.lastError, StateChanged: true,
					Revision: member.touch()})
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
				Evicted: true, LastError: lastError, EvictedAt: evictedAt, CurrentChanged: currentChanged, StateChanged: true,
				Revision: member.touch()}
			runtime.mu.Unlock()
			notifyGroupState(event)
			return true
		}
		runtime.mu.Unlock()
		return true
	})
}
