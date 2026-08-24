package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"
)

// sharedMemberState holds failure/blacklist state shared across all pool instances.
// This enables hybrid mode where pool and multi-port modes share the same node state.
type sharedMemberState struct {
	mu               sync.Mutex
	failures         int
	blacklisted      bool
	blacklistedUntil time.Time
	entry            atomic.Pointer[monitor.EntryHandle]
	active           atomic.Int32
	totalUpload      atomic.Int64
	totalDownload    atomic.Int64
}

var sharedStateStore sync.Map // map[tag]*sharedMemberState

// acquireSharedState returns the shared state for a tag, creating if needed.
func acquireSharedState(tag string) *sharedMemberState {
	if v, ok := sharedStateStore.Load(tag); ok {
		return v.(*sharedMemberState)
	}
	state := &sharedMemberState{}
	actual, _ := sharedStateStore.LoadOrStore(tag, state)
	return actual.(*sharedMemberState)
}

// lookupSharedState returns the shared state if it exists.
func lookupSharedState(tag string) (*sharedMemberState, bool) {
	v, ok := sharedStateStore.Load(tag)
	if !ok {
		return nil, false
	}
	return v.(*sharedMemberState), true
}

// ResetSharedStateStore clears all shared state (used during config reload).
func ResetSharedStateStore() {
	sharedStateStore.Range(func(key, _ any) bool {
		sharedStateStore.Delete(key)
		return true
	})
}

// ActiveConnections returns the number of currently open proxied connections.
func ActiveConnections() int32 {
	var total int32
	sharedStateStore.Range(func(_, value any) bool {
		total += value.(*sharedMemberState).activeCount()
		return true
	})
	return total
}

func (s *sharedMemberState) attachEntry(entry *monitor.EntryHandle) {
	if entry == nil {
		return
	}
	s.entry.Store(entry)
}

func (s *sharedMemberState) entryHandle() *monitor.EntryHandle {
	return s.entry.Load()
}

// recordFailure increments failure count and triggers blacklist if threshold reached.
// Returns: (current failures, blacklisted, blacklist until time)
func (s *sharedMemberState) recordFailure(cause error, threshold int, duration time.Duration, destination string) (int, bool, time.Time) {
	s.mu.Lock()
	s.failures++
	count := s.failures
	triggered := false
	var until time.Time
	if s.failures >= threshold {
		triggered = true
		until = time.Now().Add(duration)
		s.failures = 0
		s.blacklisted = true
		s.blacklistedUntil = until
	}
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordFailure(cause, destination)
		if triggered {
			entry.Blacklist(until)
		}
	}
	return count, triggered, until
}

func (s *sharedMemberState) recordSuccess(destination string) {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordSuccess(destination)
	}
}

// isBlacklisted checks if the node is currently blacklisted, auto-clearing if expired.
func (s *sharedMemberState) isBlacklisted(now time.Time) bool {
	s.mu.Lock()
	expired := s.blacklisted && now.After(s.blacklistedUntil)
	if expired {
		s.blacklisted = false
		s.blacklistedUntil = time.Time{}
	}
	blacklisted := s.blacklisted
	s.mu.Unlock()

	if expired {
		if entry := s.entry.Load(); entry != nil {
			entry.ClearBlacklist()
		}
	}
	return blacklisted
}

func (s *sharedMemberState) forceRelease() {
	s.mu.Lock()
	s.failures = 0
	s.blacklisted = false
	s.blacklistedUntil = time.Time{}
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.ClearBlacklist()
	}
}

func (s *sharedMemberState) incActive() {
	s.active.Add(1)
	if entry := s.entry.Load(); entry != nil {
		entry.IncActive()
	}
}

func (s *sharedMemberState) decActive() {
	s.active.Add(-1)
	if entry := s.entry.Load(); entry != nil {
		entry.DecActive()
	}
}

func (s *sharedMemberState) activeCount() int32 {
	return s.active.Load()
}

func (s *sharedMemberState) addTraffic(upload, download int64) {
	if upload > 0 {
		s.totalUpload.Add(upload)
	}
	if download > 0 {
		s.totalDownload.Add(download)
	}
	if entry := s.entry.Load(); entry != nil {
		entry.AddTraffic(upload, download)
	}
}

// releaseSharedMember clears blacklist state for a tag (called from release functions).
func releaseSharedMember(tag string) {
	if state, ok := lookupSharedState(tag); ok {
		state.forceRelease()
	}
}
