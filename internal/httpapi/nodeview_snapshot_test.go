package httpapi

import (
	"context"
	"path/filepath"
	"testing"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

type nodeViewReadTracker struct {
	store.Store
	afterManaged  func()
	listNodeCalls *int
}

func (s *nodeViewReadTracker) WithReadTx(ctx context.Context, fn func(store.Store) error) error {
	return s.Store.WithReadTx(ctx, func(tx store.Store) error {
		return fn(&nodeViewReadTracker{Store: tx, afterManaged: s.afterManaged, listNodeCalls: s.listNodeCalls})
	})
}

func (s *nodeViewReadTracker) ListManagedNodes(ctx context.Context, subscriptionID *int64) ([]store.ManagedNode, error) {
	rows, err := s.Store.ListManagedNodes(ctx, subscriptionID)
	if err == nil {
		s.afterManaged()
	}
	return rows, err
}

func (s *nodeViewReadTracker) ListNodes(ctx context.Context, filter store.NodeFilter) ([]store.Node, error) {
	*s.listNodeCalls++
	return s.Store.ListNodes(ctx, filter)
}

func TestNodeViewReusesManagedRowsWithinConsistentReadSnapshot(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	node := store.Node{Name: "before", URI: "http://127.0.0.1:8080", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	listNodeCalls := 0
	tracker := &nodeViewReadTracker{Store: db, listNodeCalls: &listNodeCalls, afterManaged: func() {
		updated := node
		updated.Name = "after"
		if err := db.UpdateNode(ctx, &updated); err != nil {
			t.Fatal(err)
		}
	}}
	server := &Server{store: tracker}
	view, err := server.loadNodeView(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	result, err := view.Query(nodeview.Spec{Condition: &nodefacts.Condition{FieldName: nodefacts.FieldNodeName, Op: nodefacts.OpEq, Value: "before"}}, nodeview.Page{})
	if err != nil || result.Total != 1 || result.Items[0].Name != "before" {
		t.Fatalf("mixed read snapshot: %+v %v", result, err)
	}
	current, err := db.GetNode(ctx, node.ID)
	if err != nil || current.Name != "after" {
		t.Fatalf("concurrent writer did not commit: %+v %v", current, err)
	}
	if listNodeCalls != 0 {
		t.Fatalf("fact loader redundantly read nodes %d times", listNodeCalls)
	}
}
