package pool

import (
	"testing"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
)

func monitorHealthEventAt(nodeID int64, probeID string, success bool, at time.Time) monitor.HealthResultEvent {
	return monitor.HealthResultEvent{NodeID: nodeID, ProbeID: probeID, Tag: "a", Success: success, CheckedAt: at}
}

// The health consumer must hand back the member state its transition produced,
// otherwise the cursor would commit "applied" while the eviction itself stays
// only in memory and a restart brings the node back healthy.
func TestHandleHealthResultReturnsDurableState(t *testing.T) {
	group.Reset()
	defer group.Reset()
	const groupID = int64(997)
	options := normalizeOptions(Options{GroupID: groupID, Members: []string{"a"},
		Metadata: map[string]MemberMeta{"a": {NodeID: 41}}})
	group.Register(groupID, time.Minute, 3, "", options.InitialGroupState)
	p := &poolOutbound{options: options, mode: modeFixed}
	p.topology = []*MemberRef{{tag: "a", meta: MemberMeta{NodeID: 41}}}
	p.topologyByTag = map[string]*MemberRef{"a": p.topology[0]}

	// Distinct seconds: concurrent reports inside one second are one evidence
	// unit by design, so a burst would not reach the threshold.
	at := time.Now()
	states := p.handleHealthResult(monitorHealthEventAt(41, "probe-1", false, at))
	if len(states) != 1 {
		t.Fatalf("failure produced %d states, want 1: %+v", len(states), states)
	}
	if states[0].GroupID != groupID || states[0].NodeID != 41 || len(states[0].FailureHistory) != 1 {
		t.Fatalf("unexpected state: %+v", states[0])
	}
	p.handleHealthResult(monitorHealthEventAt(41, "probe-2", false, at.Add(2*time.Second)))
	states = p.handleHealthResult(monitorHealthEventAt(41, "probe-3", false, at.Add(4*time.Second)))
	if len(states) != 1 || !states[0].Evicted {
		t.Fatalf("eviction not reported: %+v", states)
	}
	if states[0].Revision == 0 {
		t.Fatal("state carries no revision; a stale write could overwrite it")
	}

	// A read-only refresh owns no state.
	if got := p.handleHealthResult(monitor.HealthResultEvent{SnapshotOnly: true, CheckedAt: time.Now()}); got != nil {
		t.Fatalf("snapshot-only refresh returned states: %+v", got)
	}
	// A member that is not part of this pool contributes nothing.
	if got := p.handleHealthResult(monitorHealthEventAt(999, "probe-x", false, time.Now())); got != nil {
		t.Fatalf("event for an unknown node returned states: %+v", got)
	}
}
