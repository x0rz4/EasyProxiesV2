package pool

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"

	"github.com/sagernet/sing-box/adapter"
	boxlog "github.com/sagernet/sing-box/log"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func testProbeTarget(t *testing.T, raw string) monitor.ProbeTarget {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	requestURI := parsed.RequestURI()
	if requestURI == "" {
		requestURI = "/generate_204"
	}
	return monitor.ProbeTarget{
		Scheme: parsed.Scheme, Host: parsed.Host, ServerName: parsed.Hostname(), RequestURI: requestURI,
		Destination: M.ParseSocksaddrHostPort(parsed.Hostname(), uint16(port)),
	}
}

func TestNormalizeOptionsAlignsInitialStateWithMembers(t *testing.T) {
	originalHistory := []int64{1, 2}
	options := normalizeOptions(Options{GroupID: 9, Members: []string{"node-a", "node-b"},
		Metadata: map[string]MemberMeta{"node-a": {NodeID: 1}, "node-b": {NodeID: 2}},
		InitialGroupState: map[string]group.GroupInitialState{
			"node-a": {NodeID: 1, FailureHistory: originalHistory},
			"stale":  {NodeID: 3, Evicted: true},
		}})
	if len(options.InitialGroupState) != 2 {
		t.Fatalf("initial state=%v", options.InitialGroupState)
	}
	if _, ok := options.InitialGroupState["stale"]; ok {
		t.Fatal("stale initial state was retained")
	}
	if options.InitialGroupState["node-b"].NodeID != 2 {
		t.Fatalf("missing member was not initialized: %v", options.InitialGroupState["node-b"])
	}
	originalHistory[0] = 99
	if options.InitialGroupState["node-a"].FailureHistory[0] != 1 {
		t.Fatal("failure history was not defensively copied")
	}
}

func TestPoolDependenciesDoNotProtectRuntimeMembers(t *testing.T) {
	dependencies := poolDependencies(Options{
		Members:     []string{"0123456789abcdef@v1", "fedcba9876543210@v2"},
		SelectorTag: "group-selector-7",
	})
	if !slices.Equal(dependencies, []string{"group-selector-7"}) {
		t.Fatalf("pool dependencies=%v, want only static selector dependency", dependencies)
	}
	if dependencies := poolDependencies(Options{Members: []string{"0123456789abcdef@v1"}}); len(dependencies) != 0 {
		t.Fatalf("base pool dependencies=%v, want runtime members unprotected", dependencies)
	}
}

func TestHTTPProbeRequiresValid204Response(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			http.Redirect(w, r, "/portal", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("captive portal"))
		}
	}))
	defer server.Close()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "204", path: "/ok"},
		{name: "200 portal", path: "/portal", wantErr: true},
		{name: "302 redirect", path: "/redirect", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := testProbeTarget(t, server.URL+tc.path)
			conn, err := net.Dial("tcp", target.Destination.String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, err = httpProbe(t.Context(), conn, target, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("httpProbe error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestHTTPProbeRejectsNonHTTPBytes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		reader := make([]byte, 1024)
		_, _ = server.Read(reader)
		_, _ = server.Write([]byte("Xnot-http\r\n"))
	}()
	target := monitor.ProbeTarget{Scheme: "http", Host: "example.com", ServerName: "example.com",
		RequestURI: "/generate_204", Destination: M.ParseSocksaddr("example.com:80")}
	if _, err := httpProbe(t.Context(), client, target, nil); err == nil {
		t.Fatal("non-HTTP response passed health probe")
	}
}

func TestHTTPSProbePerformsTLSAndPreservesRequestURI(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connectivity" || r.URL.Query().Get("source") != "test" {
			http.Error(w, "wrong target", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	target := testProbeTarget(t, server.URL+"/connectivity?source=test")
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	conn, err := net.Dial("tcp", target.Destination.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := httpProbe(t.Context(), conn, target, &tls.Config{RootCAs: roots}); err != nil {
		t.Fatal(err)
	}
	untrustedConn, err := net.Dial("tcp", target.Destination.String())
	if err != nil {
		t.Fatal(err)
	}
	defer untrustedConn.Close()
	if _, err := httpProbe(t.Context(), untrustedConn, target, nil); err == nil {
		t.Fatal("HTTPS probe accepted an untrusted certificate")
	}
}

func TestHTTPProbeHonorsContextDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		buffer := make([]byte, 1024)
		_, _ = server.Read(buffer)
	}()
	target := monitor.ProbeTarget{Scheme: "http", Host: "example.com", ServerName: "example.com",
		RequestURI: "/generate_204", Destination: M.ParseSocksaddr("example.com:80")}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := httpProbe(ctx, client, target, nil); err == nil {
		t.Fatal("stalled response passed health probe")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe ignored context deadline: %s", elapsed)
	}
}

func TestProbeFunctionSupportsTargetConfiguredAfterPoolCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	member := &memberState{tag: "node", outbound: &fakeOutbound{dial: func(_ context.Context, _ string, destination M.Socksaddr) (net.Conn, error) {
		return net.Dial("tcp", destination.String())
	}}}
	p := &poolOutbound{monitor: mgr}
	probe := p.makeProbeFunc(member)
	if probe == nil {
		t.Fatal("pool created without a target has no dynamic probe function")
	}
	if err := mgr.UpdateProbeTarget(server.URL + "/generate_204"); err != nil {
		t.Fatal(err)
	}
	if _, err := probe(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestProbeMemberUsesIndependentDialDeadline(t *testing.T) {
	mgr, err := monitor.NewManager(monitor.Config{
		ProbeTarget: "http://example.com", ProbeDialTimeout: 25 * time.Millisecond,
		ProbeResponseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	member := &memberState{tag: "slow-dial", outbound: &fakeOutbound{dial: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	target, _ := mgr.TargetForProbe()
	started := time.Now()
	if _, err := (&poolOutbound{monitor: mgr}).probeMember(t.Context(), member, target); err == nil {
		t.Fatal("stalled dial passed health probe")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("dial phase ignored configured deadline: %s", elapsed)
	}
}

func TestProbeMemberUsesIndependentResponseDeadline(t *testing.T) {
	mgr, err := monitor.NewManager(monitor.Config{
		ProbeTarget: "http://example.com", ProbeDialTimeout: time.Second,
		ProbeResponseTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		buffer := make([]byte, 1024)
		_, _ = server.Read(buffer)
	}()
	member := &memberState{tag: "slow-response", outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		return client, nil
	}}}
	target, _ := mgr.TargetForProbe()
	started := time.Now()
	if _, err := (&poolOutbound{monitor: mgr}).probeMember(t.Context(), member, target); err == nil {
		t.Fatal("stalled response passed health probe")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("response phase ignored configured deadline: %s", elapsed)
	}
}

func TestBasePoolExcludesActiveProbeFailures(t *testing.T) {
	mgr, err := monitor.NewManager(monitor.Config{ProbeTarget: "http://example.com/generate_204"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "healthy"}).RecordSuccessWithLatency(time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "failed"}).MarkInitialCheckDone(false)
	healthy := &memberState{tag: "healthy", shared: acquireSharedState("healthy")}
	failed := &memberState{tag: "failed", shared: acquireSharedState("failed")}
	p := &poolOutbound{monitor: mgr}
	snapshot := storeTestSnapshot(p, healthy, failed)
	got := p.availableMembers(snapshot, time.Now(), "", nil)
	if len(got) != 1 || got[0] != healthy {
		t.Fatalf("available members=%v, want only healthy", memberTags(got))
	}
}

func TestDialSuccessDoesNotClearPassiveFailuresBeforeResponse(t *testing.T) {
	ResetSharedStateStore()
	t.Cleanup(ResetSharedStateStore)
	state := acquireSharedState("node")
	state.recordFailure(errors.New("reset"), 3, time.Minute, "example.com:443")
	state.recordSuccess("example.com:443")
	if state.failures != 1 {
		t.Fatalf("dial success cleared passive failures: %d", state.failures)
	}
	left, right := net.Pipe()
	defer right.Close()
	p := &poolOutbound{logger: boxlog.NewNOPFactory().Logger()}
	conn := p.wrapConn(left, &memberState{tag: "node", shared: state}, "example.com:443")
	defer conn.Close()
	go func() { _, _ = right.Write([]byte("response")) }()
	buffer := make([]byte, 8)
	if _, err := conn.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if state.failures != 0 {
		t.Fatalf("response bytes did not clear passive failures: %d", state.failures)
	}
}

func TestEstablishedIOFailuresAccumulateAcrossSuccessfulDials(t *testing.T) {
	ResetSharedStateStore()
	t.Cleanup(ResetSharedStateStore)
	member := &memberState{tag: "node", shared: acquireSharedState("node")}
	p := &poolOutbound{logger: boxlog.NewNOPFactory().Logger(), options: Options{FailureThreshold: 3, BlacklistDuration: time.Minute}}
	for range 3 {
		p.recordSuccess(member, "example.com:443")
		p.recordEstablishedIOError(member, errors.New("remote reset"), "example.com:443")
	}
	if !member.shared.isBlacklisted(time.Now()) {
		t.Fatal("established I/O failures never reached blacklist threshold")
	}
}

func memberTags(members []*memberState) []string {
	tags := make([]string, len(members))
	for i, member := range members {
		tags[i] = member.tag
	}
	return tags
}

func storeTestSnapshot(p *poolOutbound, members ...*memberState) *PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topology = append([]*memberState(nil), members...)
	p.topologyByTag = make(map[string]*MemberRef, len(members))
	for _, member := range members {
		p.topologyByTag[member.tag] = member
		if member.meta == (MemberMeta{}) {
			member.meta = p.options.Metadata[member.tag]
		}
		if p.monitor != nil {
			if snapshot := p.monitor.SnapshotForTag(member.tag); snapshot != nil {
				member.latencyMs.Store(snapshot.LastLatencyMs)
			}
		}
	}
	if current := p.topologyByTag[group.CurrentTag(p.options.GroupID)]; current != nil {
		p.currentMember.Store(current)
	}
	return p.publishSnapshotLocked(members)
}

func TestPoolSnapshotReplacementIsVersionedAndImmutable(t *testing.T) {
	p := &poolOutbound{}
	a := &memberState{tag: "a"}
	b := &memberState{tag: "b"}
	c := &memberState{tag: "c"}

	input := []*memberState{a, b}
	first := storeTestSnapshot(p, input...)
	input[0] = c
	second := storeTestSnapshot(p, b, c)

	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("snapshot versions = %d, %d; want 1, 2", first.Version, second.Version)
	}
	if got := memberTags(first.Members); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("first snapshot mutated after publication: %v", got)
	}
	if got := memberTags(p.current.Load().Members); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("current snapshot members = %v, want [b c]", got)
	}
}

func TestRetiredMemberWaitsForLoadedSnapshotReader(t *testing.T) {
	member := &memberState{tag: "old", outbound: &fakeOutbound{}, shared: acquireSharedState("old")}
	p := &poolOutbound{}
	storeTestSnapshot(p, member)
	_, release, err := p.acquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	storeTestSnapshot(p, &memberState{tag: "new", outbound: &fakeOutbound{}, shared: acquireSharedState("new")})
	member.runtime.retired.Store(true)
	retired := RetiredMember{ref: member}
	if retired.Drained() {
		t.Fatal("retired member drained while an old snapshot reader was paused")
	}
	release()
	if !retired.Drained() {
		refs, operations, active := retired.Counts()
		t.Fatalf("retired member did not drain after reader release: refs=%d operations=%d active=%d", refs, operations, active)
	}
}

func BenchmarkSelectMember1000(b *testing.B) {
	members := make([]*memberState, 1024)
	for index := range members {
		members[index] = &memberState{tag: strconv.Itoa(index), outbound: &fakeOutbound{}, shared: acquireSharedState(strconv.Itoa(index))}
	}
	p := &poolOutbound{mode: modeSequential}
	snapshot := storeTestSnapshot(p, members...)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = p.selectionIndex(snapshot, snapshot.tcpMembers)
	}
}

type halfCloseConn struct {
	net.Conn
	closeWriteCalls atomic.Int32
	closeReadCalls  atomic.Int32
}

type fakeOutbound struct {
	adapter.Outbound
	dial     func(context.Context, string, M.Socksaddr) (net.Conn, error)
	networks []string
}

type fakeSelector struct {
	adapter.Outbound
	selected string
	calls    atomic.Int32
}

func (s *fakeSelector) SelectOutbound(tag string) bool {
	s.calls.Add(1)
	s.selected = tag
	return true
}
func (s *fakeSelector) Now() string { return s.selected }

func (o *fakeOutbound) Network() []string {
	if len(o.networks) == 0 {
		return []string{N.NetworkTCP}
	}
	return o.networks
}

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
		options: Options{FailureThreshold: 3, BlacklistDuration: time.Minute},
	}
	storeTestSnapshot(p, first, second)
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

func TestLockFreeSelectionModesAndNetworkCandidates(t *testing.T) {
	a := &memberState{tag: "a", outbound: &fakeOutbound{networks: []string{N.NetworkTCP}}, shared: acquireSharedState("select-a")}
	bMember := &memberState{tag: "b", outbound: &fakeOutbound{networks: []string{N.NetworkTCP, N.NetworkUDP}}, shared: acquireSharedState("select-b")}
	c := &memberState{tag: "c", outbound: &fakeOutbound{networks: []string{N.NetworkUDP}}, shared: acquireSharedState("select-c")}
	p := &poolOutbound{mode: modeSequential}
	snapshot := storeTestSnapshot(p, a, bMember, c)
	if got := memberTags(snapshot.tcpMembers); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("TCP candidates = %v", got)
	}
	if got := memberTags(snapshot.udpMembers); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("UDP candidates = %v", got)
	}
	if first, second := p.selectionIndex(snapshot, snapshot.tcpMembers), p.selectionIndex(snapshot, snapshot.tcpMembers); first != 0 || second != 1 {
		t.Fatalf("sequential indexes = %d,%d", first, second)
	}
	// Legacy unit setup mutates p.mode directly; production snapshots carry an
	// immutable mode and are republished by Commit.
	snapshot.mode = ""

	p.mode = modeBalance
	a.shared.incActive()
	if selected := p.selectionIndex(snapshot, snapshot.tcpMembers); selected != 1 {
		t.Fatalf("balance selected index %d, want idle member 1", selected)
	}
	a.shared.decActive()

	p.mode = modeRandom
	var invalid atomic.Int32
	var done sync.WaitGroup
	for range 32 {
		done.Add(1)
		go func() {
			defer done.Done()
			for range 1000 {
				index := p.selectionIndex(snapshot, snapshot.tcpMembers)
				if index < 0 || index >= len(snapshot.tcpMembers) {
					invalid.Add(1)
				}
			}
		}()
	}
	done.Wait()
	if invalid.Load() != 0 {
		t.Fatalf("random selection returned %d invalid indexes", invalid.Load())
	}
}

func TestBlacklistExpiryRepublishesCandidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := acquireSharedState("expiry")
		member := &memberState{tag: "expiry", outbound: &fakeOutbound{}, shared: state}
		p := &poolOutbound{logger: boxlog.NewNOPFactory().Logger(), mode: modeSequential,
			options: Options{FailureThreshold: 1, BlacklistDuration: 20 * time.Millisecond}}
		initial := storeTestSnapshot(p, member)
		p.recordFailure(member, errors.New("down"), "test")
		if got := len(p.current.Load().Members); got != 0 {
			t.Fatalf("blacklisted snapshot has %d candidates", got)
		}
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		if got := len(p.current.Load().Members); got != 0 {
			t.Fatalf("candidate returned before guarded expiry: %d", got)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		expired := p.current.Load()
		if got := len(expired.Members); got != 1 {
			t.Fatalf("expired blacklist snapshot has %d candidates", got)
		}
		if expired.Version != initial.Version+2 {
			t.Fatalf("snapshot version = %d, want %d", expired.Version, initial.Version+2)
		}
		_ = p.Close()
	})
}

func TestHealthSuccessCoalescesCandidateRebuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		member := &memberState{tag: "healthy", outbound: &fakeOutbound{}, shared: acquireSharedState("healthy")}
		p := &poolOutbound{logger: boxlog.NewNOPFactory().Logger(), mode: modeLowestLatency}
		initial := storeTestSnapshot(p, member)

		p.handleHealthResult(monitor.HealthResultEvent{Tag: member.tag, Success: true, Latency: 20 * time.Millisecond})
		p.handleHealthResult(monitor.HealthResultEvent{Tag: member.tag, Success: true, Latency: 10 * time.Millisecond})
		time.Sleep(99 * time.Millisecond)
		synctest.Wait()
		if version := p.current.Load().Version; version != initial.Version {
			t.Fatalf("snapshot published before coalescing window: %d", version)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if version := p.current.Load().Version; version != initial.Version+1 {
			t.Fatalf("coalesced snapshot version = %d, want %d", version, initial.Version+1)
		}
		_ = p.Close()
	})
}

func TestDialContextKeepsLoadedSnapshotAcrossRetry(t *testing.T) {
	ResetSharedStateStore()
	t.Cleanup(ResetSharedStateStore)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := &memberState{
		tag:    "first",
		shared: acquireSharedState("first"),
		outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			close(firstStarted)
			<-releaseFirst
			return nil, errors.New("first unavailable")
		}},
	}
	client, server := net.Pipe()
	defer server.Close()
	var secondCalls atomic.Int32
	second := &memberState{
		tag:    "second",
		shared: acquireSharedState("second"),
		outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			secondCalls.Add(1)
			return client, nil
		}},
	}
	var replacementCalls atomic.Int32
	replacement := &memberState{
		tag:    "replacement",
		shared: acquireSharedState("replacement"),
		outbound: &fakeOutbound{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			replacementCalls.Add(1)
			return nil, errors.New("replacement must not be used by in-flight dial")
		}},
	}
	p := &poolOutbound{
		logger:  boxlog.NewNOPFactory().Logger(),
		mode:    modeSequential,
		options: Options{FailureThreshold: 3, BlacklistDuration: time.Minute},
	}
	firstSnapshot := storeTestSnapshot(p, first, second)

	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)
	go func() {
		conn, err := p.DialContext(t.Context(), N.NetworkTCP, M.ParseSocksaddr("example.com:443"))
		result <- dialResult{conn: conn, err: err}
	}()
	<-firstStarted
	secondSnapshot := storeTestSnapshot(p, replacement)
	close(releaseFirst)

	dialed := <-result
	if dialed.err != nil {
		t.Fatal(dialed.err)
	}
	defer dialed.conn.Close()
	if secondCalls.Load() != 1 || replacementCalls.Load() != 0 {
		t.Fatalf("retry calls: old second=%d replacement=%d", secondCalls.Load(), replacementCalls.Load())
	}
	if firstSnapshot.Version != 1 || secondSnapshot.Version != 2 || p.current.Load() != secondSnapshot {
		t.Fatalf("unexpected snapshots: first=%d second=%d current=%p", firstSnapshot.Version, secondSnapshot.Version, p.current.Load())
	}
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

func TestFixedSelectionKeepsHealthyCurrentThenUsesMemberOrder(t *testing.T) {
	group.Reset()
	defer group.Reset()
	group.Register(20, 5*time.Minute, 3, "b", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2}, "c": {NodeID: 3}, "d": {NodeID: 4},
	})
	a := &memberState{tag: "a", shared: acquireSharedState("a")}
	b := &memberState{tag: "b", shared: acquireSharedState("b")}
	c := &memberState{tag: "c", shared: acquireSharedState("c")}
	d := &memberState{tag: "d", shared: acquireSharedState("d")}
	selector := &fakeSelector{selected: "b"}
	p := &poolOutbound{mode: modeFixed, selector: selector, options: Options{GroupID: 20}}
	snapshot := storeTestSnapshot(p, a, b, c, d)
	if selected := p.selectMember(snapshot, []*memberState{a, b, c, d}); selected != b {
		t.Fatalf("healthy current changed to %s", selected.tag)
	}
	group.SetCurrentTag(20, "")
	p.currentMember.Store(b)
	if selected := p.selectMember(snapshot, []*memberState{a, d}); selected != d {
		t.Fatalf("replacement = %s, want next available d", selected.tag)
	}
	selector.selected = "d"
	p.currentMember.Store(d)
	if selected := p.selectMember(snapshot, []*memberState{a}); selected != a {
		t.Fatalf("wrapped replacement = %s, want a", selected.tag)
	}
	selector.selected = ""
	p.currentMember.Store(nil)
	if selected := p.selectMember(snapshot, []*memberState{a, d}); selected != a {
		t.Fatalf("initial replacement = %s, want first available a", selected.tag)
	}
}

func TestLowestLatencySelectionKeepsHealthyCurrentThenUsesLatency(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(80 * time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "b"}).RecordSuccessWithLatency(10 * time.Millisecond)
	group.Register(25, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	a := &memberState{tag: "a", shared: acquireSharedState("a")}
	b := &memberState{tag: "b", shared: acquireSharedState("b")}
	p := &poolOutbound{mode: modeLowestLatency, monitor: mgr, options: Options{GroupID: 25,
		Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}}}
	snapshot := storeTestSnapshot(p, a, b)
	if selected := p.selectMember(snapshot, []*memberState{a, b}); selected != a {
		t.Fatalf("healthy current changed to %s", selected.tag)
	}
	group.SetCurrentTag(25, "")
	p.currentMember.Store(nil)
	if selected := p.selectMember(snapshot, []*memberState{a, b}); selected != b {
		t.Fatalf("replacement = %s, want lowest-latency b", selected.tag)
	}
}

func TestLowestLatencySelectionUsesKnownLatencyThenStableNodeOrder(t *testing.T) {
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "unknown"})
	mgr.Register(monitor.NodeInfo{Tag: "higher-id"}).RecordSuccessWithLatency(25 * time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "lower-id"}).RecordSuccessWithLatency(25 * time.Millisecond)
	unknown := &memberState{tag: "unknown"}
	higherID := &memberState{tag: "higher-id"}
	lowerID := &memberState{tag: "lower-id"}
	p := &poolOutbound{mode: modeLowestLatency, monitor: mgr, options: Options{Metadata: map[string]MemberMeta{
		"unknown": {NodeID: 1}, "higher-id": {NodeID: 3}, "lower-id": {NodeID: 2},
	}}}
	snapshot := storeTestSnapshot(p, unknown, higherID, lowerID)
	if selected := p.selectMember(snapshot, []*memberState{unknown, higherID, lowerID}); selected != lowerID {
		t.Fatalf("selected %s, want known latency with lowest stable node ID", selected.tag)
	}
}

func TestLowestLatencyInitialSelectionWaitsForAllInitialChecks(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(80 * time.Millisecond)
	bEntry := mgr.Register(monitor.NodeInfo{Tag: "b"})
	group.Register(28, 5*time.Minute, 3, "", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeLowestLatency, monitor: mgr, selector: selector,
		options: Options{GroupID: 28, Members: []string{"a", "b"}, Metadata: map[string]MemberMeta{
			"a": {NodeID: 1}, "b": {NodeID: 2},
		}},
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")})
	p.waitForInitialLatency.Store(true)
	p.reconcileCurrent()
	if selector.calls.Load() != 0 || group.CurrentTag(28) != "" {
		t.Fatalf("selected before all initial checks: calls=%d current=%q", selector.calls.Load(), group.CurrentTag(28))
	}
	bEntry.RecordSuccessWithLatency(10 * time.Millisecond)
	p.rebuildCandidatesNow()
	p.reconcileCurrent()
	if selector.calls.Load() != 1 || selector.selected != "b" || group.CurrentTag(28) != "b" {
		t.Fatalf("initial lowest selection: calls=%d selector=%q current=%q", selector.calls.Load(), selector.selected, group.CurrentTag(28))
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
	cEntry := mgr.Register(monitor.NodeInfo{Tag: "c"})
	aEntry.RecordSuccessWithLatency(5 * time.Millisecond)
	bEntry.MarkInitialCheckDone(false)
	cEntry.RecordSuccessWithLatency(15 * time.Millisecond)
	group.Register(21, 5*time.Minute, 3, "b", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2}, "c": {NodeID: 3},
	})
	selector := &fakeSelector{selected: "b"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeFixed, monitor: mgr, selector: selector,
		options: Options{GroupID: 21, Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}, "c": {NodeID: 3}}},
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")}, &memberState{tag: "c", shared: acquireSharedState("c")})
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "b", Error: "down", CheckedAt: time.Now()})
	if selector.selected != "c" || group.CurrentTag(21) != "c" {
		t.Fatalf("selector=%q current=%q, want ordered replacement c", selector.selected, group.CurrentTag(21))
	}
}

func TestFixedHealthSuccessDoesNotSwitchHealthyCurrent(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(80 * time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "b"}).RecordSuccessWithLatency(10 * time.Millisecond)
	group.Register(27, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeFixed, monitor: mgr, selector: selector,
		options: Options{GroupID: 27},
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")})
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "b", Success: true, Latency: 10 * time.Millisecond, CheckedAt: time.Now()})
	if selector.calls.Load() != 0 || selector.selected != "a" || group.CurrentTag(27) != "a" {
		t.Fatalf("healthy fixed current switched: calls=%d selector=%q current=%q", selector.calls.Load(), selector.selected, group.CurrentTag(27))
	}
}

func TestLowestLatencyHealthFailureHotSwitchesToLowestCandidate(t *testing.T) {
	group.Reset()
	defer group.Reset()
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(5 * time.Millisecond)
	mgr.Register(monitor.NodeInfo{Tag: "b"}).MarkInitialCheckDone(false)
	mgr.Register(monitor.NodeInfo{Tag: "c"}).RecordSuccessWithLatency(15 * time.Millisecond)
	group.Register(26, 5*time.Minute, 3, "b", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2}, "c": {NodeID: 3},
	})
	selector := &fakeSelector{selected: "b"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeLowestLatency, monitor: mgr, selector: selector,
		options: Options{GroupID: 26, Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}, "c": {NodeID: 3}}},
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")}, &memberState{tag: "c", shared: acquireSharedState("c")})
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "b", Error: "down", CheckedAt: time.Now()})
	if selector.selected != "a" || group.CurrentTag(26) != "a" {
		t.Fatalf("selector=%q current=%q, want lowest-latency replacement a", selector.selected, group.CurrentTag(26))
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
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")})
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
	mgr.Register(monitor.NodeInfo{Tag: "a"}).RecordSuccessWithLatency(10 * time.Millisecond)
	bEntry := mgr.Register(monitor.NodeInfo{Tag: "b"})
	bEntry.RecordSuccessWithLatency(20 * time.Millisecond)
	group.Register(24, 5*time.Minute, 3, "a", map[string]group.GroupInitialState{
		"a": {NodeID: 1}, "b": {NodeID: 2},
	})
	selector := &fakeSelector{selected: "a"}
	p := &poolOutbound{
		logger: boxlog.NewNOPFactory().Logger(), mode: modeLowestLatency, monitor: mgr, selector: selector,
		options: Options{GroupID: 24, Metadata: map[string]MemberMeta{"a": {NodeID: 1}, "b": {NodeID: 2}}},
	}
	storeTestSnapshot(p, &memberState{tag: "a", shared: acquireSharedState("a")}, &memberState{tag: "b", shared: acquireSharedState("b")})
	unregister := group.RegisterActivationHandler(24, p.activateNodeID)
	defer unregister()
	if err := group.ActivateMember(24, 2); err != nil {
		t.Fatal(err)
	}
	if selector.selected != "b" || group.CurrentTag(24) != "b" {
		t.Fatalf("selector=%q current=%q, want b", selector.selected, group.CurrentTag(24))
	}
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "a", Success: true, Latency: 10 * time.Millisecond, CheckedAt: time.Now()})
	if selector.calls.Load() != 1 || selector.selected != "b" || group.CurrentTag(24) != "b" {
		t.Fatalf("manual healthy current did not persist: calls=%d selector=%q current=%q", selector.calls.Load(), selector.selected, group.CurrentTag(24))
	}
	bEntry.MarkInitialCheckDone(false)
	p.handleHealthResult(monitor.HealthResultEvent{Tag: "b", Error: "down", CheckedAt: time.Now()})
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
