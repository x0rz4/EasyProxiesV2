package boxmgr

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

// TestApplyGroupMembershipChangesRebuildsOnlyAffectedGroups is the load-bearing
// guarantee of tag-driven membership: a node gaining a tag must not reload the
// base listener or the groups that node did not join, because those listeners are
// serving live connections.
func TestApplyGroupMembershipChangesRebuildsOnlyAffectedGroups(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "membership.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tagged := &store.Node{URI: "http://127.0.0.1:65530", Name: "tagged", Source: store.NodeSourceManual,
		Enabled: true, Region: "hk", Country: "Hong Kong"}
	untagged := &store.Node{URI: "http://127.0.0.2:65530", Name: "untagged", Source: store.NodeSourceManual,
		Enabled: true, Region: "hk", Country: "Hong Kong"}
	for _, node := range []*store.Node{tagged, untagged} {
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	vip := &store.Tag{Name: "vip"}
	if err := db.CreateTag(ctx, vip); err != nil {
		t.Fatal(err)
	}
	if err := db.SetManualNodeTags(ctx, tagged.ID, []int64{vip.ID}); err != nil {
		t.Fatal(err)
	}

	ports := reserveTCPPorts(t, 3)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes: []config.NodeConfig{
			{ID: tagged.ID, Name: tagged.Name, URI: tagged.URI, Region: "hk", Country: "Hong Kong",
				Tags: []string{"vip"}},
			{ID: untagged.ID, Name: untagged.Name, URI: untagged.URI, Region: "hk", Country: "Hong Kong"},
		},
		TagNames: map[int64]string{vip.ID: "vip"},
		Groups: []config.GroupPoolConfig{
			{ID: 21, Name: "vip-only", BindAddress: "127.0.0.1", BindPort: ports[1], Protocol: "mixed",
				DispatchMode: "fixed", Regions: []string{"hk"}, TagWhitelist: []int64{vip.ID},
				TagFilterMatch: store.TagFilterMatchAny, FailureWindow: 5 * time.Minute,
				FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true},
			{ID: 22, Name: "all-hk", BindAddress: "127.0.0.1", BindPort: ports[2], Protocol: "mixed",
				DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute,
				FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true},
		},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{}, WithStore(db))
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	manager.mu.RLock()
	baseBefore := manager.currentBox
	manager.mu.RUnlock()
	vipBefore := manager.groupSlot(21).box
	allBefore := manager.groupSlot(22).box
	if baseBefore == nil || vipBefore == nil || allBefore == nil {
		t.Fatalf("expected base and both group runtimes to be running: base=%p vip=%p all=%p",
			baseBefore, vipBefore, allBefore)
	}

	// The untagged node joins the VIP tag. Only the tag-filtered group's member
	// set moves; the region-only group already contained both nodes.
	if err := db.SetManualNodeTags(ctx, untagged.ID, []int64{vip.ID}); err != nil {
		t.Fatal(err)
	}
	if err := manager.ApplyGroupMembershipChanges(ctx, []int64{untagged.ID}); err != nil {
		t.Fatal(err)
	}

	manager.mu.RLock()
	baseAfter := manager.currentBox
	manager.mu.RUnlock()
	if baseAfter != baseBefore {
		t.Fatal("membership change reloaded the base sing-box instance")
	}
	if manager.groupSlot(22).box != allBefore {
		t.Fatal("group whose member set did not change was rebuilt")
	}
	if manager.groupSlot(21).box == vipBefore {
		t.Fatal("group whose member set changed was not rebuilt")
	}
	if status := manager.GroupRuntimeStatus(21).Status; status != "ready" {
		t.Fatalf("rebuilt group status = %q, want ready", status)
	}
	assertTCPListening(t, ports[1])
	assertTCPListening(t, ports[2])

	// The refreshed tag has to land in the cached config too, or the next full
	// reload would rebuild the group without it.
	manager.mu.RLock()
	cached := manager.cfg.Clone()
	manager.mu.RUnlock()
	vipGroup, found := groupConfigByID(cached, 21)
	if !found {
		t.Fatal("rebuilt group vanished from the cached config")
	}
	members := groupMemberNodes(cached, vipGroup)
	if len(members) != 2 {
		t.Fatalf("cached config still resolves %d VIP members, want 2: %+v", len(members), members)
	}
}

// TestApplyGroupMembershipChangesFollowsLandingRegion covers the other fact that
// moves membership: the landing classification written by a node check. It
// arrives through the same incremental path, so the region has to be re-read from
// the store rather than assumed unchanged.
func TestApplyGroupMembershipChangesFollowsLandingRegion(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "membership-region.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resident := &store.Node{URI: "http://127.0.0.1:65530", Name: "resident", Source: store.NodeSourceManual,
		Enabled: true, Region: "hk", Country: "Hong Kong"}
	mover := &store.Node{URI: "http://127.0.0.2:65530", Name: "mover", Source: store.NodeSourceManual,
		Enabled: true, Region: "hk", Country: "Hong Kong"}
	for _, node := range []*store.Node{resident, mover} {
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes: []config.NodeConfig{
			{ID: resident.ID, Name: resident.Name, URI: resident.URI, Region: "hk", Country: "Hong Kong"},
			{ID: mover.ID, Name: mover.Name, URI: mover.URI, Region: "hk", Country: "Hong Kong"},
		},
		Groups: []config.GroupPoolConfig{{ID: 51, Name: "hk-only", BindAddress: "127.0.0.1", BindPort: ports[1],
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute,
			FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{}, WithStore(db))
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	boxBefore := manager.groupSlot(51).box
	if boxBefore == nil {
		t.Fatal("expected the group runtime to be running")
	}

	if err := db.UpdateNodeLocation(ctx, mover.ID, "us", "United States"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ApplyGroupMembershipChanges(ctx, []int64{mover.ID}); err != nil {
		t.Fatal(err)
	}
	if manager.groupSlot(51).box == boxBefore {
		t.Fatal("group did not rebuild after a member left its region")
	}
	manager.mu.RLock()
	cached := manager.cfg.Clone()
	manager.mu.RUnlock()
	groupCfg, found := groupConfigByID(cached, 51)
	if !found {
		t.Fatal("rebuilt group vanished from the cached config")
	}
	members := groupMemberNodes(cached, groupCfg)
	if len(members) != 1 || members[0].ID != resident.ID {
		t.Fatalf("relocated node is still a member: %+v", members)
	}
}

// TestApplyGroupMembershipChangesIgnoresUnrelatedNodes guards the cheap path: a
// node that no group filters on must cost zero rebuilds.
func TestApplyGroupMembershipChangesIgnoresUnrelatedNodes(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "membership-noop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hk := &store.Node{URI: "http://127.0.0.1:65530", Name: "hk", Source: store.NodeSourceManual,
		Enabled: true, Region: "hk"}
	us := &store.Node{URI: "http://127.0.0.2:65530", Name: "us", Source: store.NodeSourceManual,
		Enabled: true, Region: "us"}
	for _, node := range []*store.Node{hk, us} {
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes: []config.NodeConfig{
			{ID: hk.ID, Name: hk.Name, URI: hk.URI, Region: "hk"},
			{ID: us.ID, Name: us.Name, URI: us.URI, Region: "us"},
		},
		Groups: []config.GroupPoolConfig{{ID: 31, Name: "hk-only", BindAddress: "127.0.0.1", BindPort: ports[1],
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute,
			FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{}, WithStore(db))
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	manager.mu.RLock()
	baseBefore := manager.currentBox
	manager.mu.RUnlock()
	groupBefore := manager.groupSlot(31).box
	if baseBefore == nil || groupBefore == nil {
		t.Fatal("expected base and group runtimes to be running")
	}
	if err := manager.ApplyGroupMembershipChanges(ctx, []int64{us.ID}); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	baseAfter := manager.currentBox
	manager.mu.RUnlock()
	if baseAfter != baseBefore || manager.groupSlot(31).box != groupBefore {
		t.Fatal("a node outside every group's filter still triggered a rebuild")
	}
	// An empty or all-invalid ID list is a no-op rather than an error.
	if err := manager.ApplyGroupMembershipChanges(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.ApplyGroupMembershipChanges(ctx, []int64{0, -3}); err != nil {
		t.Fatal(err)
	}
	if manager.groupSlot(31).box != groupBefore {
		t.Fatal("degenerate ID list triggered a rebuild")
	}
}

// TestForcedRebuildWithRollbackKeepsServingListener contrasts the two forced
// modes. The node-fact path must never trade a working listener for an error
// state, while the post-base-reload path has no valid old box to keep.
func TestForcedRebuildWithRollbackKeepsServingListener(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 41, Name: "group", BindAddress: "127.0.0.1", BindPort: ports[1],
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute,
			FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	boxBefore := manager.groupSlot(41).box
	if boxBefore == nil {
		t.Fatal("expected the group runtime to be running")
	}

	// A member set that resolves to nobody cannot be built, which is exactly the
	// failure a retag can produce when a whitelist stops matching.
	before := storeGroupFromConfig(cfg.Groups[0])
	after := cloneStoreGroupForTest(before)
	after.Regions = []string{"us"}
	if err := manager.applyGroupRuntime(context.Background(), before, after, applyModeForceWithRollback); err == nil {
		t.Fatal("forced rebuild with no members unexpectedly succeeded")
	}
	if manager.groupSlot(41).box != boxBefore {
		t.Fatal("failed rebuild replaced the listener it could not rebuild")
	}
	if status := manager.GroupRuntimeStatus(41).Status; status != "ready" {
		t.Fatalf("rolled-back group status = %q, want ready", status)
	}
	assertTCPListening(t, ports[1])

	// Same failure, no-rollback mode: the group is left stopped and reported as
	// broken, which TestForcedTopologyUpdateDoesNotKeepRemovedNodeRuntime pins.
	if err := manager.applyGroupRuntime(context.Background(), before, after, applyModeForceNoRollback); err == nil {
		t.Fatal("forced rebuild with no members unexpectedly succeeded")
	}
	if manager.groupSlot(41).box != nil {
		t.Fatal("no-rollback mode kept a runtime it reported as failed")
	}
	if status := manager.GroupRuntimeStatus(41).Status; status != "error" {
		t.Fatalf("no-rollback group status = %q, want error", status)
	}
}
