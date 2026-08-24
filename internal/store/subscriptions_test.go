package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSubscriptionsAndSnapshots(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a := &Subscription{Name: "A", URL: "https://a.example/sub", Enabled: true, RefreshIntervalSeconds: 60, RefreshTimeoutSeconds: 10, SortOrder: 2}
	b := &Subscription{Name: "B", URL: "https://b.example/sub", Enabled: true, RefreshIntervalSeconds: 120, RefreshTimeoutSeconds: 20, SortOrder: 1}
	if err := db.CreateSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetSubscriptionByURL(ctx, a.URL); err != nil || got == nil || got.ID != a.ID {
		t.Fatalf("get by URL: got=%+v err=%v", got, err)
	}
	if err := db.CreateSubscription(ctx, &Subscription{URL: a.URL}); err == nil {
		t.Fatal("duplicate subscription URL succeeded")
	}

	a.Name = "A updated"
	a.RefreshTimeoutSeconds = 15
	if err := db.UpdateSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListSubscriptions(ctx)
	if err != nil || len(list) != 2 || list[0].ID != b.ID || list[1].Name != "A updated" {
		t.Fatalf("list subscriptions: got=%+v err=%v", list, err)
	}
	if err := db.ActivateSubscriptionExclusive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	gotA, _ := db.GetSubscription(ctx, a.ID)
	gotB, _ := db.GetSubscription(ctx, b.ID)
	if !gotA.Enabled || gotB.Enabled {
		t.Fatalf("exclusive activation: A=%v B=%v", gotA.Enabled, gotB.Enabled)
	}
	if err := db.SetSubscriptionEnabled(ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}

	shared := SubscriptionNodeInput{URI: "http://user:pass@shared.example:8080", Name: "shared", Port: 8080, Enabled: true}
	if err := db.ReplaceSubscriptionNodes(ctx, a.ID, []SubscriptionNodeInput{shared, {URI: "http://a.example:80", Name: "a-only", Port: 80, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubscriptionNodes(ctx, b.ID, []SubscriptionNodeInput{shared, {URI: "http://b.example:81", Name: "b-only", Port: 81, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	sharedNode, err := db.GetNodeByURI(ctx, shared.URI)
	if err != nil || sharedNode == nil {
		t.Fatalf("get shared node: node=%+v err=%v", sharedNode, err)
	}
	sharedID := sharedNode.ID
	sharedNode.Enabled = false
	if err := db.UpdateNode(ctx, sharedNode); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeStats(ctx, &NodeStats{NodeID: sharedID, SuccessCount: 7, LastLatencyMs: 42}); err != nil {
		t.Fatal(err)
	}

	snapshotTime := time.Now().UTC().Truncate(time.Second)
	shared.Name = "shared refreshed"
	if err := db.CommitSnapshot(ctx, a.ID, []SubscriptionNodeInput{shared}, SubscriptionSnapshot{
		Attempt: snapshotTime, Success: snapshotTime, ETag: "v2", LastModified: "yesterday",
	}); err != nil {
		t.Fatal(err)
	}
	sharedNode, _ = db.GetNodeByURI(ctx, shared.URI)
	stats, _ := db.GetNodeStats(ctx, sharedID)
	if sharedNode.ID != sharedID || sharedNode.Enabled || sharedNode.Name != "shared refreshed" {
		t.Fatalf("node identity/state not preserved: %+v", sharedNode)
	}
	if stats == nil || stats.SuccessCount != 7 || stats.LastLatencyMs != 42 {
		t.Fatalf("node stats not preserved: %+v", stats)
	}
	bNodes, err := db.ListSubscriptionNodes(ctx, b.ID)
	if err != nil || len(bNodes) != 2 || bNodes[0].Node.ID != sharedID {
		t.Fatalf("B membership changed: nodes=%+v err=%v", bNodes, err)
	}
	gotA, _ = db.GetSubscription(ctx, a.ID)
	if gotA.NodeCount != 1 || gotA.ETag != "v2" || !gotA.LastSuccess.Equal(snapshotTime) {
		t.Fatalf("snapshot metadata not committed: %+v", gotA)
	}

	effective, err := db.ListEffectiveSubscriptionNodes(ctx)
	if err != nil || len(effective) != 1 || effective[0].URI != "http://b.example:81" {
		t.Fatalf("effective nodes: got=%+v err=%v", effective, err)
	}
	if err := db.ReplaceSubscriptionNodes(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	bNodes, _ = db.ListSubscriptionNodes(ctx, b.ID)
	if len(bNodes) != 2 {
		t.Fatalf("empty A snapshot changed B: %+v", bNodes)
	}
	if err := db.DeleteSubscription(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if node, _ := db.GetNodeByURI(ctx, shared.URI); node == nil || node.ID != sharedID {
		t.Fatalf("deleting subscription deleted node: %+v", node)
	}
	if deleted, _ := db.GetSubscription(ctx, a.ID); deleted != nil {
		t.Fatalf("subscription was not deleted: %+v", deleted)
	}
}

func TestListManagedNodes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "managed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manual := &Node{URI: "http://manual.example:80", Name: "manual", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, manual); err != nil {
		t.Fatal(err)
	}
	enabled := &Subscription{Name: "enabled", URL: "https://enabled.example/sub", Enabled: true}
	disabled := &Subscription{Name: "disabled", URL: "https://disabled.example/sub", Enabled: false}
	if err := db.CreateSubscription(ctx, enabled); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	shared := SubscriptionNodeInput{URI: "http://shared.example:80", Name: "shared", Enabled: true}
	disabledNode := SubscriptionNodeInput{URI: "http://node-disabled.example:80", Name: "node-disabled", Enabled: false}
	if err := db.ReplaceSubscriptionNodes(ctx, enabled.ID, []SubscriptionNodeInput{shared, disabledNode}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubscriptionNodes(ctx, disabled.ID, []SubscriptionNodeInput{
		shared, {URI: "http://disabled-only.example:80", Name: "disabled-only", Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	orphan := &Node{URI: "http://orphan.example:80", Name: "orphan", Source: NodeSourceSubscription, Enabled: true}
	if err := db.CreateNode(ctx, orphan); err != nil {
		t.Fatal(err)
	}

	nodes, err := db.ListManagedNodes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]ManagedNode, len(nodes))
	for _, node := range nodes {
		byName[node.Name] = node
	}
	if len(nodes) != 3 || byName[manual.Name].Name == "" || byName[disabledNode.Name].Enabled {
		t.Fatalf("managed nodes did not include manual and disabled nodes: %+v", nodes)
	}
	if got := byName[shared.Name].SubscriptionIDs; len(got) != 1 || got[0] != enabled.ID {
		t.Fatalf("shared enabled memberships = %v, want [%d]", got, enabled.ID)
	}
	if _, ok := byName["disabled-only"]; ok {
		t.Fatalf("disabled subscription-only node is visible: %+v", nodes)
	}
	if _, ok := byName[orphan.Name]; ok {
		t.Fatalf("orphan subscription node is visible: %+v", nodes)
	}

	filtered, err := db.ListManagedNodes(ctx, &enabled.ID)
	if err != nil || len(filtered) != 2 {
		t.Fatalf("enabled subscription filter: nodes=%+v err=%v", filtered, err)
	}
	filtered, err = db.ListManagedNodes(ctx, &disabled.ID)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("disabled subscription filter: nodes=%+v err=%v", filtered, err)
	}
}

func TestUpdateAllSubscriptionRefreshSettings(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "refresh-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sub := &Subscription{Name: "A", URL: "https://a.example/sub", Enabled: true, RefreshIntervalSeconds: 60, RefreshTimeoutSeconds: 10}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAllSubscriptionRefreshSettings(ctx, 300, 0); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetSubscription(ctx, sub.ID)
	if err != nil || got.RefreshIntervalSeconds != 300 || got.RefreshTimeoutSeconds != 10 {
		t.Fatalf("interval-only update: got=%+v err=%v", got, err)
	}
	if err := db.UpdateAllSubscriptionRefreshSettings(ctx, 0, 25); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetSubscription(ctx, sub.ID)
	if err != nil || got.RefreshIntervalSeconds != 300 || got.RefreshTimeoutSeconds != 25 {
		t.Fatalf("timeout-only update: got=%+v err=%v", got, err)
	}
}
