package boxmgr

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

// TestTriggerReloadExcludesNodesOfDisabledSubscriptions pins the regression the
// trigger path used to have: it merged every enabled store row, so a node that
// only reaches the runtime through a disabled subscription came back after any
// node CRUD, import or settings save. Startup and the subscription reconcile
// pass both honoured the subscription state; TriggerReload must too.
func TestTriggerReloadExcludesNodesOfDisabledSubscriptions(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "trigger-reload.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	manual := &store.Node{URI: "http://127.0.0.1:65530", Name: "manual", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, manual); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{Name: "disabled", URL: "https://example.com/sub", Enabled: false}
	if err := db.CreateSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSnapshot(ctx, subscription.ID, []store.SubscriptionNodeInput{{
		URI: "http://127.0.0.2:65530", Name: "sub-node", Enabled: true,
	}}, store.SubscriptionSnapshot{Attempt: time.Now().UTC(), Success: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	port := reserveTCPPorts(t, 1)[0]
	cfg := &config.Config{
		Listener: config.ListenerConfig{Address: "127.0.0.1", Port: port, Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: manual.ID, Name: manual.Name, URI: manual.URI}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{}, WithStore(db))
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if err := manager.TriggerReload(ctx); err != nil {
		t.Fatal(err)
	}

	manager.mu.RLock()
	names := make([]string, 0, len(manager.cfg.Nodes))
	for _, node := range manager.cfg.Nodes {
		names = append(names, node.Name)
	}
	manager.mu.RUnlock()
	if len(names) != 1 || names[0] != "manual" {
		t.Fatalf("runtime nodes after reload = %v, want only the manual node", names)
	}
}

type listFailureStore struct {
	store.Store
}

func (s listFailureStore) ListNodes(context.Context, store.NodeFilter) ([]store.Node, error) {
	return nil, errors.New("store unavailable")
}

// TestTriggerReloadFailsWhenRuntimeNodesCannotBeRead guards the second half of
// the same bug: a failed store read used to be logged and the reload proceeded
// with the inline-only config, silently taking every stored node offline.
func TestTriggerReloadFailsWhenRuntimeNodesCannotBeRead(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "trigger-reload-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manual := &store.Node{URI: "http://127.0.0.1:65530", Name: "manual", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, manual); err != nil {
		t.Fatal(err)
	}

	port := reserveTCPPorts(t, 1)[0]
	cfg := &config.Config{
		Listener: config.ListenerConfig{Address: "127.0.0.1", Port: port, Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: manual.ID, Name: manual.Name, URI: manual.URI}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{}, WithStore(listFailureStore{db}))
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if err := manager.TriggerReload(ctx); err == nil {
		t.Fatal("expected reload to fail when the store cannot be read")
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if len(manager.cfg.Nodes) != 1 || manager.cfg.Nodes[0].Name != "manual" {
		t.Fatalf("failed reload altered the runtime config: %+v", manager.cfg.Nodes)
	}
}
