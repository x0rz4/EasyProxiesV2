package boxmgr

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

func TestHealingPromotesStandbyWithoutRestartingBoxes(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "healing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &store.Node{Name: "a", URI: "http://127.0.0.1:65530", Source: store.NodeSourceManual, Enabled: true}
	b := &store.Node{Name: "b", URI: "http://127.0.0.2:65530", Source: store.NodeSourceManual, Enabled: true}
	for _, n := range []*store.Node{a, b} {
		if err := db.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: a.ID, Name: a.Name, URI: a.URI}, {ID: b.ID, Name: b.Name, URI: b.URI}},
		Groups: []config.GroupPoolConfig{{ID: 995, Name: "capacity", Enabled: true, BindAddress: "127.0.0.1", BindPort: ports[1], Protocol: "mixed", DispatchMode: "fixed",
			ExplicitNodeIDs: []int64{a.ID, b.ID}, FailureWindow: time.Minute, FailureThreshold: 1, HealthCheckInterval: time.Minute,
			HealPolicy: group.HealPolicy{Execute: true, MinHealthy: 1, TargetSize: 1, Refill: []string{group.PromoteStandby}}}}, LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	mgr := New(cfg, monitor.Config{}, WithStore(db))
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	base, listener := mgr.currentBox, mgr.groupSlot(995).box
	if base == nil || listener == nil {
		t.Fatal("missing boxes")
	}
	for _, node := range []*store.Node{a, b} {
		snapshot := mgr.monitorMgr.SnapshotForNodeID(node.ID)
		if snapshot == nil {
			t.Fatal("missing monitor registration")
		}
		if err := mgr.monitorMgr.MarkAvailableWithoutProbe(snapshot.Tag); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range group.GroupRuntimeSnapshotsReadonly()[995].Members {
		if member.NodeID == a.ID {
			group.RecordFailure(995, member.Tag, nil, time.Now())
		}
	}
	inputs, err := mgr.HealingInputs(ctx, time.Now())
	if err != nil || len(inputs) != 1 {
		t.Fatalf("inputs=%+v %v", inputs, err)
	}
	plan, err := group.PlanHealing(inputs[0])
	if err != nil || !slices.Equal(plan.DesiredIDs, []int64{b.ID}) {
		t.Fatalf("plan=%+v %v", plan, err)
	}
	if err := mgr.ApplyHealingSelection(ctx, 995, plan.DesiredIDs); err != nil {
		t.Fatal(err)
	}
	if mgr.currentBox != base || mgr.groupSlot(995).box != listener {
		t.Fatal("healing restarted a box")
	}
	state := group.GroupRuntimeSnapshotsReadonly()[995]
	if len(state.Members) != 2 {
		t.Fatalf("standby topology lost: %+v", state)
	}
	for _, member := range state.Members {
		if member.NodeID == a.ID && (!member.Standby || member.Status != "EVICTED") {
			t.Fatalf("evicted standby state lost: %+v", member)
		}
		if member.NodeID == b.ID && member.Standby {
			t.Fatal("standby not promoted")
		}
	}
	if err := mgr.ApplyHealingSelection(ctx, 995, []int64{999999}); err == nil {
		t.Fatal("ineligible selection admitted")
	}
	mgr.mu.Lock()
	mgr.cfg.Groups[0].Execute = false
	mgr.mu.Unlock()
	if err := mgr.ApplyHealingSelection(ctx, 995, []int64{b.ID}); err == nil {
		t.Fatal("dry-run performed mutation")
	}
}
