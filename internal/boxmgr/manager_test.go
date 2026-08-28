package boxmgr

import (
	"context"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/runtimetag"
	"easy_proxies/internal/store"
)

func TestRuntimeConfigEqual(t *testing.T) {
	a := &config.Config{Nodes: []config.NodeConfig{{Name: "a", URI: "http://a.example:80"}}}
	b := a.Clone()
	if !runtimeConfigEqual(a, b) {
		t.Fatal("equivalent configurations were considered different")
	}
	b.Nodes[0].URI = "http://b.example:80"
	if runtimeConfigEqual(a, b) {
		t.Fatal("different configurations were considered equal")
	}
	b = a.Clone()
	b.Groups = []config.GroupPoolConfig{{ID: 1, Name: "group", BindPort: 10001}}
	if runtimeConfigEqual(a, b) {
		t.Fatal("group configuration change was ignored")
	}
}

func TestReloadTogglesPoolEntryWithoutAffectingBaseRuntime(t *testing.T) {
	port := reserveTCPPorts(t, 1)[0]
	cfg := &config.Config{
		Listener:            config.ListenerConfig{Address: "127.0.0.1", Port: port, Protocol: "http"},
		Pool:                config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:               []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530"}},
		SubscriptionRefresh: config.SubscriptionRefreshConfig{HealthCheckTimeout: time.Millisecond},
		LogLevel:            "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	assertTCPNotListening(t, port)
	snapshots := manager.monitorMgr.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Mode != "disabled" {
		t.Fatalf("disabled entries did not retain node monitoring: %+v", snapshots)
	}

	enabled := cfg.Clone()
	enabled.Listener.Enabled = true
	if err := manager.ReloadWithPortMap(enabled, manager.CurrentPortMap()); err != nil {
		t.Fatal(err)
	}
	assertTCPListening(t, port)

	disabled := enabled.Clone()
	disabled.Listener.Enabled = false
	if err := manager.ReloadWithPortMap(disabled, manager.CurrentPortMap()); err != nil {
		t.Fatal(err)
	}
	assertTCPNotListening(t, port)
}

func TestStartRegistersVersionedRuntimeTagOnFirstBox(t *testing.T) {
	cfg := &config.Config{
		Pool:                config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:               []config.NodeConfig{{ID: 1, Name: "display name", URI: "http://127.0.0.1:65530", Region: "hk"}},
		SubscriptionRefresh: config.SubscriptionRefreshConfig{HealthCheckTimeout: time.Millisecond},
		LogLevel:            "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	wantTag, err := runtimetag.Format(cfg.Nodes[0].NodeKey(), runtimetag.InitialVersion)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	snapshots := manager.monitorMgr.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Tag != wantTag {
		t.Fatalf("cold-start monitor snapshots=%+v, want tag %q", snapshots, wantTag)
	}
}

func TestNodeOnlyReloadCreatesPublishesAndDrainsInPlace(t *testing.T) {
	cfg := &config.Config{
		Pool:                config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:               []config.NodeConfig{{ID: 41, Name: "before", URI: "http://127.0.0.1:65530", Region: "hk"}},
		SubscriptionRefresh: config.SubscriptionRefreshConfig{HealthCheckTimeout: time.Millisecond, DrainTimeout: 100 * time.Millisecond},
		LogLevel:            "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.mu.RLock()
	instance := manager.currentBox
	manager.mu.RUnlock()
	oldTag, err := runtimetag.Format(cfg.Nodes[0].NodeKey(), 1)
	if err != nil {
		t.Fatal(err)
	}
	poolOutbound, found := instance.Outbound().Outbound("proxy-pool")
	if !found || len(poolOutbound.Dependencies()) != 0 {
		t.Fatalf("pool dependencies = %v, want no dynamic node dependency", poolOutbound.Dependencies())
	}

	updated := cfg.Clone()
	updated.Nodes[0].Name = "after"
	updated.Nodes[0].URI = "http://127.0.0.1:65531"
	if err := manager.Reload(updated); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	if manager.currentBox != instance {
		manager.mu.RUnlock()
		t.Fatal("node-only reload replaced the sing-box instance")
	}
	generation := manager.runtimeGeneration
	manager.mu.RUnlock()
	if generation != 2 {
		t.Fatalf("runtime generation = %d, want 2", generation)
	}
	newTag, err := runtimetag.Format(updated.Nodes[0].NodeKey(), 2)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := manager.monitorMgr.Snapshot()
	if len(snapshots) != 1 || snapshots[0].Tag != newTag || snapshots[0].Name != "after" {
		t.Fatalf("monitor migration = %+v, want only %s", snapshots, newTag)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := instance.Outbound().Outbound(oldTag); !found {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("retired outbound %s was not removed after drain", oldTag)
}

func TestReassignConflictingPortRequiresMultiPort(t *testing.T) {
	cfg := &config.Config{Nodes: []config.NodeConfig{{Name: "retained", Port: 2323}}}
	if reassignConflictingPort(cfg, 2323) {
		t.Fatal("disabled multi-port reassigned a retained node port")
	}
	if cfg.Nodes[0].Port != 2323 {
		t.Fatalf("disabled multi-port changed retained port to %d", cfg.Nodes[0].Port)
	}
}

func TestApplyGroupRuntimeDoesNotReplaceBaseOrSiblingRuntime(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ports := reserveTCPPorts(t, 3)
	basePort, firstPort, secondPort := ports[0], ports[1], ports[2]
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: basePort, Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530", Region: "hk"}},
		Groups: []config.GroupPoolConfig{
			{ID: 1, Name: "one", BindAddress: "127.0.0.1", BindPort: firstPort, Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute, FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true},
			{ID: 2, Name: "two", BindAddress: "127.0.0.1", BindPort: secondPort, Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute, FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true},
		},
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

	manager.mu.RLock()
	baseBefore := manager.currentBox
	manager.mu.RUnlock()
	siblingBefore := manager.groupSlot(2).box
	if baseBefore == nil || siblingBefore == nil {
		t.Fatal("expected base and sibling runtimes to be running")
	}
	assertTCPListening(t, secondPort)

	before := storeGroupFromConfig(cfg.Groups[0])
	after := cloneStoreGroupForTest(before)
	after.Enabled = false
	if err := manager.ApplyGroupRuntime(context.Background(), before, after); err != nil {
		t.Fatal(err)
	}

	manager.mu.RLock()
	baseAfter := manager.currentBox
	manager.mu.RUnlock()
	if baseAfter != baseBefore {
		t.Fatal("targeted group update replaced the base sing-box instance")
	}
	if manager.groupSlot(2).box != siblingBefore {
		t.Fatal("targeted group update replaced a sibling group instance")
	}
	assertTCPListening(t, secondPort)
	if status := manager.GroupRuntimeStatus(1).Status; status != "stopped" {
		t.Fatalf("target group status = %q, want stopped", status)
	}
}

func TestApplyGroupPolicyUpdateKeepsGroupBoxAndListener(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 3, Name: "policy", BindAddress: "127.0.0.1", BindPort: ports[1], Protocol: "mixed",
			DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute, FailureThreshold: 3,
			HealthCheckInterval: time.Minute, Enabled: true}}, LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	boxBefore := manager.groupSlot(3).box
	before := storeGroupFromConfig(cfg.Groups[0])
	after := cloneStoreGroupForTest(before)
	after.DispatchMode = "random"
	after.FailureWindowSeconds = 60
	after.FailureThreshold = 2
	after.HealthCheckSeconds = 15
	if err := manager.ApplyGroupRuntime(context.Background(), before, after); err != nil {
		t.Fatal(err)
	}
	if manager.groupSlot(3).box != boxBefore {
		t.Fatal("non-listener policy update replaced the group box")
	}
	assertTCPListening(t, ports[1])
}

func TestActivateGroupMemberWaitsForSameGroupRebuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		group.Reset()
		defer group.Reset()
		manager := New(&config.Config{}, monitor.Config{})
		slot := manager.groupSlot(91)
		if err := acquireGroupSlot(t.Context(), slot); err != nil {
			t.Fatal(err)
		}
		manager.setGroupRuntimeStatus(91, "reconfiguring", "")
		activated := make(chan int64, 1)
		unregister := group.RegisterActivationHandler(91, func(nodeID int64) error {
			activated <- nodeID
			return nil
		})
		defer unregister()

		result := make(chan error, 1)
		go func() { result <- manager.ActivateGroupMember(t.Context(), 91, 7) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("activation returned before rebuild completed: %v", err)
		default:
		}
		manager.setGroupRuntimeStatus(91, "ready", "")
		releaseGroupSlot(slot)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if nodeID := <-activated; nodeID != 7 {
			t.Fatalf("activated node = %d, want 7", nodeID)
		}
	})
}

func TestGroupRuntimeTopologyOnlyChangesForAffectedMembers(t *testing.T) {
	base := &config.Config{Nodes: []config.NodeConfig{
		{ID: 1, Name: "hk", URI: "http://hk.example:80", Region: "hk"},
		{ID: 2, Name: "us", URI: "http://us.example:80", Region: "us"},
	}, Groups: []config.GroupPoolConfig{
		{ID: 1, Enabled: true, Regions: []string{"hk"}, BindPort: 10001, Protocol: "mixed", DispatchMode: "fixed"},
		{ID: 2, Enabled: true, Regions: []string{"us"}, BindPort: 10002, Protocol: "mixed", DispatchMode: "fixed"},
	}}
	updated := base.Clone()
	updated.Nodes[0].URI = "http://new-hk.example:80"
	if groupRuntimeTopologyEqual(base, updated, 1) {
		t.Fatal("group using the changed HK node was considered unaffected")
	}
	if !groupRuntimeTopologyEqual(base, updated, 2) {
		t.Fatal("unrelated US group was considered affected by an HK node change")
	}
}

func TestGroupConfigsFromStorePreservesDetachedMembership(t *testing.T) {
	stored := []store.GroupPool{{
		ID: 7, Name: "persisted", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{11, 12},
		ExcludedNodeIDs: []int64{13}, Enabled: true,
		NodeStates: []store.GroupNodeState{{NodeID: 11, FailureHistory: []int64{1, 2}}},
	}}
	converted := GroupConfigsFromStore(stored)
	if len(converted) != 1 || len(converted[0].ExplicitNodeIDs) != 2 ||
		len(converted[0].ExcludedNodeIDs) != 1 || converted[0].ExcludedNodeIDs[0] != 13 {
		t.Fatalf("group membership was not converted: %+v", converted)
	}

	converted[0].Regions[0] = "us"
	converted[0].ExplicitNodeIDs[0] = 99
	converted[0].ExcludedNodeIDs[0] = 98
	converted[0].NodeStates[0].FailureHistory[0] = 9
	if stored[0].Regions[0] != "hk" || stored[0].ExplicitNodeIDs[0] != 11 ||
		stored[0].ExcludedNodeIDs[0] != 13 || stored[0].NodeStates[0].FailureHistory[0] != 1 {
		t.Fatalf("runtime conversion aliases persisted membership: stored=%+v", stored[0])
	}
}

func TestForcedTopologyUpdateDoesNotKeepRemovedNodeRuntime(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 11, Name: "group", BindAddress: "127.0.0.1", BindPort: ports[1],
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: 5 * time.Minute,
			FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true}}, LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	before := storeGroupFromConfig(cfg.Groups[0])
	after := cloneStoreGroupForTest(before)
	after.Regions = []string{"us"}
	if err := manager.applyGroupRuntime(context.Background(), before, after, applyModeForceNoRollback); err == nil {
		t.Fatal("forced topology update with no members unexpectedly succeeded")
	}
	slot := manager.groupSlot(11)
	if slot.box == nil || manager.GroupRuntimeStatus(11).Status != "degraded" {
		t.Fatalf("empty update did not retain degraded runtime: box=%p status=%+v", slot.box, manager.GroupRuntimeStatus(11))
	}
}

func reserveTCPPorts(t *testing.T, count int) []uint16 {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]uint16, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, uint16(listener.Addr().(*net.TCPAddr).Port))
	}
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return ports
}

func assertTCPListening(t *testing.T, port uint16) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), time.Second)
	if err != nil {
		t.Fatalf("port %d is not listening: %v", port, err)
	}
	_ = connection.Close()
}

func assertTCPNotListening(t *testing.T, port uint16) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("port %d is unexpectedly listening", port)
	}
}

func cloneStoreGroupForTest(value *store.GroupPool) *store.GroupPool {
	copyValue := *value
	copyValue.Regions = append([]string(nil), value.Regions...)
	copyValue.ExplicitNodeIDs = append([]int64(nil), value.ExplicitNodeIDs...)
	copyValue.ExcludedNodeIDs = append([]int64(nil), value.ExcludedNodeIDs...)
	return &copyValue
}
