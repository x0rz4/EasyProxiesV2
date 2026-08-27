package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestNodeDetectionNullableRoundTripAndTaskPruning(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "diagnostics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &Node{URI: "http://user:pass@127.0.0.1:8080", Name: "diagnostic", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := db.UpsertNodeDetectionResult(ctx, &NodeDetectionResult{NodeID: node.ID, TaskID: "one", LatencyStatus: "success", LatencyMs: &zero, SpeedStatus: "untested", ExitIPStatus: "success", ExitIP: "1.1.1.1", ExitIPFamily: "ipv4", ExitCountry: "Australia", ExitCountryCode: "AU"}); err != nil {
		t.Fatal(err)
	}
	results, err := db.ListNodeDetectionResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if results[node.ID].LatencyMs == nil || *results[node.ID].LatencyMs != 0 || results[node.ID].AverageBytesPerSecond != nil {
		t.Fatalf("nullable fields lost: %+v", results[node.ID])
	}
	if results[node.ID].ExitIP != "1.1.1.1" || results[node.ID].ExitCountry != "Australia" || results[node.ID].ExitCountryCode != "AU" {
		t.Fatalf("landing IP country lost: %+v", results[node.ID])
	}
	falseValue := false
	score := 0
	if err := db.UpsertNodeIPQualityResult(ctx, &NodeIPQualityResult{NodeID: node.ID, Provider: "ippure", Status: "success", IsBroadcast: &falseValue, FraudScore: &score, CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	quality, err := db.ListNodeIPQualityResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if quality[node.ID][0].IsBroadcast == nil || *quality[node.ID][0].IsBroadcast || quality[node.ID][0].FraudScore == nil || *quality[node.ID][0].FraudScore != 0 {
		t.Fatalf("quality nullable fields lost: %+v", quality[node.ID][0])
	}
	for index := 0; index < 25; index++ {
		id := time.Now().Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano)
		if err := db.UpsertNodeDetectionTask(ctx, &NodeDetectionTask{ID: id, Status: "completed", CreatedAt: time.Now().Add(time.Duration(index) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PruneNodeDetectionTasks(ctx, 20); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.ListNodeDetectionTasks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 20 {
		t.Fatalf("tasks=%d, want 20", len(tasks))
	}
}

func TestNodeConnectionIdentityChangeClearsDetectionCache(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "identity-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &Node{URI: "http://user:old@127.0.0.1:8080", Name: "cached", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeDetectionResult(ctx, &NodeDetectionResult{NodeID: node.ID, LatencyStatus: "untested", SpeedStatus: "untested", ExitIPStatus: "success", ExitIP: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeIPQualityResult(ctx, &NodeIPQualityResult{NodeID: node.ID, Provider: "ippure", Status: "success"}); err != nil {
		t.Fatal(err)
	}

	// A display-only rename preserves the cache.
	node.Name = "renamed"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	results, err := db.ListNodeDetectionResults(ctx)
	if err != nil || results[node.ID] == nil {
		t.Fatalf("rename unexpectedly cleared landing cache: results=%v err=%v", results, err)
	}

	// Authentication is part of the semantic identity, so changing it must
	// force one new landing-IP check on the next reload/start.
	node.URI = "http://user:new@127.0.0.1:8080"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	results, err = db.ListNodeDetectionResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if results[node.ID] != nil {
		t.Fatalf("connection identity change retained stale landing cache: %+v", results[node.ID])
	}
	quality, err := db.ListNodeIPQualityResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(quality[node.ID]) != 0 {
		t.Fatalf("connection identity change retained stale quality cache: %+v", quality[node.ID])
	}
}

// TestIdentityChangeDropsAutoTagsOnly pins the coupling between cached facts and
// auto tags: an auto tag must not outlive the evidence it was derived from, while
// operator-entered tags survive.
func TestIdentityChangeDropsAutoTagsOnly(t *testing.T) {
	ctx := context.Background()
	opened, err := Open(filepath.Join(t.TempDir(), "identity-autotags.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	db := opened.(*sqliteStore)

	node := &Node{URI: "http://user:old@127.0.0.1:8080", Name: "tagged", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	manual := &Tag{Name: "hand-picked"}
	auto := &Tag{Name: "derived", AutoEnabled: true}
	if err := db.CreateTag(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTag(ctx, auto); err != nil {
		t.Fatal(err)
	}
	if err := db.SetManualNodeTags(ctx, node.ID, []int64{manual.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{{NodeID: node.ID, TagIDs: []int64{auto.ID}}}); err != nil {
		t.Fatal(err)
	}

	node.URI = "http://user:new@127.0.0.1:8080"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}

	assignments, err := db.ListNodeTags(ctx, NodeTagFilter{NodeIDs: []int64{node.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].TagID != manual.ID || assignments[0].Source != NodeTagSourceManual {
		t.Fatalf("assignments = %+v, want only the manual one", assignments)
	}
	reloaded, err := db.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Tags) != 1 || reloaded.Tags[0] != "hand-picked" {
		t.Fatalf("projection = %v, want [hand-picked]", reloaded.Tags)
	}
}

func TestUpdateNodeLocationOnlyChangesLandingMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "location.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &Node{URI: "http://user:pass@127.0.0.1:8080", Name: "stable", Source: NodeSourceManual, Enabled: false, Tags: []string{"kept"}}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeLocation(ctx, node.ID, "HK", "Hong Kong"); err != nil {
		t.Fatal(err)
	}
	nodes, err := db.ListNodes(ctx, NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Region != "hk" || nodes[0].Country != "Hong Kong" || nodes[0].Enabled || nodes[0].URI != node.URI || len(nodes[0].Tags) != 1 || nodes[0].Tags[0] != "kept" {
		t.Fatalf("specialized landing update changed unrelated fields: %+v", nodes)
	}
}
