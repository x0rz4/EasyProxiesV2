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
	if err := db.UpsertNodeDetectionResult(ctx, &NodeDetectionResult{NodeID: node.ID, TaskID: "one", LatencyStatus: "success", LatencyMs: &zero, SpeedStatus: "untested", ExitIPStatus: "untested"}); err != nil {
		t.Fatal(err)
	}
	results, err := db.ListNodeDetectionResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if results[node.ID].LatencyMs == nil || *results[node.ID].LatencyMs != 0 || results[node.ID].AverageBytesPerSecond != nil {
		t.Fatalf("nullable fields lost: %+v", results[node.ID])
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
