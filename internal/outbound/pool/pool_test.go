package pool

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxlog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type halfCloseConn struct {
	net.Conn
	closeWriteCalls atomic.Int32
	closeReadCalls  atomic.Int32
}

type fakeOutbound struct {
	adapter.Outbound
	dial func(context.Context, string, M.Socksaddr) (net.Conn, error)
}

func (o *fakeOutbound) Network() []string { return []string{N.NetworkTCP} }

func (o *fakeOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	return o.dial(ctx, network, destination)
}

func TestDialContextFallsBackToAnotherMember(t *testing.T) {
	ResetSharedStateStore()
	t.Cleanup(ResetSharedStateStore)
	firstErr := errors.New("first unavailable")
	first := &memberState{
		tag:    "first",
		shared: acquireSharedState("first"),
		outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			return nil, firstErr
		}},
	}
	client, server := net.Pipe()
	defer server.Close()
	second := &memberState{
		tag:    "second",
		shared: acquireSharedState("second"),
		outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			return client, nil
		}},
	}
	p := &poolOutbound{
		logger:  boxlog.NewNOPFactory().Logger(),
		mode:    modeSequential,
		members: []*memberState{first, second},
		options: Options{FailureThreshold: 3, BlacklistDuration: time.Minute},
	}
	conn, err := p.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("example.com:443"))
	if err != nil {
		t.Fatal(err)
	}
	if first.shared.failures != 1 {
		t.Fatalf("first member failures = %d, want 1", first.shared.failures)
	}
	if second.shared.activeCount() != 1 {
		t.Fatalf("second member active count = %d, want 1", second.shared.activeCount())
	}
	_ = conn.Close()
}

func (c *halfCloseConn) CloseWrite() error {
	c.closeWriteCalls.Add(1)
	return nil
}

func (c *halfCloseConn) CloseRead() error {
	c.closeReadCalls.Add(1)
	return nil
}

func TestTrackedConnPreservesHalfClose(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	underlying := &halfCloseConn{Conn: left}
	var releases atomic.Int32
	conn := &trackedConn{Conn: underlying, release: func() { releases.Add(1) }}

	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseRead(); err != nil {
		t.Fatal(err)
	}
	if underlying.closeWriteCalls.Load() != 1 || underlying.closeReadCalls.Load() != 1 {
		t.Fatal("half-close methods were not forwarded")
	}
	if releases.Load() != 0 {
		t.Fatal("half-close released the active connection")
	}
	_ = conn.Close()
	_ = conn.Close()
	if releases.Load() != 1 {
		t.Fatalf("release called %d times, want 1", releases.Load())
	}
}

type failingConn struct {
	net.Conn
	err error
}

func (c *failingConn) Read([]byte) (int, error)  { return 0, c.err }
func (c *failingConn) Write([]byte) (int, error) { return 0, c.err }

func TestTrackedConnRecordsUnexpectedIOErrorOnce(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int32
	}{
		{name: "connection failure", err: errors.New("reset"), want: 1},
		{name: "EOF", err: io.EOF, want: 0},
		{name: "closed", err: net.ErrClosed, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var failures atomic.Int32
			conn := &trackedConn{
				Conn:    &failingConn{err: tc.err},
				release: func() {},
				onError: func(error) { failures.Add(1) },
			}
			_, _ = conn.Read(nil)
			_, _ = conn.Write(nil)
			if failures.Load() != tc.want {
				t.Fatalf("recorded %d failures, want %d", failures.Load(), tc.want)
			}
		})
	}
}

func TestActiveConnections(t *testing.T) {
	ResetSharedStateStore()
	t.Cleanup(ResetSharedStateStore)
	first := acquireSharedState("first")
	second := acquireSharedState("second")
	first.incActive()
	first.incActive()
	second.incActive()
	if got := ActiveConnections(); got != 3 {
		t.Fatalf("ActiveConnections() = %d, want 3", got)
	}
	first.decActive()
	second.decActive()
	first.decActive()
}

var _ net.Conn = (*halfCloseConn)(nil)
var _ interface{ CloseWrite() error } = (*trackedConn)(nil)
var _ interface{ CloseRead() error } = (*trackedConn)(nil)
