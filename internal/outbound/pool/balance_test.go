package pool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
)

func TestBalanceKeepsExactMinimumAndFirstIdleTie(t *testing.T) {
	p, snapshot := balanceBenchmarkPool(5, 5, modeBalance)
	for i, active := range []int32{7, 2, 2, 4, 8} {
		snapshot.Members[i].shared.active.Store(active)
	}
	if got := p.selectionIndex(snapshot, snapshot.tcpMembers); got != 1 {
		t.Fatalf("minimum index=%d", got)
	}
	snapshot.Members[2].shared.active.Store(0)
	snapshot.Members[4].shared.active.Store(0)
	if got := p.selectionIndex(snapshot, snapshot.tcpMembers); got != 2 {
		t.Fatalf("first idle index=%d", got)
	}
}

func TestTwoChoiceAlwaysComparesDifferentMembers(t *testing.T) {
	p, snapshot := balanceBenchmarkPool(2, 2, modeBalanceP2C)
	for _, counts := range [][2]int32{{1, 9}, {9, 1}, {0, 1000}, {1000, 0}} {
		snapshot.Members[0].shared.active.Store(counts[0])
		snapshot.Members[1].shared.active.Store(counts[1])
		want := 0
		if counts[1] < counts[0] {
			want = 1
		}
		for i := 0; i < 1000; i++ {
			if got := twoChoiceIndex(snapshot.tcpMembers, p.nextRandom()); got != want {
				t.Fatalf("counts=%v got=%d want=%d", counts, got, want)
			}
		}
	}
	if got := twoChoiceIndex(snapshot.Members[:1], 0); got != 0 {
		t.Fatalf("single member=%d", got)
	}
}

func TestTwoChoiceDistributionAndAccumulatedLoad(t *testing.T) {
	p, snapshot := balanceBenchmarkPool(64, 0, modeBalanceP2C)
	counts := make([]int, 64)
	const attempts = 128000
	for i := 0; i < attempts; i++ {
		counts[twoChoiceIndex(snapshot.tcpMembers, p.nextRandom())]++
	}
	for index, count := range counts {
		if count < 1600 || count > 2400 {
			t.Fatalf("unequal tie distribution: member=%d count=%d expected~2000", index, count)
		}
	}
	for i := 0; i < 64000; i++ {
		index := twoChoiceIndex(snapshot.tcpMembers, p.nextRandom())
		snapshot.tcpMembers[index].shared.active.Add(1)
	}
	var minActive, maxActive int32 = 64000, 0
	for _, member := range snapshot.tcpMembers {
		active := member.shared.active.Load()
		if active < minActive {
			minActive = active
		}
		if active > maxActive {
			maxActive = active
		}
	}
	if maxActive-minActive > 8 {
		t.Fatalf("accumulated load drift: min=%d max=%d", minActive, maxActive)
	}
}

func TestTwoChoiceConcurrentDispatchAndCandidateBoundary(t *testing.T) {
	p, snapshot := balanceBenchmarkPool(1024, 0, modeBalanceP2C)
	var invalid atomic.Int64
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 10000 {
				index := p.selectionIndex(snapshot, snapshot.tcpMembers)
				if index < 0 || index >= len(snapshot.tcpMembers) {
					invalid.Add(1)
					continue
				}
				state := snapshot.tcpMembers[index].shared
				state.active.Add(1)
				state.active.Add(-1)
			}
		})
	}
	workers.Wait()
	if invalid.Load() != 0 {
		t.Fatal("selected outside candidates")
	}
	for _, member := range snapshot.tcpMembers {
		if member.shared.active.Load() != 0 {
			t.Fatal("active accounting did not balance")
		}
	}
	blocked := &MemberRef{tag: "blocked", outbound: &fakeOutbound{}, shared: &sharedMemberState{blacklisted: true, blacklistedUntil: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)}}
	udp := &MemberRef{tag: "udp", outbound: &fakeOutbound{networks: []string{N.NetworkUDP}}}
	tcp := &MemberRef{tag: "tcp", outbound: &fakeOutbound{networks: []string{N.NetworkTCP}}}
	snapshot = storeTestSnapshot(p, blocked, udp, tcp)
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		cursor, err := p.newAttemptCursor(snapshot, network)
		if err != nil {
			t.Fatal(err)
		}
		member := cursor.next()
		if member == nil || member.tag != network || cursor.next() != nil {
			t.Fatalf("invalid %s candidates", network)
		}
	}
}

func TestTwoChoicePolicySurvivesPrepareAndCommit(t *testing.T) {
	ResetAllRuntimeState()
	t.Cleanup(ResetAllRuntimeState)
	manager := &capacityManager{members: map[string]adapter.Outbound{"one": &fakeOutbound{}}}
	p := &poolOutbound{manager: manager, ctx: context.Background(), mode: modeBalance, options: Options{Mode: modeBalance},
		topologyByTag: make(map[string]*MemberRef), runtimeByTag: make(map[string]*memberRuntime)}
	defer p.Close()
	update, err := p.PrepareUpdate(RuntimeUpdate{Mode: "BALANCE_P2C", Members: []RuntimeMemberSpec{{Tag: "one"}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.mode != modeBalance {
		t.Fatal("prepare mutated live policy")
	}
	if _, _, err := update.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshot := p.current.Load()
	if snapshot.mode != modeBalanceP2C || p.dataPathMode(snapshot) != modeBalanceP2C || p.selectionIndex(snapshot, snapshot.tcpMembers) != 0 {
		t.Fatal("committed policy was lost")
	}
}
