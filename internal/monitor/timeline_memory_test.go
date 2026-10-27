package monitor

import "testing"

func TestUnusedNodeTimelineIsLazyAndMigrationPreservesBoundedHistory(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	mgr.Register(NodeInfo{NodeID: 1, Tag: "old", URI: "http://127.0.0.1:80"})
	entry := mgr.registry.byTagEntry("old")
	if cap(entry.timeline) != 0 {
		t.Fatal("unused node preallocated history")
	}
	for i := 0; i < maxTimelineSize+5; i++ {
		entry.recordSuccess("target")
	}
	before := entry.snapshot()
	if len(before.Timeline) != maxTimelineSize || before.SuccessCount != maxTimelineSize+5 {
		t.Fatal("history bound or success count changed")
	}
	mgr.MigrateRuntimeTag(1, NodeInfo{NodeID: 1, Tag: "new", URI: "http://127.0.0.1:80"})
	after := mgr.SnapshotForTag("new")
	if after == nil || len(after.Timeline) != maxTimelineSize || after.SuccessCount != before.SuccessCount {
		t.Fatal("migration lost history")
	}
	after.Timeline[0].Destination = "changed"
	if mgr.SnapshotForTag("new").Timeline[0].Destination != "target" {
		t.Fatal("history snapshot aliases live buffer")
	}
	mgr.MigrateRuntimeTag(2, NodeInfo{NodeID: 2, Tag: "fresh", URI: "http://127.0.0.1:81"})
	if cap(mgr.registry.byTagEntry("fresh").timeline) != 0 {
		t.Fatal("new migrated identity preallocated history")
	}
}
