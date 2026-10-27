package pool

import (
	"fmt"
	"testing"
)

func balanceBenchmarkPool(size, busy int, mode string) (*poolOutbound, *PoolSnapshot) {
	members := make([]*MemberRef, size)
	for i := range members {
		members[i] = &MemberRef{tag: fmt.Sprint(i), outbound: &fakeOutbound{}, shared: &sharedMemberState{}}
		if i < busy {
			members[i].shared.active.Store(1)
		}
	}
	p := &poolOutbound{mode: mode}
	p.randomState.Store(1)
	return p, storeTestSnapshot(p, members...)
}

func BenchmarkBalanceSelection(b *testing.B) {
	benchmarkBalanceSelection(b, modeBalance)
}

func BenchmarkTwoChoiceSelection(b *testing.B) {
	benchmarkBalanceSelection(b, modeBalanceP2C)
}

func benchmarkBalanceSelection(b *testing.B, mode string) {
	for _, size := range []int{1024, 10000} {
		for _, load := range []struct {
			name string
			busy int
		}{{"idle", 0}, {"busy_prefix", 32}, {"all_busy", size}} {
			b.Run(fmt.Sprintf("%d/%s", size, load.name), func(b *testing.B) {
				p, snapshot := balanceBenchmarkPool(size, load.busy, mode)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					_ = p.selectionIndex(snapshot, snapshot.tcpMembers)
				}
			})
		}
	}
}

func BenchmarkBalanceConcurrentDispatch(b *testing.B) {
	benchmarkBalanceDispatch(b, modeBalance)
}

func BenchmarkTwoChoiceConcurrentDispatch(b *testing.B) {
	benchmarkBalanceDispatch(b, modeBalanceP2C)
}

func benchmarkBalanceDispatch(b *testing.B, mode string) {
	for _, size := range []int{1024, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			p, snapshot := balanceBenchmarkPool(size, size, mode)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					member := snapshot.tcpMembers[p.selectionIndex(snapshot, snapshot.tcpMembers)]
					member.shared.active.Add(1)
					member.shared.active.Add(-1)
				}
			})
		})
	}
}
