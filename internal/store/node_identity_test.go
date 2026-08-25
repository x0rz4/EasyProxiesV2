package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestIdentityReconcileMergesHistoricalReferences(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.db")
	opened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db := opened.(*sqliteStore)
	first := &Node{URI: "http://user:pass@EXAMPLE.com:80#first", Name: "first", Source: NodeSourceSubscription, Enabled: true}
	if err := db.CreateNode(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP INDEX uq_nodes_identity_hash`); err != nil {
		t.Fatal(err)
	}
	result, err := db.db.Exec(`INSERT INTO nodes(uri,name,source,enabled,tags,identity_hash,canonical_json) VALUES(?,?,?,?,?,?,?)`,
		"http://user:pass@example.com#second", "manual-name", NodeSourceManual, 0, `["manual-tag"]`, "legacy-second", "")
	if err != nil {
		t.Fatal(err)
	}
	secondID, _ := result.LastInsertId()
	if _, err := db.db.Exec(`INSERT INTO node_stats(node_id,success_count,total_download_bytes) VALUES(?,?,?)`, secondID, 3, 99); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE node_stats SET success_count=2,total_upload_bytes=11 WHERE node_id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	sub := &Subscription{Name: "sub", URL: "https://example.test/sub", Enabled: true, RefreshIntervalSeconds: 60, RefreshTimeoutSeconds: 10}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO subscription_nodes(subscription_id,node_id,position) VALUES(?,?,?),(?,?,?)`, sub.ID, first.ID, 4, sub.ID, secondID, 1); err != nil {
		t.Fatal(err)
	}
	group := &GroupPool{Name: "group", BindAddress: "127.0.0.1", BindPort: 12000, Protocol: "mixed", DispatchMode: "fixed", ExplicitNodeIDs: []int64{secondID}, ExcludedNodeIDs: []int64{first.ID, secondID}, CurrentActiveNodeID: secondID, Enabled: true}
	if err := db.CreateGroupPool(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertGroupNodeState(ctx, &GroupNodeState{GroupID: group.ID, NodeID: secondID, FailureHistory: []int64{1, 2}, Evicted: true}); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	nodes, err := reopened.ListNodes(ctx, NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes=%+v", nodes)
	}
	winner := nodes[0]
	if winner.ID != first.ID || winner.Enabled || winner.Source != NodeSourceManual || winner.Name != "manual-name" {
		t.Fatalf("winner=%+v", winner)
	}
	if len(winner.Tags) != 1 || winner.Tags[0] != "manual-tag" {
		t.Fatalf("tags=%v", winner.Tags)
	}
	stats, _ := reopened.GetNodeStats(ctx, winner.ID)
	if stats.SuccessCount != 5 || stats.TotalUploadBytes != 11 || stats.TotalDownloadBytes != 99 {
		t.Fatalf("stats=%+v", stats)
	}
	members, _ := reopened.ListSubscriptionNodes(ctx, sub.ID)
	if len(members) != 1 || members[0].Node.ID != winner.ID || members[0].Position != 1 {
		t.Fatalf("members=%+v", members)
	}
	mergedGroup, _ := reopened.GetGroupPool(ctx, group.ID)
	if mergedGroup.CurrentActiveNodeID != winner.ID || len(mergedGroup.ExplicitNodeIDs) != 1 || mergedGroup.ExplicitNodeIDs[0] != winner.ID || len(mergedGroup.ExcludedNodeIDs) != 1 {
		encoded, _ := json.Marshal(mergedGroup)
		t.Fatalf("group=%s", encoded)
	}
	if len(mergedGroup.NodeStates) != 1 || mergedGroup.NodeStates[0].NodeID != winner.ID || !mergedGroup.NodeStates[0].Evicted {
		t.Fatalf("states=%+v", mergedGroup.NodeStates)
	}
}
