package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

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
