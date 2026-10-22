package pool

import (
	"reflect"
	"testing"
	"time"

	"easy_proxies/internal/group"
	json "easy_proxies/internal/jsonx"
	"github.com/sagernet/sing-box/adapter"
)

func TestCapacitySnapshotRetainsStandbyTopology(t *testing.T) {
	group.Reset()
	defer group.Reset()
	opts := normalizeOptions(Options{GroupID: 991, CapacityTarget: 1, Members: []string{"a", "b"},
		Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}})
	encoded, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Options
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CapacityTarget != 1 || !reflect.DeepEqual(decoded.ActiveNodeIDs, []int64{1}) || !decoded.InitialGroupState["b"].Standby {
		t.Fatalf("capacity round trip: %+v", decoded)
	}
	group.Register(991, time.Minute, 3, "a", decoded.InitialGroupState)
	p := &poolOutbound{options: decoded, mode: modeRandom}
	a, b := &MemberRef{tag: "a", meta: MemberMeta{NodeID: 1}}, &MemberRef{tag: "b", meta: MemberMeta{NodeID: 2}}
	first := p.publishSnapshotLocked([]*MemberRef{a, b})
	if len(first.allMembers) != 2 || len(first.Members) != 1 || first.Members[0].tag != "a" {
		t.Fatalf("first snapshot=%+v", first)
	}
	states := decoded.InitialGroupState
	applyCapacitySelection(1, []int64{2}, states)
	events := group.ReconcileSilent(991, group.RuntimeUpdate{FailureWindow: time.Minute, FailureThreshold: 3, Members: states})
	second := p.publishSnapshotLocked([]*MemberRef{a, b})
	group.NotifyEvents(events)
	if len(second.allMembers) != 2 || len(second.Members) != 1 || second.Members[0].tag != "b" {
		t.Fatalf("second snapshot=%+v", second)
	}
	if first.Members[0].tag != "a" {
		t.Fatal("published selection was mutated")
	}
	if group.MemberNotEvicted(991, "a") {
		t.Fatal("optimistic routing admitted standby")
	}
}

type capacityManager struct {
	adapter.OutboundManager
	members map[string]adapter.Outbound
}

func (m *capacityManager) Outbound(tag string) (adapter.Outbound, bool) {
	v, ok := m.members[tag]
	return v, ok
}

func TestCapacityCommitMissingMemberLeavesPolicyAndStateUnchanged(t *testing.T) {
	group.Reset()
	defer group.Reset()
	states := map[string]group.GroupInitialState{"a": {NodeID: 1}, "b": {NodeID: 2, Standby: true}}
	group.Register(992, time.Minute, 3, "a", states)
	manager := &capacityManager{members: map[string]adapter.Outbound{"a": &fakeOutbound{}, "b": &fakeOutbound{}}}
	p := &poolOutbound{manager: manager, options: Options{GroupID: 992, CapacityTarget: 1, ActiveNodeIDs: []int64{1}, Mode: modeFixed}, mode: modeFixed}
	prepared, err := p.PrepareUpdate(RuntimeUpdate{CapacityTarget: 1, ActiveNodeIDs: []int64{2}, Mode: modeRandom, FailureThreshold: 1,
		Members: []RuntimeMemberSpec{{Tag: "a", Meta: MemberMeta{NodeID: 1}}, {Tag: "b", Meta: MemberMeta{NodeID: 2}}}, InitialGroupState: states})
	if err != nil {
		t.Fatal(err)
	}
	before := group.GroupRuntimeSnapshots()[992]
	delete(manager.members, "b")
	if _, _, err := prepared.Commit(); err == nil {
		t.Fatal("missing outbound accepted")
	}
	prepared.Rollback()
	if !reflect.DeepEqual(before, group.GroupRuntimeSnapshots()[992]) || p.mode != modeFixed || p.options.Mode != modeFixed || !reflect.DeepEqual(p.options.ActiveNodeIDs, []int64{1}) {
		t.Fatal("failed commit partially changed capacity or group state")
	}
}

func TestCommitPublishesSnapshotBeforeGroupNotification(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ResetSharedStateStore()
	defer ResetSharedStateStore()
	states := map[string]group.GroupInitialState{"a": {NodeID: 1}, "b": {NodeID: 2, Standby: true}}
	group.Register(993, time.Minute, 3, "a", states)
	manager := &capacityManager{members: map[string]adapter.Outbound{"a": &fakeOutbound{}, "b": &fakeOutbound{}}}
	p := &poolOutbound{manager: manager, options: Options{GroupID: 993}, runtimeByTag: make(map[string]*memberRuntime)}
	notifications := 0
	unsubscribe := group.SubscribeStateChanges(func(event group.GroupStateEvent) {
		if event.GroupID != 993 {
			return
		}
		notifications++
		snapshot := p.current.Load()
		if snapshot == nil || len(snapshot.Members) != 1 || snapshot.Members[0].tag != "b" {
			t.Error("group notification preceded matching pool snapshot")
		}
	})
	defer unsubscribe()
	prepared, err := p.PrepareUpdate(RuntimeUpdate{CapacityTarget: 1, ActiveNodeIDs: []int64{2}, Mode: modeFixed,
		Members: []RuntimeMemberSpec{{Tag: "a", Meta: MemberMeta{NodeID: 1}}, {Tag: "b", Meta: MemberMeta{NodeID: 2}}}, InitialGroupState: states})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if notifications == 0 {
		t.Fatal("test did not exercise group notification")
	}
}
