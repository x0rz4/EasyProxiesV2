package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGroupPoolCRUDAndState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "groups.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &Node{URI: "http://node.example:80", Name: "node", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	group := &GroupPool{Name: "HK", BindAddress: "0.0.0.0", BindPort: 10001, Protocol: "mixed",
		DispatchMode: "fixed", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{node.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, group); err != nil {
		t.Fatal(err)
	}
	if group.ID == 0 {
		t.Fatal("group ID was not assigned")
	}
	if err := db.UpsertGroupNodeState(ctx, &GroupNodeState{GroupID: group.ID, NodeID: node.ID,
		FailureHistory: []int64{1, 2, 3}, Evicted: true, LastError: "down"}); err != nil {
		t.Fatal(err)
	}

	loaded, err := db.GetGroupPool(ctx, group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.NodeStates) != 1 || !loaded.NodeStates[0].Evicted {
		t.Fatalf("unexpected loaded group: %+v", loaded)
	}
	loaded.Name = "HK VIP"
	loaded.DispatchMode = "random"
	if err := db.UpdateGroupPool(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	groups, err := db.ListGroupPools(ctx)
	if err != nil || len(groups) != 1 || groups[0].Name != "HK VIP" {
		t.Fatalf("unexpected groups: %+v, %v", groups, err)
	}
	if err := db.ClearGroupNodeState(ctx, group.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteGroupPool(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
}
