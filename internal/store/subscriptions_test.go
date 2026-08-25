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
	if gotA.NodeCount != 2 || gotA.ETag != "v2" || !gotA.LastSuccess.Equal(snapshotTime) {
		t.Fatalf("snapshot metadata not committed: %+v", gotA)
	}

	effective, err := db.ListEffectiveSubscriptionNodes(ctx)
	if err != nil || len(effective) != 2 || effective[0].URI != "http://a.example:80" || effective[1].URI != "http://b.example:81" {
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

func TestCommitSnapshotMergesAndRetainsMissingNodes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "merge", URL: "https://merge.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	a := SubscriptionNodeInput{URI: "http://a.example:80", Name: "A", Region: "hk", Country: "Hong Kong", Enabled: true}
	b := SubscriptionNodeInput{URI: "http://b.example:80", Name: "B", Enabled: true}
	first := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a, b}, SubscriptionSnapshot{Attempt: first, Success: first, ETag: "first"}); err != nil {
		t.Fatal(err)
	}
	aNode, err := db.GetNodeByURI(ctx, a.URI)
	if err != nil || aNode == nil {
		t.Fatalf("A node=%+v err=%v", aNode, err)
	}
	if err := db.UpsertNodeStats(ctx, &NodeStats{NodeID: aNode.ID, SuccessCount: 9, LastLatencyMs: 37}); err != nil {
		t.Fatal(err)
	}
	second := time.Now().UTC().Truncate(time.Second)
	b.Name, b.Port = "B updated", 8080
	c := SubscriptionNodeInput{URI: "http://c.example:80", Name: "C", Enabled: true}
	if err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{b, c}, SubscriptionSnapshot{Attempt: second, Success: second, ETag: "second"}); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListSubscriptionNodes(ctx, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{b.URI, c.URI, a.URI}
	if len(members) != len(wantOrder) {
		t.Fatalf("merged members=%+v", members)
	}
	for index, uri := range wantOrder {
		if members[index].Node.URI != uri {
			t.Fatalf("member order=%+v, want=%v", members, wantOrder)
		}
	}
	reloadedA, _ := db.GetNodeByURI(ctx, a.URI)
	stats, _ := db.GetNodeStats(ctx, aNode.ID)
	if reloadedA == nil || reloadedA.ID != aNode.ID || reloadedA.Region != "hk" || reloadedA.Country != "Hong Kong" {
		t.Fatalf("retained node identity/metadata changed: %+v", reloadedA)
	}
	if stats == nil || stats.SuccessCount != 9 || stats.LastLatencyMs != 37 {
		t.Fatalf("retained stats changed: %+v", stats)
	}
	updatedSub, _ := db.GetSubscription(ctx, sub.ID)
	if updatedSub.NodeCount != 3 || updatedSub.ETag != "second" {
		t.Fatalf("merged snapshot metadata=%+v", updatedSub)
	}
	if err := db.SetSubscriptionEnabled(ctx, sub.ID, false); err != nil {
		t.Fatal(err)
	}
	effective, err := db.ListEffectiveSubscriptionNodes(ctx)
	if err != nil || len(effective) != 0 {
		t.Fatalf("disabled retained members remained effective: nodes=%+v err=%v", effective, err)
	}
	if err := db.SetSubscriptionEnabled(ctx, sub.ID, true); err != nil {
		t.Fatal(err)
	}
	effective, err = db.ListEffectiveSubscriptionNodes(ctx)
	if err != nil || len(effective) != 3 {
		t.Fatalf("re-enabled retained members were not restored: nodes=%+v err=%v", effective, err)
	}
	if err := db.CommitSnapshot(ctx, sub.ID, nil, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now(), ETag: "empty"}); err == nil {
		t.Fatal("empty snapshot unexpectedly replaced retained members")
	}
	afterEmpty, _ := db.ListSubscriptionNodes(ctx, sub.ID)
	afterEmptySub, _ := db.GetSubscription(ctx, sub.ID)
	if len(afterEmpty) != 3 || afterEmptySub.ETag != "second" || afterEmptySub.NodeCount != 3 {
		t.Fatalf("empty snapshot changed state: members=%+v subscription=%+v", afterEmpty, afterEmptySub)
	}
}

func TestDeleteSubscriptionPreservesExclusiveNodesAsManual(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "delete-preserve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &Subscription{Name: "A", URL: "https://a.example/sub", Enabled: true}
	b := &Subscription{Name: "B", URL: "https://b.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(ctx, b); err != nil {
		t.Fatal(err)
	}
	shared := SubscriptionNodeInput{URI: "http://shared.example:80", Name: "shared", Enabled: true}
	exclusive := SubscriptionNodeInput{URI: "http://exclusive.example:80", Name: "exclusive", Enabled: true}
	if err := db.ReplaceSubscriptionNodes(ctx, a.ID, []SubscriptionNodeInput{exclusive, shared}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubscriptionNodes(ctx, b.ID, []SubscriptionNodeInput{shared}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSubscription(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	exclusiveNode, _ := db.GetNodeByURI(ctx, exclusive.URI)
	sharedNode, _ := db.GetNodeByURI(ctx, shared.URI)
	if exclusiveNode == nil || exclusiveNode.Source != NodeSourceManual || !exclusiveNode.Enabled {
		t.Fatalf("exclusive node was not preserved as manual: %+v", exclusiveNode)
	}
	if sharedNode == nil || sharedNode.Source != NodeSourceSubscription {
		t.Fatalf("shared node source changed: %+v", sharedNode)
	}
	bMembers, err := db.ListSubscriptionNodes(ctx, b.ID)
	if err != nil || len(bMembers) != 1 || bMembers[0].Node.ID != sharedNode.ID {
		t.Fatalf("remaining subscription membership=%+v err=%v", bMembers, err)
	}
}

func TestAdoptOrphanSubscriptionNodes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "orphans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	orphan := &Node{URI: "http://orphan.example:80", Name: "orphan", Source: NodeSourceSubscription,
		Region: "hk", Country: "Hong Kong", Enabled: true}
	if err := db.CreateNode(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeStats(ctx, &NodeStats{NodeID: orphan.ID, SuccessCount: 4, LastLatencyMs: 51}); err != nil {
		t.Fatal(err)
	}
	adopted, err := db.AdoptOrphanSubscriptionNodes(ctx)
	if err != nil || adopted != 1 {
		t.Fatalf("adopted=%d err=%v", adopted, err)
	}
	recovered, _ := db.GetNode(ctx, orphan.ID)
	stats, _ := db.GetNodeStats(ctx, orphan.ID)
	if recovered == nil || recovered.Source != NodeSourceManual || !recovered.Enabled || recovered.Region != "hk" || recovered.Country != "Hong Kong" {
		t.Fatalf("recovered orphan=%+v", recovered)
	}
	if stats == nil || stats.SuccessCount != 4 || stats.LastLatencyMs != 51 {
		t.Fatalf("recovered stats=%+v", stats)
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
