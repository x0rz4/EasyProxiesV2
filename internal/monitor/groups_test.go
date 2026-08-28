package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
)

func TestValidateGroupPortIgnoresDisabledBaseEntries(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-disabled-entries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listener: config.ListenerConfig{Port: port},
		Nodes:    []config.NodeConfig{{Name: "retained", Port: port}},
	}
	server := &Server{store: db, cfgSrc: cfg}
	if err := server.validateGroupPort(ctx, "127.0.0.1", port, nil); err != nil {
		t.Fatalf("disabled base entries blocked group port: %v", err)
	}
	cfg.Listener.Enabled = true
	if err := server.validateGroupPort(ctx, "127.0.0.1", port, nil); err == nil {
		t.Fatal("enabled Pool entry did not block its port")
	}
	cfg.Listener.Enabled = false
	cfg.MultiPort.Enabled = true
	if err := server.validateGroupPort(ctx, "127.0.0.1", port, nil); err == nil {
		t.Fatal("enabled multi-port entry did not block its node port")
	}
}

func TestGroupNodeOptionsUseManagedAvailableNodes(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-options.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	available := createGroupTestNode(t, ctx, db, "available", store.NodeSourceManual, true)
	unavailable := createGroupTestNode(t, ctx, db, "unavailable", store.NodeSourceManual, true)
	pending := createGroupTestNode(t, ctx, db, "pending", store.NodeSourceManual, true)
	disabled := createGroupTestNode(t, ctx, db, "disabled", store.NodeSourceManual, false)
	hiddenSubscription := createGroupTestNode(t, ctx, db, "hidden-subscription", store.NodeSourceSubscription, true)

	availableHandle := mgr.Register(NodeInfo{Tag: "available-tag", Name: available.Name, URI: available.URI, Region: "hk"})
	availableHandle.RecordSuccessWithLatency(86 * time.Millisecond)
	unavailableHandle := mgr.Register(NodeInfo{Tag: "unavailable-tag", Name: unavailable.Name, URI: unavailable.URI})
	unavailableHandle.MarkInitialCheckDone(false)
	mgr.Register(NodeInfo{Tag: "pending-tag", Name: pending.Name, URI: pending.URI})
	disabledHandle := mgr.Register(NodeInfo{Tag: "disabled-tag", Name: disabled.Name, URI: disabled.URI})
	disabledHandle.MarkInitialCheckDone(true)
	hiddenHandle := mgr.Register(NodeInfo{Tag: "hidden-tag", Name: hiddenSubscription.Name, URI: hiddenSubscription.URI})
	hiddenHandle.MarkInitialCheckDone(true)

	server := &Server{store: db, mgr: mgr, nodeMgr: &reloadNodeManager{}}
	monitorByTag := make(map[string]Snapshot)
	for _, snapshot := range mgr.Snapshot() {
		monitorByTag[snapshot.Tag] = snapshot
	}
	options, err := server.groupNodeOptions(ctx, []store.GroupPool{{ExplicitNodeIDs: []int64{unavailable.ID}, ExcludedNodeIDs: []int64{pending.ID}}}, monitorByTag)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[int64]groupNodeOptionResponse, len(options))
	for _, option := range options {
		byID[option.ID] = option
	}
	if option, ok := byID[available.ID]; !ok || !option.Selectable || option.Status != "normal" || option.LatencyMs != 86 {
		t.Fatalf("available option = %+v, present=%v", option, ok)
	}
	if option, ok := byID[unavailable.ID]; !ok || option.Selectable || option.Status != "unavailable" {
		t.Fatalf("referenced unavailable option = %+v, present=%v", option, ok)
	}
	if option, ok := byID[pending.ID]; !ok || option.Selectable || option.Status != "pending" {
		t.Fatalf("excluded pending option = %+v, present=%v", option, ok)
	}
	for _, node := range []*store.Node{disabled, hiddenSubscription} {
		if _, ok := byID[node.ID]; ok {
			t.Fatalf("non-selectable unreferenced node %q leaked into options", node.Name)
		}
	}
}

func TestGroupFromInputDropsUnavailableExplicitNodes(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	available := createGroupTestNode(t, ctx, db, "available", store.NodeSourceManual, true)
	unavailable := createGroupTestNode(t, ctx, db, "unavailable", store.NodeSourceManual, true)
	mgr.Register(NodeInfo{Tag: "available-tag", Name: available.Name, URI: available.URI}).MarkInitialCheckDone(true)
	mgr.Register(NodeInfo{Tag: "unavailable-tag", Name: unavailable.Name, URI: unavailable.URI}).MarkInitialCheckDone(false)

	server := &Server{store: db, mgr: mgr}
	groupPool, removed, err := server.groupFromInput(ctx, groupPoolInput{Name: "HK", BindAddress: "127.0.0.1",
		BindPort: 12091, Protocol: "mixed", DispatchMode: "lowest_latency", Regions: []string{"HK"},
		ExplicitNodeIDs: []int64{available.ID, unavailable.ID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groupPool.ExplicitNodeIDs) != 1 || groupPool.ExplicitNodeIDs[0] != available.ID {
		t.Fatalf("kept explicit nodes = %v", groupPool.ExplicitNodeIDs)
	}
	if len(removed) != 1 || removed[0] != unavailable.ID {
		t.Fatalf("removed nodes = %v", removed)
	}
	if len(groupPool.Regions) != 1 || groupPool.Regions[0] != "hk" {
		t.Fatalf("regions changed while filtering explicit nodes: %v", groupPool.Regions)
	}
	if groupPool.DispatchMode != "lowest_latency" {
		t.Fatalf("dispatch mode = %q, want lowest_latency", groupPool.DispatchMode)
	}
	fallback, _, err := server.groupFromInput(ctx, groupPoolInput{Name: "fallback", BindAddress: "127.0.0.1",
		BindPort: 12090, Protocol: "mixed", DispatchMode: "unsupported"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.DispatchMode != "fixed" {
		t.Fatalf("unknown dispatch mode = %q, want fixed fallback", fallback.DispatchMode)
	}
}

func TestUpdateGroupAPIReportsRemovedUnavailableNodes(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-update-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	available := createGroupTestNode(t, ctx, db, "available", store.NodeSourceManual, true)
	unavailable := createGroupTestNode(t, ctx, db, "unavailable", store.NodeSourceManual, true)
	mgr.Register(NodeInfo{Tag: "available-tag", Name: available.Name, URI: available.URI}).MarkInitialCheckDone(true)
	mgr.Register(NodeInfo{Tag: "unavailable-tag", Name: unavailable.Name, URI: unavailable.URI}).MarkInitialCheckDone(false)
	groupPool := &store.GroupPool{Name: "group", BindAddress: "127.0.0.1", BindPort: 12092, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60,
		Enabled: true, SubscriptionEnabled: true, SubscriptionToken: "token", SubscriptionMode: "entry"}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(groupPoolInput{Name: "group", BindAddress: "127.0.0.1", BindPort: 12092,
		Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"},
		ExplicitNodeIDs: []int64{available.ID, unavailable.ID}, FailureWindowSeconds: 300,
		FailureThreshold: 3, HealthCheckSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, mgr: mgr, nodeMgr: &reloadNodeManager{}}
	request := httptest.NewRequest(http.MethodPut, "/api/groups/"+strconv.FormatInt(groupPool.ID, 10), bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Removed []int64 `json:"removed_unavailable_node_ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Removed) != 1 || payload.Removed[0] != unavailable.ID {
		t.Fatalf("removed node IDs = %v", payload.Removed)
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil || len(updated.ExplicitNodeIDs) != 1 || updated.ExplicitNodeIDs[0] != available.ID {
		t.Fatalf("stored group = %+v err=%v", updated, err)
	}
}

func TestUpdateGroupPersistsAutoExcludedEvictedNode(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-auto-exclude-evicted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := createGroupTestNode(t, ctx, db, "evicted", store.NodeSourceManual, true)
	groupPool := &store.GroupPool{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12093, Protocol: "mixed",
		DispatchMode: "fixed", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{node.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertGroupNodeState(ctx, &store.GroupNodeState{GroupID: groupPool.ID, NodeID: node.ID,
		FailureHistory: []int64{1, 2, 3}, Evicted: true, LastError: "authentication required"}); err != nil {
		t.Fatal(err)
	}
	excludedNodeIDs := []int64{node.ID}
	body, err := json.Marshal(groupPoolInput{Name: "HK", BindAddress: "127.0.0.1", BindPort: groupPool.BindPort,
		Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, ExcludedNodeIDs: &excludedNodeIDs,
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	runtimeManager := &isolatedGroupRuntimeManager{}
	server := &Server{store: db, nodeMgr: runtimeManager}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10), bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ExplicitNodeIDs) != 0 || len(updated.ExcludedNodeIDs) != 1 || updated.ExcludedNodeIDs[0] != node.ID {
		t.Fatalf("stored membership rules = explicit %v excluded %v", updated.ExplicitNodeIDs, updated.ExcludedNodeIDs)
	}
	if len(updated.NodeStates) != 1 || !updated.NodeStates[0].Evicted || updated.NodeStates[0].NodeID != node.ID {
		t.Fatalf("evicted state was unexpectedly cleared: %+v", updated.NodeStates)
	}
	if runtimeManager.applies.Load() != 1 {
		t.Fatalf("runtime applies=%d, want 1", runtimeManager.applies.Load())
	}
}

func TestProbeFailureReturnsErrorStatus(t *testing.T) {
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	handle := mgr.Register(NodeInfo{Tag: "broken", Name: "broken"})
	handle.SetProbe(func(context.Context) (time.Duration, error) { return 0, errors.New("dial failed") })
	server := &Server{mgr: mgr}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/nodes/broken/probe", nil))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload["error"] != "dial failed" {
		t.Fatalf("payload=%v err=%v", payload, err)
	}
}

func TestConcurrentGroupAutoPortAllocationIsUnique(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-concurrent-port.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	server := &Server{store: db, mgr: mgr, nodeMgr: &reloadNodeManager{}}

	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, name := range []string{"group-a", "group-b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			body, _ := json.Marshal(groupPoolInput{Name: name, BindAddress: "127.0.0.1", BindPort: 0,
				Protocol: "mixed", DispatchMode: "fixed", FailureWindowSeconds: 300,
				FailureThreshold: 3, HealthCheckSeconds: 60})
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/groups", bytes.NewReader(body)))
			responses <- response
		}(name)
	}
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	groups, err := db.ListGroupPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].BindPort == groups[1].BindPort {
		t.Fatalf("allocated ports = %+v", groups)
	}
}

func TestRemoveRunningMemberPersistsGroupExclusion(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-remove-member.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := createGroupTestNode(t, ctx, db, "hk-node", store.NodeSourceManual, true)
	groupPool := &store.GroupPool{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12096, Protocol: "mixed",
		DispatchMode: "fixed", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{node.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60,
		CurrentActiveNodeID: node.ID, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	group.Register(groupPool.ID, 5*time.Minute, 3, "node-tag", map[string]group.GroupInitialState{"node-tag": {NodeID: node.ID}})
	nodeManager := &reloadNodeManager{}
	server := &Server{store: db, nodeMgr: nodeManager}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/members/"+strconv.FormatInt(node.ID, 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ExplicitNodeIDs) != 0 || len(updated.ExcludedNodeIDs) != 1 || updated.ExcludedNodeIDs[0] != node.ID {
		t.Fatalf("updated membership rules = explicit %v excluded %v", updated.ExplicitNodeIDs, updated.ExcludedNodeIDs)
	}
	if updated.CurrentActiveNodeID != 0 || nodeManager.reloads.Load() != 1 {
		t.Fatalf("current=%d reloads=%d", updated.CurrentActiveNodeID, nodeManager.reloads.Load())
	}
}

func TestUnexcludeGroupMemberRebuildsRuntime(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-unexclude.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := createGroupTestNode(t, ctx, db, "excluded-node", store.NodeSourceManual, true)
	groupPool := &store.GroupPool{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12098, Protocol: "mixed",
		DispatchMode: "fixed", Regions: []string{"hk"}, ExcludedNodeIDs: []int64{node.ID},
		FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeManager := &isolatedGroupRuntimeManager{}
	server := &Server{store: db, nodeMgr: runtimeManager}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/exclusions/"+strconv.FormatInt(node.ID, 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ExcludedNodeIDs) != 0 || runtimeManager.applies.Load() != 1 {
		t.Fatalf("excluded=%v applies=%d", updated.ExcludedNodeIDs, runtimeManager.applies.Load())
	}
}

func TestUnexcludeGroupMemberRuntimeFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-unexclude-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := createGroupTestNode(t, ctx, db, "excluded-node", store.NodeSourceManual, true)
	groupPool := &store.GroupPool{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12099, Protocol: "mixed",
		DispatchMode: "fixed", ExcludedNodeIDs: []int64{node.ID}, FailureWindowSeconds: 300,
		FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeManager := &isolatedGroupRuntimeManager{applyErr: errors.New("group start failed")}
	server := &Server{store: db, nodeMgr: runtimeManager}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/exclusions/"+strconv.FormatInt(node.ID, 10), nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ExcludedNodeIDs) != 1 || updated.ExcludedNodeIDs[0] != node.ID {
		t.Fatalf("rollback lost exclusion: %v", updated.ExcludedNodeIDs)
	}
}

type serialGroupRuntimeManager struct {
	reloadNodeManager
	active atomic.Int32
	max    atomic.Int32
}

func (m *serialGroupRuntimeManager) ApplyGroupRuntime(context.Context, *store.GroupPool, *store.GroupPool) error {
	active := m.active.Add(1)
	for {
		current := m.max.Load()
		if active <= current || m.max.CompareAndSwap(current, active) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	m.active.Add(-1)
	return nil
}

func (m *serialGroupRuntimeManager) ActivateGroupMember(context.Context, int64, int64) error {
	return nil
}

func (m *serialGroupRuntimeManager) GroupRuntimeStatus(int64) GroupRuntimeStatus {
	return GroupRuntimeStatus{Status: "ready"}
}

func TestConcurrentGroupExclusionMutationsAreSerialized(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-exclusion-concurrency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := createGroupTestNode(t, ctx, db, "first-excluded", store.NodeSourceManual, true)
	second := createGroupTestNode(t, ctx, db, "second-excluded", store.NodeSourceManual, true)
	groupPool := &store.GroupPool{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12100, Protocol: "mixed",
		DispatchMode: "fixed", ExcludedNodeIDs: []int64{first.ID, second.ID}, FailureWindowSeconds: 300,
		FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeManager := &serialGroupRuntimeManager{}
	server := &Server{store: db, nodeMgr: runtimeManager}
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for _, nodeID := range []int64{first.ID, second.ID} {
		wg.Add(1)
		go func(nodeID int64) {
			defer wg.Done()
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete,
				"/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/exclusions/"+strconv.FormatInt(nodeID, 10), nil))
			responses <- response
		}(nodeID)
	}
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	updated, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.ExcludedNodeIDs) != 0 || runtimeManager.max.Load() != 1 {
		t.Fatalf("excluded=%v max concurrent applies=%d", updated.ExcludedNodeIDs, runtimeManager.max.Load())
	}
}

func TestActivateRunningMemberUsesRuntimeHandler(t *testing.T) {
	group.Reset()
	defer group.Reset()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-activate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	groupPool := &store.GroupPool{Name: "group", BindAddress: "127.0.0.1", BindPort: 12097, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(context.Background(), groupPool); err != nil {
		t.Fatal(err)
	}
	var activated int64
	unregister := group.RegisterActivationHandler(groupPool.ID, func(nodeID int64) error { activated = nodeID; return nil })
	defer unregister()
	server := &Server{store: db}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/members/9/activate", nil))
	if response.Code != http.StatusOK || activated != 9 {
		t.Fatalf("status=%d activated=%d body=%s", response.Code, activated, response.Body.String())
	}
}

type isolatedGroupRuntimeManager struct {
	reloadNodeManager
	applyErr error
	applies  atomic.Int32
}

func (m *isolatedGroupRuntimeManager) ApplyGroupRuntime(context.Context, *store.GroupPool, *store.GroupPool) error {
	m.applies.Add(1)
	return m.applyErr
}

func (m *isolatedGroupRuntimeManager) ActivateGroupMember(context.Context, int64, int64) error {
	return nil
}

func (m *isolatedGroupRuntimeManager) GroupRuntimeStatus(int64) GroupRuntimeStatus {
	return GroupRuntimeStatus{Status: "ready"}
}

func TestUpdateGroupUsesIsolatedRuntimeWithoutGlobalReload(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-isolated-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	groupPool := &store.GroupPool{Name: "before", BindAddress: "127.0.0.1", BindPort: 12101, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeManager := &isolatedGroupRuntimeManager{}
	server := &Server{store: db, nodeMgr: runtimeManager}
	body, _ := json.Marshal(groupPoolInput{Name: "after", BindAddress: "127.0.0.1", BindPort: groupPool.BindPort,
		Protocol: "mixed", DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10), bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if runtimeManager.applies.Load() != 1 || runtimeManager.reloads.Load() != 0 {
		t.Fatalf("target applies=%d global reloads=%d", runtimeManager.applies.Load(), runtimeManager.reloads.Load())
	}
}

func TestUpdateGroupRuntimeFailureRollsBackDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "group-update-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	groupPool := &store.GroupPool{Name: "before", BindAddress: "127.0.0.1", BindPort: 12102, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60, Enabled: true}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	runtimeManager := &isolatedGroupRuntimeManager{applyErr: errors.New("group start failed")}
	server := &Server{store: db, nodeMgr: runtimeManager}
	body, _ := json.Marshal(groupPoolInput{Name: "after", BindAddress: "127.0.0.1", BindPort: groupPool.BindPort,
		Protocol: "mixed", DispatchMode: "random", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10), bytes.NewReader(body)))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		RolledBack bool `json:"rolled_back"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || !payload.RolledBack {
		t.Fatalf("rollback response=%+v err=%v", payload, err)
	}
	stored, err := db.GetGroupPool(ctx, groupPool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "before" || stored.DispatchMode != "fixed" {
		t.Fatalf("database was not rolled back: %+v", stored)
	}
	if runtimeManager.reloads.Load() != 0 {
		t.Fatalf("rollback triggered %d global reloads", runtimeManager.reloads.Load())
	}
}

func createGroupTestNode(t *testing.T, ctx context.Context, db store.Store, name, source string, enabled bool) *store.Node {
	t.Helper()
	node := &store.Node{URI: "http://" + name + ".example:8080", Name: name, Source: source, Enabled: enabled}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestGroupFromInputNormalizesTagFilter(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-tag-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vip := &store.Tag{Name: "vip"}
	slow := &store.Tag{Name: "slow"}
	for _, tag := range []*store.Tag{vip, slow} {
		if err := db.CreateTag(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: db}
	base := func() groupPoolInput {
		return groupPoolInput{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12190, Protocol: "mixed",
			DispatchMode: "fixed", Regions: []string{"hk"}}
	}

	input := base()
	// Duplicates and non-positive IDs are noise from the UI; order is what the
	// operator sees back, so it has to survive.
	input.TagWhitelist = &[]int64{slow.ID, vip.ID, slow.ID, 0, -7}
	input.TagFilterMatch = " ALL "
	groupPool, _, err := server.groupFromInput(ctx, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groupPool.TagWhitelist) != 2 || groupPool.TagWhitelist[0] != slow.ID || groupPool.TagWhitelist[1] != vip.ID {
		t.Fatalf("whitelist = %v, want [%d %d]", groupPool.TagWhitelist, slow.ID, vip.ID)
	}
	if groupPool.TagFilterMatch != store.TagFilterMatchAll {
		t.Fatalf("match mode = %q, want all", groupPool.TagFilterMatch)
	}

	// No tag filter at all still has to produce a usable default match mode.
	plain, _, err := server.groupFromInput(ctx, base(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.TagWhitelist) != 0 || len(plain.TagBlacklist) != 0 ||
		plain.TagFilterMatch != store.TagFilterMatchAny {
		t.Fatalf("unfiltered group = %+v", plain)
	}

	// An omitted pointer keeps the stored filter; the existing match mode too.
	existing := &store.GroupPool{ID: 5, TagWhitelist: []int64{vip.ID}, TagBlacklist: []int64{slow.ID},
		TagFilterMatch: store.TagFilterMatchAll}
	kept, _, err := server.groupFromInput(ctx, base(), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.TagWhitelist) != 1 || kept.TagWhitelist[0] != vip.ID ||
		len(kept.TagBlacklist) != 1 || kept.TagBlacklist[0] != slow.ID ||
		kept.TagFilterMatch != store.TagFilterMatchAll {
		t.Fatalf("partial update dropped the stored tag filter: %+v", kept)
	}
	// An explicit empty list is a clear, not an omission.
	cleared := base()
	cleared.TagWhitelist = &[]int64{}
	clearedPool, _, err := server.groupFromInput(ctx, cleared, existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(clearedPool.TagWhitelist) != 0 || len(clearedPool.TagBlacklist) != 1 {
		t.Fatalf("explicit empty whitelist was not applied: %+v", clearedPool)
	}
}

func TestGroupFromInputRejectsUnusableTagFilter(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "groups-tag-reject.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vip := &store.Tag{Name: "vip"}
	if err := db.CreateTag(ctx, vip); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db}
	base := func() groupPoolInput {
		return groupPoolInput{Name: "HK", BindAddress: "127.0.0.1", BindPort: 12191, Protocol: "mixed",
			DispatchMode: "fixed", Regions: []string{"hk"}}
	}

	both := base()
	both.TagWhitelist = &[]int64{vip.ID}
	both.TagBlacklist = &[]int64{vip.ID}
	if _, _, err := server.groupFromInput(ctx, both, nil); err == nil {
		t.Fatal("a tag on both lists was accepted")
	} else if !strings.Contains(err.Error(), "白名单与黑名单") {
		t.Fatalf("unexpected intersection error: %v", err)
	}

	missing := base()
	missing.TagWhitelist = &[]int64{vip.ID + 999}
	if _, _, err := server.groupFromInput(ctx, missing, nil); err == nil {
		t.Fatal("a nonexistent tag ID was accepted")
	} else if !strings.Contains(err.Error(), "标签不存在") {
		t.Fatalf("unexpected missing-tag error: %v", err)
	}

	badMatch := base()
	badMatch.TagFilterMatch = "either"
	if _, _, err := server.groupFromInput(ctx, badMatch, nil); err == nil {
		t.Fatal("an invalid match mode was accepted")
	} else if !strings.Contains(err.Error(), "标签匹配方式") {
		t.Fatalf("unexpected match-mode error: %v", err)
	}
}

func TestCloneGroupPoolDetachesTagFilter(t *testing.T) {
	original := &store.GroupPool{ID: 7, Regions: []string{"hk"}, TagWhitelist: []int64{1, 2},
		TagBlacklist: []int64{3}, TagFilterMatch: store.TagFilterMatchAll}
	cloned := cloneGroupPool(original)
	cloned.TagWhitelist[0] = 99
	cloned.TagBlacklist[0] = 98
	if original.TagWhitelist[0] != 1 || original.TagBlacklist[0] != 3 {
		t.Fatalf("clone aliases the tag filter: %+v", original)
	}
	if cloned.TagFilterMatch != store.TagFilterMatchAll {
		t.Fatalf("clone lost the match mode: %+v", cloned)
	}
}
