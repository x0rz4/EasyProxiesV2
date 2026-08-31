package monitor

import (
	"sync"
	"testing"
	"time"
)

// TestProbeDueDoesNotReenterNodeLock is the regression test for nodes staying
// stuck at 待检查. RunProbeBatch used to enumerate the registry under its
// read lock and call probeDue, which re-entered that same lock. RWMutex forbids
// recursive read locking: once a writer is queued between the two acquisitions
// the inner RLock waits for the writer, the writer waits for the outer RLock,
// and the periodic health-check goroutine wedges permanently — no node ever
// reaches initial_check_done again, and every later Register, MigrateRuntimeTag,
// SweepStaleNodes or reload blocks behind it too.
//
// The test reproduces that exact interleaving deterministically.
func TestProbeDueDoesNotReenterNodeLock(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.healthMu.Lock()
	mgr.healthInterval = 10 * time.Minute
	mgr.healthMu.Unlock()
	mgr.RegisterGroupHealthSchedule(1, []string{"node-a"}, 2*time.Minute)
	handle := mgr.Register(NodeInfo{NodeID: 7, Tag: "node-a", Name: "node-a"})
	if handle == nil {
		t.Fatal("register returned no handle")
	}

	// Hold the registry for reading, exactly as the old batch enumeration did.
	mgr.registry.mu.RLock()

	// Queue a writer. Register/MigrateRuntimeTag do this on every reload and on
	// every group runtime start.
	var writer sync.WaitGroup
	writer.Add(1)
	writerQueued := make(chan struct{})
	go func() {
		defer writer.Done()
		close(writerQueued)
		mgr.Register(NodeInfo{NodeID: 8, Tag: "node-b", Name: "node-b"})
	}()
	<-writerQueued
	// Give the writer time to block on the registry write lock so a nested read
	// lock below has a pending writer ahead of it.
	time.Sleep(50 * time.Millisecond)

	returned := make(chan bool, 1)
	go func() {
		returned <- mgr.probeDue("node-a", 7, time.Now().Add(-3*time.Minute), time.Now())
	}()

	select {
	case due := <-returned:
		mgr.registry.mu.RUnlock()
		if !due {
			t.Fatal("group member was not due at its shorter interval")
		}
	case <-time.After(5 * time.Second):
		mgr.registry.mu.RUnlock()
		t.Fatal("probeDue blocked while the registry was read-locked: it must not enter the registry")
	}
	writer.Wait()
}

// TestProbeDueUsesCallerSuppliedNodeID pins the group-schedule lookup to the
// node ID handed in by the caller, since probeDue can no longer resolve it from
// the node map itself.
func TestProbeDueUsesCallerSuppliedNodeID(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.healthMu.Lock()
	mgr.healthInterval = 10 * time.Minute
	mgr.healthMu.Unlock()
	mgr.RegisterGroupHealthScheduleByNodeID(1, []int64{42}, 2*time.Minute)

	now := time.Now()
	lastCheck := now.Add(-3 * time.Minute)
	if !mgr.probeDue("any-tag", 42, lastCheck, now) {
		t.Fatal("node scheduled by ID was not due at its shorter interval")
	}
	if mgr.probeDue("any-tag", 43, lastCheck, now) {
		t.Fatal("unrelated node ID inherited another group's interval")
	}
}
