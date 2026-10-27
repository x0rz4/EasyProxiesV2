package pool

import (
	"slices"
	"testing"
	"time"

	N "github.com/sagernet/sing/common/network"
)

func TestSnapshotNetworkOrderingDoesNotMutateTopologyOrOlderIndexes(t *testing.T) {
	members := []*MemberRef{{tag: "a", outbound: &fakeOutbound{networks: []string{N.NetworkTCP, N.NetworkUDP}}}, {tag: "b", outbound: &fakeOutbound{networks: []string{N.NetworkTCP, N.NetworkUDP}}}, {tag: "c", outbound: &fakeOutbound{networks: []string{N.NetworkTCP, N.NetworkUDP}}}}
	for i, latency := range []int64{40, 10, 20} {
		members[i].latencyMs.Store(latency)
	}
	p := &poolOutbound{mode: modeLowestLatency}
	first := storeTestSnapshot(p, members...)
	if !slices.Equal(memberTags(first.allMembers), []string{"a", "b", "c"}) || !slices.Equal(memberTags(first.Members), []string{"a", "b", "c"}) {
		t.Fatal("latency sort mutated topology")
	}
	for _, view := range [][]*MemberRef{first.tcpMembers, first.udpMembers} {
		if !slices.Equal(memberTags(view), []string{"b", "c", "a"}) {
			t.Fatal("network view lost latency order")
		}
	}
	storeTestSnapshot(p, members[2], members[1], members[0])
	if first.memberIndex[members[0]] != 0 || first.tcpIndex[members[0]] != 2 || first.udpIndex[members[0]] != 2 {
		t.Fatal("later publication mutated old indexes")
	}
	if !slices.Equal(memberTags(first.allMembers), []string{"a", "b", "c"}) {
		t.Fatal("later publication mutated old topology")
	}
}

func TestSnapshotSubsetsPreserveAvailabilityAndProtocolIndexes(t *testing.T) {
	blocked := &MemberRef{tag: "blocked", outbound: &fakeOutbound{}, shared: &sharedMemberState{blacklisted: true, blacklistedUntil: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)}}
	a := &MemberRef{tag: "a", outbound: &fakeOutbound{networks: []string{N.NetworkTCP}}}
	b := &MemberRef{tag: "b", outbound: &fakeOutbound{networks: []string{N.NetworkUDP}}}
	c := &MemberRef{tag: "c", outbound: &fakeOutbound{networks: []string{N.NetworkTCP, N.NetworkUDP}}}
	p := &poolOutbound{mode: modeFixed}
	snapshot := storeTestSnapshot(p, blocked, a, b, c)
	if !slices.Equal(memberTags(snapshot.Members), []string{"a", "b", "c"}) || !slices.Equal(memberTags(snapshot.tcpMembers), []string{"a", "c"}) || !slices.Equal(memberTags(snapshot.udpMembers), []string{"b", "c"}) {
		t.Fatal("candidate eligibility changed")
	}
	if snapshot.memberIndex[c] != 2 || snapshot.tcpIndex[c] != 1 || snapshot.udpIndex[c] != 1 {
		t.Fatal("network index lost candidate order")
	}
	p.currentMember.Store(b)
	if got := p.selectionIndex(snapshot, snapshot.tcpMembers); got != 1 {
		t.Fatalf("fixed fallback selected %d", got)
	}
	emptyUDP := storeTestSnapshot(p, a)
	if len(emptyUDP.udpIndex) != 0 || len(emptyUDP.udpMembers) != 0 {
		t.Fatal("TCP member exposed as UDP candidate")
	}
}
