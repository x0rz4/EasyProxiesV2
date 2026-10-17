package subscription

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

type recordingRuntimeManager struct {
	config *config.Config
	ports  map[string]uint16
}

func TestFetchSubscriptionUsesSelectedFormatAndUserAgent(t *testing.T) {
	var gotUserAgent, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent, gotAccept = r.UserAgent(), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = fmt.Fprint(w, "proxies:\n  - { name: edge, type: ss, server: example.com, port: 8388, cipher: aes-256-gcm, password: pass }\n")
	}))
	defer server.Close()

	mgr := New(&config.Config{}, nil, WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub := &store.Subscription{URL: server.URL, Format: config.SubscriptionFormatClash, UserAgent: "provider-client/1.0"}
	nodes, _, _, _, _, err := mgr.fetchSubscription(context.Background(), sub, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || gotUserAgent != sub.UserAgent || !strings.Contains(gotAccept, "application/yaml") {
		t.Fatalf("nodes=%+v user-agent=%q accept=%q", nodes, gotUserAgent, gotAccept)
	}
}

func TestSubscriptionDefaultUserAgents(t *testing.T) {
	tests := map[string]string{
		config.SubscriptionFormatAuto:    "mihomo",
		config.SubscriptionFormatClash:   "mihomo",
		config.SubscriptionFormatBase64:  "v2rayN",
		config.SubscriptionFormatSingBox: "sing-box",
	}
	for format, want := range tests {
		if got := defaultSubscriptionUserAgent(format); got != want {
			t.Fatalf("format %q user-agent=%q, want %q", format, got, want)
		}
	}
}

// waitFor polls a condition instead of sleeping for a fixed duration. Locking
// assertions are inherently timing-based once creates/updates refresh in the
// background, and a bounded poll keeps the test deterministic without tying it
// to an arbitrary delay.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// refreshOneEventually retries while a create/update-triggered background
// refresh still holds the single refresh slot.
func refreshOneEventually(t *testing.T, mgr *Manager, id int64) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := mgr.RefreshOne(context.Background(), id)
		if !errors.Is(err, controlplane.ErrRefreshInProgress) {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh stayed busy")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func subscriptionNodeCount(t *testing.T, mgr *Manager, id int64) int {
	t.Helper()
	nodes, err := mgr.Nodes(context.Background(), id)
	if err != nil {
		t.Fatalf("list subscription %d nodes: %v", id, err)
	}
	return len(nodes)
}

func (m *recordingRuntimeManager) ReloadWithPortMap(cfg *config.Config, _ map[string]uint16) error {
	m.config = cfg.Clone()
	return nil
}

func (m *recordingRuntimeManager) CurrentPortMap() map[string]uint16 {
	return m.ports
}

func TestImportConfiguredSubscriptionsUsesDefaults(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &config.Config{Subscriptions: []string{"https://example.com/a", "https://example.com/a", "https://example.com/b"}}
	cfg.SubscriptionRefresh.Enabled = true
	cfg.SubscriptionRefresh.Interval = 25 * time.Minute
	cfg.SubscriptionRefresh.Timeout = 17 * time.Second
	mgr := New(cfg, nil, WithStore(db))
	defer mgr.Stop()

	if err := mgr.importConfiguredSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	subs, err := mgr.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subscriptions, want 2", len(subs))
	}
	if subs[0].Name != "订阅 1" || !subs[0].Enabled || subs[0].RefreshIntervalSeconds != 1500 || subs[0].RefreshTimeoutSeconds != 17 {
		t.Fatalf("unexpected imported subscription: %+v", subs[0])
	}
}

func TestRefreshNowKeepsFailedMembershipAndCommitsSuccessfulSubscription(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			if fail {
				http.Error(w, "failed", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte("http://a.example:80#old"))
		case "/b":
			_, _ = w.Write([]byte("http://b.example:81#new"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()

	a, err := mgr.Create(context.Background(), store.Subscription{Name: "A", URL: server.URL + "/a", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := mgr.Create(context.Background(), store.Subscription{Name: "B", URL: server.URL + "/b", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Create refreshes in the background; wait for both so the manual refresh
	// below is not rejected by the single refresh slot.
	waitFor(t, "A and B initial snapshots", func() bool {
		return subscriptionNodeCount(t, mgr, a.ID) == 1 && subscriptionNodeCount(t, mgr, b.ID) == 1
	})
	fail = true
	if err := mgr.RefreshNow(); err == nil {
		t.Fatal("expected partial refresh error")
	}

	aNodes, err := mgr.Nodes(context.Background(), a.ID)
	if err != nil || len(aNodes) != 1 || aNodes[0].Node.URI != "http://a.example:80#old" {
		t.Fatalf("failed subscription membership changed: nodes=%+v err=%v", aNodes, err)
	}
	bNodes, err := mgr.Nodes(context.Background(), b.ID)
	if err != nil || len(bNodes) != 1 || bNodes[0].Node.URI != "http://b.example:81#new" {
		t.Fatalf("successful subscription was not committed: nodes=%+v err=%v", bNodes, err)
	}
	gotA, err := mgr.Get(context.Background(), a.ID)
	if err != nil || gotA.LastAttempt.IsZero() || gotA.LastError == "" || gotA.NodeCount != 1 {
		t.Fatalf("failure metadata not persisted: sub=%+v err=%v", gotA, err)
	}
}

func TestChangedSubscriptionURLClearsOldMembership(t *testing.T) {
	var empty atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			_, _ = w.Write([]byte("http://a.example:80#A\nhttp://b.example:80#B"))
		case "/new":
			if empty.Load() {
				_, _ = w.Write([]byte("\n"))
				return
			}
			_, _ = w.Write([]byte("http://b.example:80#B\nhttp://c.example:80#C"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "url-change.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub, err := mgr.Create(context.Background(), store.Subscription{Name: "changing", URL: server.URL + "/old", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := refreshOneEventually(t, mgr, sub.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := mgr.Update(context.Background(), sub.ID, store.Subscription{Name: "changing", URL: server.URL + "/new", Enabled: true,
		RefreshIntervalSeconds: sub.RefreshIntervalSeconds, RefreshTimeoutSeconds: sub.RefreshTimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	// Repointing a subscription at a different upstream must drop the old
	// members instead of merging two unrelated node sets. Update also kicks off
	// the refresh, so wait for the new snapshot to land.
	waitFor(t, "new upstream snapshot", func() bool { return subscriptionNodeCount(t, mgr, sub.ID) == 2 })
	members, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	byURI := make(map[string]store.SubscriptionNode, len(members))
	for _, member := range members {
		byURI[member.Node.URI] = member
	}
	for _, uri := range []string{"http://b.example:80#B", "http://c.example:80#C"} {
		if _, ok := byURI[uri]; !ok {
			t.Fatalf("node %q missing after URL change: %+v", uri, members)
		}
	}
	if _, ok := byURI["http://a.example:80#A"]; ok {
		t.Fatalf("old upstream node survived the URL change: %+v", members)
	}
	if old, _ := db.GetNodeByURI(context.Background(), "http://a.example:80#A"); old != nil {
		t.Fatalf("old exclusive node was not deleted: %+v", old)
	}
	// An empty provider response is a failure, not a signal to wipe members.
	empty.Store(true)
	if err := refreshOneEventually(t, mgr, sub.ID); err == nil {
		t.Fatal("empty subscription refresh unexpectedly succeeded")
	}
	afterEmpty, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil || len(afterEmpty) != 2 {
		t.Fatalf("empty refresh changed members=%+v err=%v", afterEmpty, err)
	}
	if current, _ := mgr.Get(context.Background(), sub.ID); current.URL != updated.URL || current.LastError == "" {
		t.Fatalf("failed refresh metadata=%+v", current)
	}
}

func TestRefreshDiffReusesUniqueProviderNodeName(t *testing.T) {
	var revision atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if revision.Load() == 0 {
			_, _ = w.Write([]byte("http://old-user:old-pass@old.example:3129#lite-家庭宽带-香港hgc"))
			return
		}
		_, _ = w.Write([]byte("http://new-user:new-pass@new.example:8080#lite-家庭宽带-香港hgc"))
	}))
	defer server.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "logical-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub, err := mgr.Create(context.Background(), store.Subscription{Name: "logical", URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial snapshot", func() bool { return subscriptionNodeCount(t, mgr, sub.ID) == 1 })
	first, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil || len(first) != 1 {
		t.Fatalf("first members=%+v err=%v", first, err)
	}
	firstID := first[0].Node.ID

	revision.Store(1)
	if err := mgr.RefreshOne(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	second, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil || len(second) != 1 {
		t.Fatalf("logical refresh appended a duplicate: members=%+v err=%v", second, err)
	}
	if second[0].Node.ID != firstID || second[0].Node.URI != "http://new-user:new-pass@new.example:8080#lite-家庭宽带-香港hgc" {
		t.Fatalf("logical refresh did not reuse the node: first=%+v second=%+v", first[0].Node, second[0].Node)
	}
	// The create-triggered background refresh also accrued counters, so assert on
	// the manual refresh's own contribution instead of the global totals.
	if err := mgr.RefreshOne(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	status := mgr.Status()
	if status.Created != 0 || status.Updated != 1 {
		t.Fatalf("logical refresh status=%+v", status)
	}
}

func TestRefreshRehydratesPersistedGroupMembershipBeforeReload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http://new.example:81#new"))
	}))
	defer server.Close()

	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sub := &store.Subscription{Name: "group source", URL: server.URL, Enabled: true,
		RefreshIntervalSeconds: 3600, RefreshTimeoutSeconds: 2}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	oldInput := store.SubscriptionNodeInput{URI: "http://bound.example:80#bound", Name: "bound", Enabled: true}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{oldInput}, store.SubscriptionSnapshot{
		Attempt: time.Now().UTC(), Success: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	boundNode, err := db.GetNodeByURI(ctx, oldInput.URI)
	if err != nil || boundNode == nil {
		t.Fatalf("bound node=%+v err=%v", boundNode, err)
	}
	groupPool := &store.GroupPool{Name: "bound group", BindAddress: "127.0.0.1", BindPort: 12001,
		Protocol: "mixed", DispatchMode: "fixed", ExplicitNodeIDs: []int64{boundNode.ID},
		ExcludedNodeIDs: []int64{999}, FailureWindowSeconds: 300, FailureThreshold: 3,
		HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}

	// Deliberately provide a stale cached config with no groups. The persisted
	// group relationship must win when the subscription refresh reconciles the
	// runtime topology.
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = 2 * time.Second
	runtime := &recordingRuntimeManager{}
	mgr := New(cfg, runtime, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	if err := refreshOneEventually(t, mgr, sub.ID); err != nil {
		t.Fatal(err)
	}
	if runtime.config == nil || len(runtime.config.Groups) != 1 {
		t.Fatalf("persisted group disappeared from reload config: %+v", runtime.config)
	}
	gotGroup := runtime.config.Groups[0]
	if gotGroup.ID != groupPool.ID || len(gotGroup.ExplicitNodeIDs) != 1 ||
		gotGroup.ExplicitNodeIDs[0] != boundNode.ID || len(gotGroup.ExcludedNodeIDs) != 1 {
		t.Fatalf("persisted group membership changed: %+v", gotGroup)
	}
	foundBoundNode := false
	for _, node := range runtime.config.Nodes {
		if node.ID == boundNode.ID {
			foundBoundNode = true
			break
		}
	}
	if !foundBoundNode {
		t.Fatalf("retained bound node %d missing from runtime nodes: %+v", boundNode.ID, runtime.config.Nodes)
	}
	members, err := db.ListSubscriptionNodes(ctx, sub.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("incremental subscription relationship changed: members=%+v err=%v", members, err)
	}
}

func TestRefreshKeepsBoundGroupRuntimeListening(t *testing.T) {
	group.Reset()
	defer group.Reset()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http://127.0.0.1:65529#new"))
	}))
	defer server.Close()

	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-runtime-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub := &store.Subscription{Name: "runtime source", URL: server.URL, Enabled: true,
		RefreshIntervalSeconds: 3600, RefreshTimeoutSeconds: 2}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	boundInput := store.SubscriptionNodeInput{URI: "http://127.0.0.1:65530#bound", Name: "bound", Enabled: true}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{boundInput}, store.SubscriptionSnapshot{
		Attempt: time.Now().UTC(), Success: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	boundNode, err := db.GetNodeByURI(ctx, boundInput.URI)
	if err != nil || boundNode == nil {
		t.Fatalf("bound node=%+v err=%v", boundNode, err)
	}
	ports := reserveRuntimePorts(t, 2)
	groupPool := &store.GroupPool{Name: "live group", BindAddress: "127.0.0.1", BindPort: ports[1],
		Protocol: "mixed", DispatchMode: "fixed", ExplicitNodeIDs: []int64{boundNode.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeCfg := &config.Config{LogLevel: "error",
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: boundNode.ID, Name: boundNode.Name, URI: boundNode.URI}},
		Groups:   boxmgr.GroupConfigsFromStore([]store.GroupPool{*groupPool}),
	}
	if err := runtimeCfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	boxManager := boxmgr.New(runtimeCfg, monitor.Config{}, boxmgr.WithStore(db))
	if err := boxManager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer boxManager.Close()
	assertRuntimePortListening(t, groupPool.BindPort)

	// Simulate the stale subscription-side snapshot that previously caused the
	// persisted group to be interpreted as deleted during refresh.
	staleCfg := runtimeCfg.Clone()
	staleCfg.Groups = nil
	mgr := New(staleCfg, boxManager, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	if err := refreshOneEventually(t, mgr, sub.ID); err != nil {
		t.Fatal(err)
	}
	if status := boxManager.GroupRuntimeStatus(groupPool.ID); status.Status != "ready" {
		t.Fatalf("group stopped after subscription refresh: %+v", status)
	}
	assertRuntimePortListening(t, groupPool.BindPort)
}

func reserveRuntimePorts(t *testing.T, count int) []uint16 {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]uint16, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, uint16(listener.Addr().(*net.TCPAddr).Port))
	}
	for _, listener := range listeners {
		_ = listener.Close()
	}
	return ports
}

func assertRuntimePortListening(t *testing.T, port uint16) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), time.Second)
	if err != nil {
		t.Fatalf("port %d is not listening: %v", port, err)
	}
	_ = connection.Close()
}

func TestCreateRefreshesInBackground(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "create-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = time.Second
	// Auto refresh stays disabled: the create-triggered fetch must not depend on
	// the global switch.
	cfg.SubscriptionRefresh.Enabled = false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http://created.example:80#created"))
	}))
	defer server.Close()
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()

	sub, err := mgr.Create(ctx, store.Subscription{Name: "fresh", URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "create-triggered snapshot", func() bool { return subscriptionNodeCount(t, mgr, sub.ID) == 1 })
	stored, err := mgr.Get(ctx, sub.ID)
	if err != nil || stored.LastSuccess.IsZero() || stored.NodeCount != 1 {
		t.Fatalf("snapshot after create=%+v err=%v", stored, err)
	}

	// A subscription created disabled must not fetch.
	disabled, err := mgr.Create(ctx, store.Subscription{Name: "off", URL: server.URL + "/off", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if count := subscriptionNodeCount(t, mgr, disabled.ID); count != 0 {
		t.Fatalf("disabled subscription fetched %d nodes", count)
	}
}

func TestRefreshFailureDoesNotClobberConcurrentSettings(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "lost-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = 5 * time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()

	sub, err := mgr.Create(ctx, store.Subscription{Name: "before", URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered: // the create-triggered fetch is now blocked in the handler
	case <-time.After(10 * time.Second):
		t.Fatal("fetch never reached the upstream")
	}
	// The user edits the row while the fetch is still in flight.
	if _, err := mgr.Update(ctx, sub.ID, store.Subscription{Name: "after", URL: server.URL + "/edited", Enabled: true,
		RefreshIntervalSeconds: 1200, RefreshTimeoutSeconds: 9}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "failure recorded", func() bool {
		current, getErr := mgr.Get(ctx, sub.ID)
		return getErr == nil && current.LastError != "" && !mgr.Status().IsRefreshing
	})
	time.Sleep(50 * time.Millisecond) // let the queued post-update refresh drain
	current, err := mgr.Get(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != "after" || current.URL != server.URL+"/edited" || current.RefreshIntervalSeconds != 1200 {
		t.Fatalf("failure write reverted concurrent edits: %+v", current)
	}
}

func TestRefreshDueFetchesConcurrently(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "parallel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			seen := peak.Load()
			if current <= seen || peak.CompareAndSwap(seen, current) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
		_, _ = w.Write([]byte("http://" + strings.TrimPrefix(r.URL.Path, "/") + ".example:80#n"))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = 5 * time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()

	ids := make([]int64, 0, 3)
	for _, name := range []string{"one", "two", "three"} {
		// Create them disabled so no background fetch races with RefreshNow, then
		// flip the flag through the store (the manager's SetEnabled would itself
		// trigger a refresh).
		sub, createErr := mgr.Create(ctx, store.Subscription{Name: name, URL: server.URL + "/" + name, Enabled: false})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if enableErr := db.SetSubscriptionEnabled(ctx, sub.ID, true); enableErr != nil {
			t.Fatal(enableErr)
		}
		ids = append(ids, sub.ID)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.RefreshNow() }()
	// Three downloads must overlap before any of them is allowed to finish.
	deadline := time.Now().Add(10 * time.Second)
	for peak.Load() < 3 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("fetches did not overlap, peak=%d", peak.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if count := subscriptionNodeCount(t, mgr, id); count != 1 {
			t.Fatalf("subscription %d committed %d nodes", id, count)
		}
	}
}

func TestRefreshOneReportsRefreshingStatus(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "refreshing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte("http://status.example:80#n"))
	}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.SubscriptionRefresh.Interval = time.Hour
	cfg.SubscriptionRefresh.Timeout = 5 * time.Second
	mgr := New(cfg, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()

	sub, err := mgr.Create(ctx, store.Subscription{Name: "busy", URL: server.URL, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.RefreshOne(ctx, sub.ID) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh never started")
	}
	status := mgr.Status()
	if !status.IsRefreshing || len(status.RefreshingIDs) != 1 || status.RefreshingIDs[0] != sub.ID {
		t.Fatalf("status while refreshing=%+v", status)
	}
	if err := mgr.RefreshOne(ctx, sub.ID); !errors.Is(err, controlplane.ErrRefreshInProgress) {
		t.Fatalf("second refresh error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status = mgr.Status()
	if status.IsRefreshing || len(status.RefreshingIDs) != 0 || status.LastRefresh.IsZero() || status.RefreshCount != 1 {
		t.Fatalf("status after refresh=%+v", status)
	}
}

func TestFetchKeepsStoredLastModifiedWhenUpstreamOmitsIt(t *testing.T) {
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if mode.Load() == 0 {
			w.Header().Set("ETag", `"first"`)
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		} else {
			w.Header().Set("ETag", `"second"`)
		}
		_, _ = w.Write([]byte("http://lm.example:80#n"))
	}))
	defer server.Close()
	mgr := New(&config.Config{}, nil, WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub := &store.Subscription{URL: server.URL}
	_, _, etag, lastModified, notModified, err := mgr.fetchSubscription(context.Background(), sub, time.Second)
	if err != nil || notModified || etag != `"first"` || lastModified == "" {
		t.Fatalf("first fetch etag=%q last-modified=%q err=%v", etag, lastModified, err)
	}
	sub.ETag, sub.LastModified = etag, lastModified
	mode.Store(1)
	_, _, etag, lastModified, _, err = mgr.fetchSubscription(context.Background(), sub, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if etag != `"second"` || lastModified != sub.LastModified {
		t.Fatalf("validator fallback lost Last-Modified: etag=%q last-modified=%q", etag, lastModified)
	}
}

func TestHealingRefreshHonorsPersistedCooldownAndBusyGate(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "healing-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	mgr := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer mgr.Stop()
	sub, err := mgr.Create(ctx, store.Subscription{Name: "healer", URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// Create's background fetch may already have recorded the attempt and the
	// cooldown; wait for the refresh slot before driving the healer directly.
	waitFor(t, "background refresh to finish", func() bool {
		persisted, getErr := db.GetSubscription(ctx, sub.ID)
		return getErr == nil && !persisted.LastAttempt.IsZero() && !mgr.Status().IsRefreshing
	})
	if _, deferred := mgr.RefreshForHealing(ctx, sub.ID).(*group.DeferredHealing); !deferred {
		t.Fatal("healer ignored the cooldown recorded by the create refresh")
	}
	first := requests.Load()
	if first == 0 {
		t.Fatal("missing first request")
	}
	if !mgr.Status().HasSubscriptions {
		t.Fatal("status lost the subscription")
	}
	persisted, err := db.GetSubscription(ctx, sub.ID)
	if err != nil || persisted.LastAttempt.IsZero() {
		t.Fatalf("attempt not persisted: %+v %v", persisted, err)
	}
	// A new manager must still honor the database cooldown.
	restarted := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer restarted.Stop()
	if _, ok := restarted.RefreshForHealing(ctx, sub.ID).(*group.DeferredHealing); !ok {
		t.Fatal("restart ignored cooldown")
	}
	if requests.Load() != first {
		t.Fatal("cooldown sent network request")
	}
	restarted.refreshMu.Lock()
	_, busy := restarted.RefreshForHealing(ctx, sub.ID).(*group.DeferredHealing)
	restarted.refreshMu.Unlock()
	if !busy || requests.Load() != first {
		t.Fatal("busy gate failed")
	}
}
