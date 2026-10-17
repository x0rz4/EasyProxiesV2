package boxmgr

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

// Disabling a node removes it from the active config, so "absent from cfg" must
// not be mistaken for "does not exist": the maintenance page deletes exactly
// these long-dead nodes.
func TestDeleteNodeRemovesPersistedNodeAbsentFromActiveConfig(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "delete-disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager := New(&config.Config{}, monitor.Config{}, WithStore(db))
	node := &store.Node{URI: "http://127.0.0.1:18080", Name: "retired-node", Source: store.NodeSourceManual, Enabled: false}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	if err := manager.DeleteNode(ctx, "retired-node"); err != nil {
		t.Fatalf("deleting a disabled node failed: %v", err)
	}
	stored, err := db.GetNodeByName(ctx, "retired-node")
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatal("disabled node survived deletion")
	}

	// The store remains the source of truth: unknown names are still a 404.
	if err := manager.DeleteNode(ctx, "ghost-node"); !errors.Is(err, controlplane.ErrNodeNotFound) {
		t.Fatalf("unknown node error=%v, want ErrNodeNotFound", err)
	}
}
