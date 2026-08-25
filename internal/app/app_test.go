package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
)

func TestLoadNodesFromStoreKeepsFreshNodesDuringSubscriptionMigration(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.BulkUpsertNodes(ctx, []store.Node{{
		URI: "ss://legacy", Name: "legacy", Source: store.NodeSourceSubscription, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Subscriptions: []string{"https://example.com/sub"},
		Nodes: []config.NodeConfig{{
			URI: "ss://fresh", Name: "fresh", Source: config.NodeSourceSubscription,
		}},
	}

	if err := loadNodesFromStore(ctx, cfg, db); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].URI != "ss://fresh" {
		t.Fatalf("expected freshly fetched bootstrap node, got %#v", cfg.Nodes)
	}
}

func TestGroupStatePersisterFlushesClearedCurrent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "group-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	groupPool := &store.GroupPool{Name: "group", BindAddress: "127.0.0.1", BindPort: 12095,
		Protocol: "mixed", DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3,
		HealthCheckSeconds: 60, CurrentActiveNodeID: 99, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	persister := newGroupStatePersister(db)
	persister.Observe(group.GroupStateEvent{GroupID: groupPool.ID, CurrentChanged: true, CurrentNodeID: 0})
	persister.Close()
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentActiveNodeID != 0 {
		t.Fatalf("current node ID = %d, want 0", updated.CurrentActiveNodeID)
	}
}

func TestLoadNodesFromStoreHonorsDisabledSubscriptions(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	sub := &store.Subscription{
		Name: "disabled", URL: "https://example.com/sub", Enabled: false,
		RefreshIntervalSeconds: 3600, RefreshTimeoutSeconds: 30,
	}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{{
		URI: "ss://disabled", Name: "disabled", Enabled: true,
	}}, store.SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Subscriptions: []string{sub.URL},
		Nodes: []config.NodeConfig{{
			URI: "ss://fresh", Name: "fresh", Source: config.NodeSourceSubscription,
		}},
	}

	if err := loadNodesFromStore(ctx, cfg, db); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Nodes) != 0 {
		t.Fatalf("disabled subscription must not bootstrap nodes, got %#v", cfg.Nodes)
	}
}

func TestLoadNodesFromStoreRecoversIncompleteImportedSubscription(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	sub := &store.Subscription{
		Name: "incomplete", URL: "https://example.com/sub", Enabled: false,
		RefreshIntervalSeconds: 3600, RefreshTimeoutSeconds: 30,
	}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := db.BulkUpsertNodes(ctx, []store.Node{{
		URI: "ss://legacy", Name: "legacy", Source: store.NodeSourceSubscription, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Subscriptions: []string{sub.URL},
		Nodes: []config.NodeConfig{{
			URI: "ss://fresh", Name: "fresh", Source: config.NodeSourceSubscription,
		}},
	}

	if err := loadNodesFromStore(ctx, cfg, db); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].URI != "ss://fresh" {
		t.Fatalf("expected incomplete import recovery node, got %#v", cfg.Nodes)
	}
	legacy, err := db.GetNodeByURI(ctx, "ss://legacy")
	if err != nil || legacy == nil || legacy.Source != store.NodeSourceSubscription {
		t.Fatalf("pending legacy node was adopted too early: node=%+v err=%v", legacy, err)
	}
}

func TestLoadNodesFromStoreAdoptsEstablishedOrphanSubscriptionNodes(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sub := &store.Subscription{Name: "established", URL: "https://example.com/sub", Enabled: true,
		RefreshIntervalSeconds: 3600, RefreshTimeoutSeconds: 30}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitSnapshot(ctx, sub.ID, []store.SubscriptionNodeInput{{URI: "ss://current", Name: "current", Enabled: true}},
		store.SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	orphan := &store.Node{URI: "ss://orphan", Name: "orphan", Source: store.NodeSourceSubscription,
		Region: "hk", Country: "Hong Kong", Enabled: true}
	if err := db.CreateNode(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeStats(ctx, &store.NodeStats{NodeID: orphan.ID, SuccessCount: 3, LastLatencyMs: 44}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Subscriptions: []string{sub.URL}}
	if err := loadNodesFromStore(ctx, cfg, db); err != nil {
		t.Fatal(err)
	}
	byURI := make(map[string]config.NodeConfig, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		byURI[node.URI] = node
	}
	if len(byURI) != 2 || byURI[orphan.URI].ID != orphan.ID || byURI[orphan.URI].Source != config.NodeSourceManual {
		t.Fatalf("startup nodes=%+v", cfg.Nodes)
	}
	recovered, _ := db.GetNode(ctx, orphan.ID)
	stats, _ := db.GetNodeStats(ctx, orphan.ID)
	if recovered == nil || recovered.Source != store.NodeSourceManual || recovered.Region != "hk" ||
		stats == nil || stats.SuccessCount != 3 || stats.LastLatencyMs != 44 {
		t.Fatalf("recovered=%+v stats=%+v", recovered, stats)
	}
}
