package boxmgr

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

func batchTestManager(t *testing.T, db store.Store) *Manager {
	t.Helper()
	// Bypass Normalize: these tests exercise CreateNodes before any node exists,
	// and normalization refuses an empty node list.
	cfg := &config.Config{
		Pool:     config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		LogLevel: "error",
	}
	return New(cfg, monitor.Config{}, WithStore(db))
}

func TestCreateNodesDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "dry-run.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := batchTestManager(t, db)

	result, err := manager.CreateNodes(ctx, []config.NodeConfig{
		{Name: "one", URI: "http://user:pass@one.example:8080"},
		{Name: "two", URI: "socks5://127.0.0.1:1080"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Planned) != 2 || len(result.Created) != 0 || len(result.Failed) != 0 {
		t.Fatalf("result=%+v", result)
	}
	nodes, err := db.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("dry run wrote %d nodes", len(nodes))
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if len(manager.cfg.Nodes) != 0 {
		t.Fatalf("dry run mutated the runtime config: %+v", manager.cfg.Nodes)
	}
}

func TestCreateNodesRejectsUnbuildableURI(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "unbuildable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := batchTestManager(t, db)

	// ssr:// parses as a URI but has no sing-box outbound builder.
	result, err := manager.CreateNodes(ctx, []config.NodeConfig{
		{Name: "bad", URI: "ssr://example"},
		{Name: "good", URI: "http://user:pass@good.example:8080"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failed) != 1 || result.Failed[0].Name != "bad" {
		t.Fatalf("failed=%+v", result.Failed)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "good" {
		t.Fatalf("created=%+v", result.Created)
	}
	nodes, err := db.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "good" {
		t.Fatalf("stored nodes=%+v", nodes)
	}
}

func TestCreateNodesReportsPerRowDuplicates(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "batch-duplicates.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := batchTestManager(t, db)

	existing := &store.Node{URI: "http://user:pass@existing.example:8080", Name: "existing",
		Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, existing); err != nil {
		t.Fatal(err)
	}

	result, err := manager.CreateNodes(ctx, []config.NodeConfig{
		{Name: "fresh", URI: "http://user:pass@fresh.example:8080"},
		{Name: "copy", URI: "http://user:pass@existing.example:8080#copy"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "fresh" {
		t.Fatalf("created=%+v", result.Created)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failed=%+v", result.Failed)
	}
	var duplicate *controlplane.NodeDuplicateError
	if !errors.As(result.Failed[0].Err, &duplicate) || duplicate.ExistingID != existing.ID {
		t.Fatalf("duplicate failure=%+v", result.Failed[0])
	}

	// A name held by a disabled node is still taken: the batch sees the store,
	// not just the runtime config.
	if err := manager.SetNodeEnabled(ctx, "existing", false); err != nil {
		t.Fatal(err)
	}
	result, err = manager.CreateNodes(ctx, []config.NodeConfig{
		{Name: "existing", URI: "http://user:pass@brand-new.example:8080"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("disabled-name collision was not reported: %+v", result)
	}
}
