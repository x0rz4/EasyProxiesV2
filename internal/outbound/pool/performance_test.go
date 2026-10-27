package pool

import (
	"context"
	"net"
	"strconv"
	"testing"

	boxlog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type benchmarkConn struct{ net.Conn }

func (*benchmarkConn) Close() error { return nil }

// Exclude protocol/network work to measure selection, accounting and wrapping.
func BenchmarkPoolDialClose(b *testing.B) {
	conn := &benchmarkConn{}
	out := &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) { return conn, nil }}
	members := make([]*MemberRef, 1024)
	for i := range members {
		members[i] = &MemberRef{tag: strconv.Itoa(i), outbound: out, shared: &sharedMemberState{}}
	}
	p := &poolOutbound{mode: modeSequential, logger: boxlog.NewNOPFactory().Logger()}
	storeTestSnapshot(p, members...)
	destination := M.ParseSocksaddr("example.com:443")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c, err := p.DialContext(context.Background(), N.NetworkTCP, destination)
		if err != nil {
			b.Fatal(err)
		}
		_ = c.Close()
	}
}

// The current member and the following half of the topology cannot serve TCP.
// This exercises fixed-mode failover rather than the healthy-current fast path.
func BenchmarkFixedFailover(b *testing.B) {
	for _, size := range []int{1024, 4096} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			members := make([]*MemberRef, size)
			for i := range members {
				networks := []string{N.NetworkTCP}
				if i < size/2 {
					networks = []string{N.NetworkUDP}
				}
				members[i] = &MemberRef{tag: strconv.Itoa(i), outbound: &fakeOutbound{networks: networks}}
			}
			p := &poolOutbound{mode: modeFixed}
			snapshot := storeTestSnapshot(p, members...)
			p.currentMember.Store(members[0])
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if got := p.selectionIndex(snapshot, snapshot.tcpMembers); got != 0 {
					b.Fatalf("selected %d, want first TCP member", got)
				}
			}
		})
	}
}

func TestFixedFailoverPreservesTopologyAndCandidateIndexes(t *testing.T) {
	members := make([]*MemberRef, 5)
	for i := range members {
		networks := []string{N.NetworkUDP}
		if i == 0 || i == 3 {
			networks = []string{N.NetworkTCP}
		}
		members[i] = &MemberRef{tag: strconv.Itoa(i), outbound: &fakeOutbound{networks: networks}}
	}
	p := &poolOutbound{mode: modeFixed}
	snapshot := storeTestSnapshot(p, members...)
	for _, tt := range []struct {
		name       string
		previous   *MemberRef
		candidates []*MemberRef
		want       int
	}{
		{"tcp", members[1], snapshot.tcpMembers, 1},
		{"tcp_wrap", members[4], snapshot.tcpMembers, 0},
		{"udp", members[0], snapshot.udpMembers, 0},
		{"udp_wrap", members[3], snapshot.udpMembers, 2},
		{"reordered", members[1], []*MemberRef{members[3], members[0]}, 0},
		{"duplicate", members[1], []*MemberRef{members[3], members[0], members[3]}, 0},
		{"absent_current", &MemberRef{tag: "removed"}, snapshot.tcpMembers, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.nextFixedIndex(snapshot, tt.candidates, tt.previous); got != tt.want {
				t.Fatalf("candidate index = %d, want %d", got, tt.want)
			}
		})
	}
}
