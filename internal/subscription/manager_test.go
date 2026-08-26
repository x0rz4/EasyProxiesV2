package subscription

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

type recordingRuntimeManager struct {
	config *config.Config
	ports  map[string]uint16
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
	if err := mgr.RefreshOne(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
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

func TestChangedSubscriptionURLMergesOldAndNewNodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			_, _ = w.Write([]byte("http://a.example:80#A\nhttp://b.example:80#B"))
		case "/new":
			_, _ = w.Write([]byte("http://b.example:80#B\nhttp://c.example:80#C"))
		case "/empty":
			_, _ = w.Write([]byte("\n"))
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
	if err := mgr.RefreshOne(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := mgr.Update(context.Background(), sub.ID, store.Subscription{Name: "changing", URL: server.URL + "/new", Enabled: true,
		RefreshIntervalSeconds: sub.RefreshIntervalSeconds, RefreshTimeoutSeconds: sub.RefreshTimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.RefreshOne(context.Background(), updated.ID); err != nil {
		t.Fatal(err)
	}
	members, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("merged members=%+v err=%v", members, err)
	}
	byURI := make(map[string]store.SubscriptionNode, len(members))
	for _, member := range members {
		byURI[member.Node.URI] = member
	}
	for _, uri := range []string{"http://a.example:80#A", "http://b.example:80#B", "http://c.example:80#C"} {
		if _, ok := byURI[uri]; !ok {
			t.Fatalf("node %q missing after URL change: %+v", uri, members)
		}
	}
	updated.URL = server.URL + "/empty"
	if _, err := mgr.Update(context.Background(), updated.ID, *updated); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RefreshOne(context.Background(), updated.ID); err == nil {
		t.Fatal("empty subscription refresh unexpectedly succeeded")
	}
	afterEmpty, err := mgr.Nodes(context.Background(), sub.ID)
	if err != nil || len(afterEmpty) != 3 {
		t.Fatalf("empty refresh changed members=%+v err=%v", afterEmpty, err)
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
	if err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{oldInput}, store.SubscriptionSnapshot{
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
	if err := mgr.RefreshOne(ctx, sub.ID); err != nil {
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
	if err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{boundInput}, store.SubscriptionSnapshot{
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
	runtimeCfg := &config.Config{Mode: "pool", LogLevel: "error",
		Listener: config.ListenerConfig{Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
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
	if err := mgr.RefreshOne(ctx, sub.ID); err != nil {
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
