package pool

import (
	"fmt"
	"testing"

	N "github.com/sagernet/sing/common/network"
)

func BenchmarkSnapshotPublication(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		for _, kind := range []string{"tcp", "dual", "mixed"} {
			b.Run(fmt.Sprintf("%d/%s", size, kind), func(b *testing.B) {
				members := make([]*MemberRef, size)
				for i := range members {
					networks := []string{N.NetworkTCP}
					if kind == "dual" {
						networks = append(networks, N.NetworkUDP)
					}
					if kind == "mixed" && i%2 != 0 {
						networks = []string{N.NetworkUDP}
					}
					members[i] = &MemberRef{tag: fmt.Sprint(i), outbound: &fakeOutbound{networks: networks}, shared: &sharedMemberState{}}
				}
				p := &poolOutbound{mode: modeFixed}
				storeTestSnapshot(p, members...)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					p.publishSnapshotLocked(members)
				}
			})
		}
	}
}
