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

	a := &Subscription{Name: "A", URL: "https://a.example/sub", Format: "clash", UserAgent: "custom-client", Enabled: true, RefreshIntervalSeconds: 60, RefreshTimeoutSeconds: 10, SortOrder: 2}
	b := &Subscription{Name: "B", URL: "https://b.example/sub", Enabled: true, RefreshIntervalSeconds: 120, RefreshTimeoutSeconds: 20, SortOrder: 1}
	if err := db.CreateSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetSubscriptionByURL(ctx, a.URL); err != nil || got == nil || got.ID != a.ID || got.Format != "clash" || got.UserAgent != "custom-client" {
		t.Fatalf("get by URL: got=%+v err=%v", got, err)
	}
	if err := db.CreateSubscription(ctx, &Subscription{URL: a.URL}); err == nil {
		t.Fatal("duplicate subscription URL succeeded")
	}

	a.Name = "A updated"
	a.RefreshTimeoutSeconds = 15
	if err := db.UpdateSubscriptionSettings(ctx, a.ID, SubscriptionSettings{
		Name: a.Name, URL: a.URL, Format: a.Format, UserAgent: a.UserAgent, Enabled: a.Enabled,
		RefreshIntervalSeconds: a.RefreshIntervalSeconds, RefreshTimeoutSeconds: a.RefreshTimeoutSeconds, SortOrder: a.SortOrder,
	}); err != nil {
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
	if _, err := db.CommitSnapshot(ctx, a.ID, []SubscriptionNodeInput{shared}, SubscriptionSnapshot{
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
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a, b}, SubscriptionSnapshot{Attempt: first, Success: first, ETag: "first"}); err != nil {
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
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{b, c}, SubscriptionSnapshot{Attempt: second, Success: second, ETag: "second"}); err != nil {
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
	if _, err := db.CommitSnapshot(ctx, sub.ID, nil, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now(), ETag: "empty"}); err == nil {
		t.Fatal("empty snapshot unexpectedly replaced retained members")
	}
	afterEmpty, _ := db.ListSubscriptionNodes(ctx, sub.ID)
	afterEmptySub, _ := db.GetSubscription(ctx, sub.ID)
	if len(afterEmpty) != 3 || afterEmptySub.ETag != "second" || afterEmptySub.NodeCount != 3 {
		t.Fatalf("empty snapshot changed state: members=%+v subscription=%+v", afterEmpty, afterEmptySub)
	}
}

func TestCommitSnapshotReconcilesUniqueLogicalNodeInPlace(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "logical-replace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "home", URL: "https://home.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}

	const logicalName = "lite-家庭宽带-香港hgc"
	oldInput := SubscriptionNodeInput{
		URI: "http://old-user:old-pass@old.example:3129", Name: logicalName,
		LogicalName: logicalName, Port: 24038, Region: "hk", Enabled: true,
	}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{oldInput}, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	oldNode, err := db.GetNodeByURI(ctx, oldInput.URI)
	if err != nil || oldNode == nil {
		t.Fatalf("old node=%+v err=%v", oldNode, err)
	}
	oldNode.Enabled = false
	if err := db.UpdateNode(ctx, oldNode); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeStats(ctx, &NodeStats{NodeID: oldNode.ID, SuccessCount: 8, LastLatencyMs: 66, TotalDownloadBytes: 123}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeDetectionResult(ctx, &NodeDetectionResult{NodeID: oldNode.ID, LatencyStatus: "untested", SpeedStatus: "untested", ExitIPStatus: "success", ExitIP: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	group := &GroupPool{
		Name: "HK", BindAddress: "127.0.0.1", BindPort: 10088, Protocol: "mixed", DispatchMode: "fixed",
		ExplicitNodeIDs: []int64{oldNode.ID}, CurrentActiveNodeID: oldNode.ID, Enabled: true,
	}
	if err := db.CreateGroupPool(ctx, group); err != nil {
		t.Fatal(err)
	}

	newInput := SubscriptionNodeInput{
		URI: "http://new-user:new-pass@new.example:8080", Name: logicalName,
		LogicalName: logicalName, Port: 24155, Region: "us", Enabled: true,
	}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{newInput}, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListSubscriptionNodes(ctx, sub.ID)
	if err != nil || len(members) != 1 {
		t.Fatalf("members=%+v err=%v", members, err)
	}
	replaced := members[0].Node
	if replaced.ID != oldNode.ID || replaced.URI != newInput.URI || replaced.Port != oldNode.Port || replaced.Enabled {
		t.Fatalf("logical replacement did not preserve stable state: old=%+v new=%+v", oldNode, replaced)
	}
	if stale, _ := db.GetNodeByURI(ctx, oldInput.URI); stale != nil {
		t.Fatalf("stale logical node was retained: %+v", stale)
	}
	stats, _ := db.GetNodeStats(ctx, oldNode.ID)
	if stats == nil || stats.SuccessCount != 0 || stats.LastLatencyMs != -1 || stats.InitialCheckDone || stats.TotalDownloadBytes != 123 {
		t.Fatalf("logical identity replacement must reset health and preserve traffic: %+v", stats)
	}
	storedGroup, _ := db.GetGroupPool(ctx, group.ID)
	if storedGroup == nil || storedGroup.CurrentActiveNodeID != oldNode.ID || len(storedGroup.ExplicitNodeIDs) != 1 || storedGroup.ExplicitNodeIDs[0] != oldNode.ID {
		t.Fatalf("logical replacement lost group references: %+v", storedGroup)
	}
	detection, err := db.ListNodeDetectionResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if detection[oldNode.ID] != nil {
		t.Fatalf("logical connection replacement retained stale landing IP: %+v", detection[oldNode.ID])
	}
}

func TestCommitSnapshotCleansAccumulatedLogicalDuplicatesUsingOldestNode(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "logical-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "legacy", URL: "https://legacy.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	const logicalName = "lite-家庭宽带-香港hgc"
	oldInput := SubscriptionNodeInput{URI: "http://first:pass@one.example:3129", Name: logicalName, Port: 24038, Enabled: true}
	currentInput := SubscriptionNodeInput{URI: "http://second:pass@two.example:3129", Name: logicalName, Port: 24155, Enabled: true}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{oldInput}, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	oldNode, _ := db.GetNodeByURI(ctx, oldInput.URI)
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{currentInput}, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before, _ := db.ListSubscriptionNodes(ctx, sub.ID)
	if len(before) != 2 {
		t.Fatalf("legacy setup did not contain two nodes: %+v", before)
	}
	currentInput.LogicalName = logicalName
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{currentInput}, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	after, err := db.ListSubscriptionNodes(ctx, sub.ID)
	if err != nil || len(after) != 1 {
		t.Fatalf("logical duplicates were not cleaned: members=%+v err=%v", after, err)
	}
	if after[0].Node.ID != oldNode.ID || after[0].Node.Port != oldNode.Port || after[0].Node.URI != currentInput.URI {
		t.Fatalf("oldest stable node was not reused: old=%+v current=%+v", oldNode, after[0].Node)
	}
}

func TestCommitSnapshotKeepsDistinctIncomingNodesWithSameName(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "same-name.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "same-name", URL: "https://same-name.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	const logicalName = "provider-duplicate-name"
	inputs := []SubscriptionNodeInput{
		{URI: "http://one:pass@one.example:8080", Name: logicalName, LogicalName: logicalName, Enabled: true},
		{URI: "http://two:pass@two.example:8080", Name: logicalName, LogicalName: logicalName, Enabled: true},
	}
	if _, err := db.CommitSnapshot(ctx, sub.ID, inputs, SubscriptionSnapshot{Attempt: time.Now(), Success: time.Now()}); err != nil {
		t.Fatal(err)
	}
	members, err := db.ListSubscriptionNodes(ctx, sub.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("legitimate same-name nodes were merged: members=%+v err=%v", members, err)
	}
}

func TestUpdateSubscriptionSettingsPreservesRefreshMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "keep", URL: "https://keep.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	committedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{{URI: "http://a.example:80", Name: "A", Enabled: true}},
		SubscriptionSnapshot{Attempt: committedAt, Success: committedAt, ETag: `"v1"`, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"}); err != nil {
		t.Fatal(err)
	}
	settings := SubscriptionSettings{Name: "renamed", URL: sub.URL, Format: "auto", Enabled: true,
		RefreshIntervalSeconds: 600, RefreshTimeoutSeconds: 15}
	if err := db.UpdateSubscriptionSettings(ctx, sub.ID, settings); err != nil {
		t.Fatal(err)
	}
	// A rename must not rewind the last successful snapshot or its validators.
	got, _ := db.GetSubscription(ctx, sub.ID)
	if got.Name != "renamed" || got.NodeCount != 1 || !got.LastSuccess.Equal(committedAt) || got.ETag != `"v1"` || got.LastModified == "" {
		t.Fatalf("metadata after rename=%+v", got)
	}
	settings.URL = "https://other.example/sub"
	if err := db.UpdateSubscriptionSettings(ctx, sub.ID, settings); err != nil {
		t.Fatal(err)
	}
	// A new URL describes a different resource, so the validators must go.
	got, _ = db.GetSubscription(ctx, sub.ID)
	if got.URL != settings.URL || got.ETag != "" || got.LastModified != "" {
		t.Fatalf("validators survived a URL change: %+v", got)
	}
	if got.LastSuccess.IsZero() {
		t.Fatalf("URL change reset last success: %+v", got)
	}
}

func TestRecordSubscriptionFailurePreservesUserEdits(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "failing", URL: "https://failing.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	successAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{{URI: "http://a.example:80", Name: "A", Enabled: true}},
		SubscriptionSnapshot{Attempt: successAt, Success: successAt, ETag: `"v1"`}); err != nil {
		t.Fatal(err)
	}
	// Simulate a settings edit during a slow fetch, then the failure write.
	if err := db.UpdateSubscriptionSettings(ctx, sub.ID, SubscriptionSettings{Name: "edited", URL: sub.URL, Format: "auto",
		Enabled: false, RefreshIntervalSeconds: 900, RefreshTimeoutSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	attempt := time.Now().UTC().Truncate(time.Second)
	if err := db.RecordSubscriptionFailure(ctx, sub.ID, attempt, "status 502"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSubscription(ctx, sub.ID)
	if got.Name != "edited" || got.Enabled || got.RefreshIntervalSeconds != 900 {
		t.Fatalf("failure write reverted user settings: %+v", got)
	}
	if got.LastError != "status 502" || !got.LastAttempt.Equal(attempt) || !got.LastSuccess.Equal(successAt) {
		t.Fatalf("failure metadata=%+v", got)
	}
	if got.ETag != `"v1"` || got.NodeCount != 1 {
		t.Fatalf("failure write touched snapshot metadata: %+v", got)
	}
	notModifiedAt := attempt.Add(time.Minute)
	if err := db.RecordSubscriptionNotModified(ctx, sub.ID, notModifiedAt); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetSubscription(ctx, sub.ID)
	if got.LastError != "" || !got.LastAttempt.Equal(notModifiedAt) {
		t.Fatalf("304 metadata=%+v", got)
	}
}

func TestCommitSnapshotPrunesNodesMissingFromConsecutiveSnapshots(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "prune.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "aging", URL: "https://aging.example/sub", Enabled: true}
	other := &Subscription{Name: "other", URL: "https://other.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSubscription(ctx, other); err != nil {
		t.Fatal(err)
	}
	a := SubscriptionNodeInput{URI: "http://a.example:80", Name: "A", Enabled: true}
	b := SubscriptionNodeInput{URI: "http://b.example:80", Name: "B", Enabled: true}
	shared := SubscriptionNodeInput{URI: "http://shared.example:80", Name: "shared", Enabled: true}
	if _, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a, b, shared}, SubscriptionSnapshot{RemoveAfterMisses: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubscriptionNodes(ctx, other.ID, []SubscriptionNodeInput{shared}); err != nil {
		t.Fatal(err)
	}
	bNode, err := db.GetNodeByURI(ctx, b.URI)
	aNode, err := db.GetNodeByURI(ctx, a.URI)
	if err != nil || bNode == nil || aNode == nil {
		t.Fatalf("nodes: A=%+v B=%+v err=%v", aNode, bNode, err)
	}
	group := &GroupPool{Name: "aging", BindAddress: "127.0.0.1", BindPort: 10188, Protocol: "mixed",
		DispatchMode: "fixed", ExplicitNodeIDs: []int64{bNode.ID, aNode.ID}, ExcludedNodeIDs: []int64{bNode.ID},
		CurrentActiveNodeID: bNode.ID, Enabled: true}
	if err := db.CreateGroupPool(ctx, group); err != nil {
		t.Fatal(err)
	}

	// First omission only ages the member.
	result, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a}, SubscriptionSnapshot{RemoveAfterMisses: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 0 || result.NodeCount != 3 {
		t.Fatalf("first omission result=%+v", result)
	}
	members, _ := db.ListSubscriptionNodes(ctx, sub.ID)
	missed := map[string]int{}
	for _, member := range members {
		missed[member.Node.URI] = member.MissedRefreshes
	}
	if missed[b.URI] != 1 || missed[shared.URI] != 1 || missed[a.URI] != 0 {
		t.Fatalf("missed counters=%v", missed)
	}

	// Second omission removes both stale memberships; only B loses its node,
	// because the shared node still belongs to the other subscription.
	result, err = db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a}, SubscriptionSnapshot{RemoveAfterMisses: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.NodeCount != 1 {
		t.Fatalf("second omission result=%+v", result)
	}
	if gone, _ := db.GetNodeByURI(ctx, b.URI); gone != nil {
		t.Fatalf("stale exclusive node survived: %+v", gone)
	}
	if kept, _ := db.GetNodeByURI(ctx, shared.URI); kept == nil {
		t.Fatal("node shared with another subscription was deleted")
	}
	otherMembers, _ := db.ListSubscriptionNodes(ctx, other.ID)
	if len(otherMembers) != 1 {
		t.Fatalf("other subscription membership=%+v", otherMembers)
	}
	storedGroup, _ := db.GetGroupPool(ctx, group.ID)
	if storedGroup == nil {
		t.Fatal("group disappeared")
	}
	if len(storedGroup.ExplicitNodeIDs) != 1 || storedGroup.ExplicitNodeIDs[0] != aNode.ID {
		t.Fatalf("explicit references=%v", storedGroup.ExplicitNodeIDs)
	}
	if len(storedGroup.ExcludedNodeIDs) != 0 {
		t.Fatalf("excluded references=%v", storedGroup.ExcludedNodeIDs)
	}
	if storedGroup.CurrentActiveNodeID != 0 {
		t.Fatalf("active pointer=%d", storedGroup.CurrentActiveNodeID)
	}
}

func TestCommitSnapshotReportsCreatedAndUpdated(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "counts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sub := &Subscription{Name: "counts", URL: "https://counts.example/sub", Enabled: true}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	a := SubscriptionNodeInput{URI: "http://a.example:80", Name: "A", Enabled: true}
	result, err := db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a}, SubscriptionSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 1 || result.Updated != 0 || result.NodeCount != 1 {
		t.Fatalf("first commit=%+v", result)
	}
	result, err = db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{a}, SubscriptionSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || result.Updated != 1 {
		t.Fatalf("repeat commit=%+v", result)
	}
	// A rotated endpoint under the same provider name reuses the node in place.
	rotated := SubscriptionNodeInput{URI: "http://a2.example:80", Name: "A", LogicalName: "A", Enabled: true}
	result, err = db.CommitSnapshot(ctx, sub.ID, []SubscriptionNodeInput{rotated}, SubscriptionSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || result.Updated != 1 {
		t.Fatalf("logical reuse commit=%+v", result)
	}
}

func TestResetSubscriptionNodesRemovesExclusiveNodes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "reset.db"))
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
	if err := db.ResetSubscriptionNodes(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if gone, _ := db.GetNodeByURI(ctx, exclusive.URI); gone != nil {
		t.Fatalf("exclusive node survived reset: %+v", gone)
	}
	if kept, _ := db.GetNodeByURI(ctx, shared.URI); kept == nil {
		t.Fatal("shared node was deleted")
	}
	members, _ := db.ListSubscriptionNodes(ctx, a.ID)
	if len(members) != 0 {
		t.Fatalf("members after reset=%+v", members)
	}
	if got, _ := db.GetSubscription(ctx, a.ID); got.NodeCount != 0 {
		t.Fatalf("node count after reset=%+v", got)
	}
	otherMembers, _ := db.ListSubscriptionNodes(ctx, b.ID)
	if len(otherMembers) != 1 {
		t.Fatalf("other membership after reset=%+v", otherMembers)
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
