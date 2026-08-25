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
		ExcludedNodeIDs:      []int64{999},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	group.SubscriptionEnabled, group.SubscriptionToken, group.SubscriptionMode = true, "token", "entry"
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
	if !loaded.SubscriptionEnabled || loaded.SubscriptionToken != "token" || loaded.SubscriptionMode != "entry" {
		t.Fatalf("subscription settings not persisted: %+v", loaded)
	}
	if len(loaded.ExcludedNodeIDs) != 1 || loaded.ExcludedNodeIDs[0] != 999 {
		t.Fatalf("member exclusions not persisted: %+v", loaded.ExcludedNodeIDs)
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

func TestUpdateGroupCurrentActiveNodeDoesNotOverwriteEditedFields(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "group-current.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	groupPool := &GroupPool{Name: "before", BindAddress: "127.0.0.1", BindPort: 10002, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	groupPool.Name = "edited"
	groupPool.DispatchMode = "random"
	if err := db.UpdateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateGroupCurrentActiveNode(ctx, groupPool.ID, 42); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CurrentActiveNodeID != 42 || stored.Name != "edited" || stored.DispatchMode != "random" {
		t.Fatalf("targeted current update overwrote group fields: %+v", stored)
	}
}
