package monitor

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// membershipNodeManager is a runtime that supports incremental group rebuilds.
type membershipNodeManager struct {
	reloadNodeManager
	changed [][]int64
	fail    error
}

func (m *membershipNodeManager) ApplyGroupMembershipChanges(_ context.Context, changedNodeIDs []int64) error {
	m.changed = append(m.changed, append([]int64(nil), changedNodeIDs...))
	return m.fail
}

// TestRefreshGroupMembershipPrefersIncrementalPath pins the reason the optional
// interface exists: a node fact changing must rebuild only the affected group
// listeners, and must fall back to a full reload only when the runtime cannot do
// that at all.
func TestRefreshGroupMembershipPrefersIncrementalPath(t *testing.T) {
	incremental := &membershipNodeManager{}
	server := &Server{nodeMgr: incremental}
	if err := server.refreshGroupMembership([]int64{7, 9}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(incremental.changed, [][]int64{{7, 9}}) {
		t.Fatalf("incremental calls = %v, want one call with [7 9]", incremental.changed)
	}
	if reloads := incremental.reloads.Load(); reloads != 0 {
		t.Fatalf("incremental path triggered %d full reloads", reloads)
	}

	// A runtime without the interface still has to see the change, even though the
	// only tool it has is the expensive one.
	legacy := &reloadNodeManager{}
	if err := (&Server{nodeMgr: legacy}).refreshGroupMembership([]int64{7}); err != nil {
		t.Fatal(err)
	}
	if reloads := legacy.reloads.Load(); reloads != 1 {
		t.Fatalf("fallback reloads = %d, want 1", reloads)
	}

	// Nothing changed, nothing wired, and no server at all are all no-ops: this is
	// called from detection goroutines that do not know the shutdown order.
	if err := (&Server{nodeMgr: legacy}).refreshGroupMembership(nil); err != nil {
		t.Fatal(err)
	}
	if reloads := legacy.reloads.Load(); reloads != 1 {
		t.Fatalf("an empty change set triggered a reload")
	}
	if err := (&Server{}).refreshGroupMembership([]int64{7}); err != nil {
		t.Fatal(err)
	}
	var absent *Server
	if err := absent.refreshGroupMembership([]int64{7}); err != nil {
		t.Fatal(err)
	}

	// A failure is reported rather than silently downgraded to a full reload: the
	// caller surfaces it as a warning on the check that produced it.
	broken := &membershipNodeManager{fail: errors.New("boom")}
	if err := (&Server{nodeMgr: broken}).refreshGroupMembership([]int64{7}); err == nil {
		t.Fatal("membership failure was swallowed")
	}
	if reloads := broken.reloads.Load(); reloads != 0 {
		t.Fatalf("failed membership refresh fell back to %d full reloads", reloads)
	}
}
