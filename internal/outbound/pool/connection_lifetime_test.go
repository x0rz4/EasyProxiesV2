package pool

import (
	"context"
	"net"
	"testing"

	boxlog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type packetTestOutbound struct {
	fakeOutbound
	listen func() (net.PacketConn, error)
}

func (o *packetTestOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return o.listen()
}

func TestConnectionPinsOnlySelectedRuntime(t *testing.T) {
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		t.Run(network, func(t *testing.T) {
			ResetSharedStateStore()
			t.Cleanup(ResetSharedStateStore)
			started, proceed := make(chan struct{}), make(chan struct{})
			out := &packetTestOutbound{fakeOutbound: fakeOutbound{networks: []string{network}}}
			if network == N.NetworkTCP {
				client, server := net.Pipe()
				t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
				out.dial = func(context.Context, string, M.Socksaddr) (net.Conn, error) {
					close(started)
					<-proceed
					return client, nil
				}
			} else {
				packet, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = packet.Close() })
				out.listen = func() (net.PacketConn, error) {
					close(started)
					<-proceed
					return packet, nil
				}
			}
			selected := &MemberRef{tag: "selected", outbound: out, shared: acquireSharedState("selected")}
			other := &MemberRef{tag: "other", outbound: &fakeOutbound{}, shared: acquireSharedState("other")}
			p := &poolOutbound{mode: modeSequential, logger: boxlog.NewNOPFactory().Logger()}
			old := storeTestSnapshot(p, selected, other)
			t.Cleanup(func() { _ = p.Close() })
			selectedTicket, otherTicket := RetiredMember{ref: selected}, RetiredMember{ref: other}
			type result struct {
				conn interface{ Close() error }
				err  error
			}
			done := make(chan result, 1)
			go func() {
				if network == N.NetworkTCP {
					conn, err := p.DialContext(context.Background(), network, M.ParseSocksaddr("example.com:443"))
					done <- result{conn, err}
				} else {
					conn, err := p.ListenPacket(context.Background(), M.ParseSocksaddr("example.com:443"))
					done <- result{conn, err}
				}
			}()
			<-started
			storeTestSnapshot(p, &MemberRef{tag: "new", outbound: &fakeOutbound{}})
			selected.runtime.retired.Store(true)
			other.runtime.retired.Store(true)
			protected := !selectedTicket.Drained() && !otherTicket.Drained()
			close(proceed)
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			t.Cleanup(func() { _ = r.conn.Close() })
			if !protected {
				t.Fatal("in-flight dial did not protect the loaded topology")
			}
			if old.readers.Load() != 0 || !otherTicket.Drained() {
				t.Fatal("established connection still pins the old topology and unrelated member")
			}
			if selectedTicket.Drained() || selected.runtime.operations.Load() != 1 {
				t.Fatal("selected runtime is unprotected while its connection is open")
			}
			_ = r.conn.Close()
			_ = r.conn.Close()
			if !selectedTicket.Drained() || selected.runtime.operations.Load() != 0 || selected.shared.activeCount() != 0 {
				t.Fatal("connection close did not release selected runtime exactly once")
			}
		})
	}
}
