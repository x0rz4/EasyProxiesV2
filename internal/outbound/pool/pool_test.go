package pool

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"

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

type fakeSelector struct {
	adapter.Outbound
	selected string
}

func (s *fakeSelector) SelectOutbound(tag string) bool { s.selected = tag; return true }
func (s *fakeSelector) Now() string                    { return s.selected }

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

func TestFixedSelectionKeepsHealthyCurrentThenUsesLowestLatency(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	aEntry := mgr.Register(monitor.NodeInfo{Tag: "a"})
	bEntry := mgr.Register(monitor.NodeInfo{Tag: "b"})
	aEntry.RecordSuccessWithLatency(80 * time.Millisecond)
	bEntry.RecordSuccessWithLatency(10 * time.Millisecond)
	group.Register(20, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	a := &memberState{tag: "a", shared: acquireSharedState("a")}
	b := &memberState{tag: "b", shared: acquireSharedState("b")}
	p := &poolOutbound{mode: modeFixed, monitor: mgr, options: Options{GroupID: 20,
		Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}}}
	if selected := p.selectMember([]*memberState{a, b}); selected != a {
		t.Fatalf("healthy current changed to %s", selected.tag)
	}
	group.SetCurrentTag(20, "")
	if selected := p.selectMember([]*memberState{a, b}); selected != b {
		t.Fatalf("replacement = %s, want lowest-latency b", selected.tag)
	}
}

func TestHealthFailureHotSwitchesSelector(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	aEntry := mgr.Register(monitor.NodeInfo{Tag: "a"})
	bEntry := mgr.Register(monitor.NodeInfo{Tag: "b"})
	aEntry.MarkInitialCheckDone(false)
	bEntry.RecordSuccessWithLatency(15 * time.Millisecond)
	group.Register(21, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeFixed, monitor: mgr, selector: selector,
		options: Options{GroupID: 21, Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}},
		members: []*memberState{{tag: "a", shared: acquireSharedState("a")}, {tag: "b", shared: acquireSharedState("b")}},
	}
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "a", Error: "down", CheckedAt: time.Now()})
	if selector.selected != "b" || group.CurrentTag(21) != "b" {
		t.Fatalf("selector=%q current=%q, want b", selector.selected, group.CurrentTag(21))
	}
}

func TestRandomHealthSuccessDoesNotRotateHealthyCurrent(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(20 * time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "b"}).RecordSuccessWithLatency(10 * time.Millisecond)
	group.Register(23, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeRandom, monitor: mgr, selector: selector,
		options: Options{GroupID: 23},
		members: []*memberState{{tag: "a", shared: acquireSharedState("a")}, {tag: "b", shared: acquireSharedState("b")}},
	}
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "b", Success: true, CheckedAt: time.Now()})
	if selector.selected != "a" || group.CurrentTag(23) != "a" {
		t.Fatalf("healthy random current rotated: selector=%q current=%q", selector.selected, group.CurrentTag(23))
	}
}

func TestManualActivationUsesSelectorAndRejectsUnhealthyMember(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(20 * time.Millisecond)
	bEntry := mgr.Register(monitor.NodeInfo{Tag: "b"})
	bEntry.RecordSuccessWithLatency(10 * time.Millisecond)
	group.Register(24, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeFixed, monitor: mgr, selector: selector,
		options: Options{GroupID: 24, Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}},
		members: []*memberState{{tag: "a", shared: acquireSharedState("a")}, {tag: "b", shared: acquireSharedState("b")}},
	}
	unregister := group.RegisterActivationHandler(24, p.activateNodeID)
	defer unregister()
	if err := group.ActivateMember(24, 2); err != nil {
		t.Fatal(err)
	}
	if selector.selected != "b" || group.CurrentTag(24) != "b" {
		t.Fatalf("selector=%q current=%q, want b", selector.selected, group.CurrentTag(24))
	}
	bEntry.MarkInitialCheckDone(false)
	if err := group.ActivateMember(24, 2); err == nil {
		t.Fatal("unhealthy member was manually activated")
	}
}

func TestEstablishedIOErrorDoesNotEvictGroupMember(t *testing.T) {
	group.Reset()
	defer group.Reset()
	group.Register(22, 5*time.Minute, 1, "a", map[string]group.GroupInitialState{"a": {NodeID: 1}})
	p := &poolOutbound{logger: boxlog.NewNOPFactory().Logger(), options: Options{GroupID: 22}}
	p.recordEstablishedIOError(&memberState{tag: "a", shared: acquireSharedState("a")}, errors.New("remote reset"), "example.com:443")
	member := group.GroupRuntimeSnapshots()[22].Members[0]
	if member.Status != "ALIVE" || member.FailureCount != 0 {
		t.Fatalf("established I/O error changed group state: %+v", member)
	}
}

var _ net.Conn = (*halfCloseConn)(nil)
var _ interface{ CloseWrite() error } = (*trackedConn)(nil)
var _ interface{ CloseRead() error } = (*trackedConn)(nil)
