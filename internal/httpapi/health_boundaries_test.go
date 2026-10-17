package httpapi

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"easy_proxies/internal/group"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/noderesult"
	"easy_proxies/internal/probe"
	"easy_proxies/internal/store"
)

func TestSuccessfulDiagnosticsDoNotRecoverEvictedMember(t *testing.T) {
	for _, kind := range []probe.Kind{probe.Latency, probe.Speed, probe.Quality, probe.Unlock} {
		t.Run(string(kind), func(t *testing.T) {
			group.Reset()
			defer group.Reset()
			mgr, db, node := newHealthFixture(t)
			members := map[string]group.GroupInitialState{"health@v1": {NodeID: node.ID}}
			policy := group.RecoveryPolicy{Enabled: true, SuccessThreshold: 3}
			group.Register(1, time.Minute, 1, "health@v1", members, policy)
			group.Register(2, time.Minute, 1, "health@v1", members, policy)
			group.RecordFailure(1, "health@v1", errors.New("down"), time.Now())
			group.RecordHealthSuccess(1, "health@v1")
			before := group.GroupRuntimeSnapshotsReadonly()
			if member := before[1].Members[0]; member.Status != "EVICTED" || member.RecoverySuccesses != 1 {
				t.Fatalf("invalid recovery fixture: %+v", member)
			}
			// A FIFO barrier ensures any accidentally published health event has
			// reached recovery before comparing state, without polling or sleeps.
			barrier := make(chan struct{})
			unsubscribe := mgr.SubscribeHealthResultsFor([]string{"health@v1"}, []int64{node.ID}, func(event monitor.HealthResultEvent) {
				if event.SnapshotOnly {
					close(barrier)
					return
				}
				if event.Success {
					group.RecordHealthSuccess(1, event.Tag)
				} else {
					group.RecordGroupHealthFailure(1, event.Tag, errors.New(event.Error), event.CheckedAt)
				}
			})
			defer unsubscribe()
			server := &Server{store: db, mgr: mgr}
			event, err := server.beginMeasuredResult(context.Background(), mgr.SnapshotForNodeID(node.ID), kind, probe.ManualDiagnostic)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case probe.Latency:
				value := int64(12)
				event.Detection = &store.NodeDetectionResult{NodeID: node.ID, LatencyStatus: "success", LatencyMs: &value}
			case probe.Speed:
				value := int64(1024)
				event.Detection = &store.NodeDetectionResult{NodeID: node.ID, SpeedStatus: "success", AverageBytesPerSecond: &value}
			case probe.Quality:
				event.Quality = &store.NodeIPQualityResult{NodeID: node.ID, Provider: "fixture", Status: "success"}
			case probe.Unlock:
				event.Unlock = &store.UnlockResult{NodeID: node.ID, Tag: "health@v1", Services: []noderesult.Service{}}
			}
			if err := server.applyMeasuredResult(event); err != nil {
				t.Fatal(err)
			}
			mgr.PublishHealthResult(monitor.HealthResultEvent{Tag: "health@v1", NodeID: node.ID, SnapshotOnly: true})
			select {
			case <-barrier:
			case <-time.After(time.Second):
				t.Fatal("health event consumer did not reach barrier")
			}
			if !reflect.DeepEqual(before, group.GroupRuntimeSnapshotsReadonly()) {
				t.Fatal("diagnostic changed global eviction or recovery progress")
			}
			if snapshot := mgr.SnapshotForNodeID(node.ID); snapshot.InitialCheckDone || snapshot.SuccessCount != 0 {
				t.Fatalf("diagnostic changed runtime health: %+v", snapshot)
			}
			receipt, err := db.GetProbeReceipt(context.Background(), node.ID, event.Stream())
			if err != nil || receipt == nil {
				t.Fatalf("diagnostic was not persisted: %+v %v", receipt, err)
			}
			var out probe.Outcome
			if err := json.Unmarshal([]byte(receipt.ResultJSON), &out); err != nil || !out.Applied {
				t.Fatalf("diagnostic not applied: %+v %v", out, err)
			}
			if receipt, err := db.GetProbeReceipt(context.Background(), node.ID, "health"); err != nil || receipt != nil {
				t.Fatalf("diagnostic created health evidence: %+v %v", receipt, err)
			}
		})
	}
}

// newHealthFixture builds the same persistent-node setup the monitor package's
// health tests use. It lives here rather than being imported because Go test
// helpers are not visible across packages, and exporting one would ship test
// setup in the production binary.
func newHealthFixture(t *testing.T) (*monitor.Manager, store.Store, *store.Node) {
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
	mgr, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Stop)
	mgr.SetProbeStore(db)
	mgr.Register(monitor.NodeInfo{NodeID: node.ID, URI: node.URI, Name: node.Name, Tag: "health@v1"})
	return mgr, db, node
}
