package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"easy_proxies/internal/group"
	json "easy_proxies/internal/jsonx"
)

func TestGroupHealPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "capacity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g := &GroupPool{Name: "capacity", HealPolicy: group.HealPolicy{MinHealthy: 2, TargetSize: 4, Refill: []string{group.PromoteStandby, group.ProbeQuarantined}}}
	if err := db.CreateGroupPool(ctx, g); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.GetGroupPool(ctx, g.ID)
	if err != nil || loaded == nil || !reflect.DeepEqual(loaded.HealPolicy, g.HealPolicy) {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	g.Execute = true
	if err := db.UpdateGroupPool(ctx, g); err != nil {
		t.Fatal(err)
	}
	groups, err := db.ListGroupPools(ctx)
	if err != nil || len(groups) != 1 || !groups[0].Execute {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	encoded, err := json.Marshal(groups[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["min_healthy"] != float64(2) || fields["target_size"] != float64(4) || fields["healing_enabled"] != true {
		t.Fatalf("policy not flattened in API: %s", encoded)
	}
	g.MinHealthy = 5
	if err := db.UpdateGroupPool(ctx, g); err == nil {
		t.Fatal("stored impossible capacity")
	}
}

func TestGroupHealingAuditDedupRetentionAndCascade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := &GroupPool{Name: "audit"}
	if err := db.CreateGroupPool(ctx, g); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	event := GroupHealEvent{GroupID: g.ID, Status: "dry_run", CreatedAt: now, Action: group.HealAction{Kind: group.CapacityAlert, Reason: "below minimum"}}
	if inserted, err := db.AppendGroupHealEvent(ctx, event, time.Hour); err != nil || !inserted {
		t.Fatalf("insert=%v %v", inserted, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	event.CreatedAt = now.Add(time.Minute)
	if inserted, err := db.AppendGroupHealEvent(ctx, event, time.Hour); err != nil || inserted {
		t.Fatalf("restart dedup=%v %v", inserted, err)
	}
	// A changed outcome must be visible even within the dedup window.
	event.Status = "failed"
	if inserted, err := db.AppendGroupHealEvent(ctx, event, time.Hour); err != nil || !inserted {
		t.Fatalf("outcome=%v %v", inserted, err)
	}
	events, err := db.ListGroupHealEvents(ctx, g.ID, 100)
	if err != nil || len(events) != 2 || events[0].Status != "failed" {
		t.Fatalf("events=%+v %v", events, err)
	}
	event.CreatedAt = now.Add(31 * 24 * time.Hour)
	if _, err := db.AppendGroupHealEvent(ctx, event, time.Hour); err != nil {
		t.Fatal(err)
	}
	events, err = db.ListGroupHealEvents(ctx, g.ID, 100)
	if err != nil || len(events) != 1 {
		t.Fatalf("expired audit retained: %+v %v", events, err)
	}
	if err := db.DeleteGroupPool(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	events, err = db.ListGroupHealEvents(ctx, g.ID, 100)
	if err != nil || len(events) != 0 {
		t.Fatalf("orphan audit: %+v %v", events, err)
	}
}
