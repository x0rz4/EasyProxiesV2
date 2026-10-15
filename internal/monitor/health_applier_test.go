package monitor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

func newPersistentHealthFixture(t *testing.T) (*Manager, store.Store, *store.Node) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	node := &store.Node{Name: "health", URI: "http://127.0.0.1:12345", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Stop)
	mgr.SetProbeStore(db)
	mgr.Register(NodeInfo{NodeID: node.ID, URI: node.URI, Name: node.Name, Tag: "health@v1"})
	return mgr, db, node
}

func TestPersistentHealthProjectionIsIdempotent(t *testing.T) {
	mgr, db, node := newPersistentHealthFixture(t)
	handle := mgr.HandleForNodeID(node.ID)
	handle.SetProbe(func(context.Context) (time.Duration, error) { return 17 * time.Millisecond, nil })
	if _, err := mgr.Probe(context.Background(), "health@v1"); err != nil {
		t.Fatal(err)
	}
	before := mgr.SnapshotForNodeID(node.ID)
	if !before.Available || before.SuccessCount != 1 {
		t.Fatalf("not projected: %+v", before)
	}
	receipt, err := db.GetProbeReceipt(context.Background(), node.ID, "health")
	if err != nil || receipt == nil {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	var out probe.Outcome
	if err := json.Unmarshal([]byte(receipt.ResultJSON), &out); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ProjectHealthOutcome(out); err != nil {
		t.Fatal(err)
	}
	if after := mgr.SnapshotForNodeID(node.ID); after.SuccessCount != before.SuccessCount {
		t.Fatal("replay counted another success")
	}
	// A previous process's receipt must not feed the recovery threshold again.
	mgr.probeEpoch = out.Event.Observation.StartedAt.Add(time.Second)
	out.ReceiptID = "historical"
	out.Event.Observation.CheckedAt = out.Event.Observation.CheckedAt.Add(time.Minute)
	if err := mgr.ProjectHealthOutcome(out); err != nil {
		t.Fatal(err)
	}
	if after := mgr.SnapshotForNodeID(node.ID); after.SuccessCount != before.SuccessCount {
		t.Fatal("historical receipt advanced recovery")
	}
}

func TestHealthResultCannotCrossIdentityReplacement(t *testing.T) {
	mgr, db, node := newPersistentHealthFixture(t)
	mgr.HandleForNodeID(node.ID).SetProbe(func(context.Context) (time.Duration, error) {
		node.URI = "http://127.0.0.2:12345"
		return time.Millisecond, db.UpdateNode(context.Background(), node)
	})
	if _, err := mgr.Probe(context.Background(), "health@v1"); err == nil {
		t.Fatal("stale health result accepted")
	}
	if snapshot := mgr.SnapshotForNodeID(node.ID); snapshot.InitialCheckDone || snapshot.SuccessCount != 0 {
		t.Fatalf("stale result affected runtime: %+v", snapshot)
	}
}
