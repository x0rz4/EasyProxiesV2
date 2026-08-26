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
	options, err := server.groupNodeOptions(ctx, []store.GroupPool{{ExplicitNodeIDs: []int64{unavailable.ID}}}, monitorByTag)
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
	for _, node := range []*store.Node{pending, disabled, hiddenSubscription} {
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
	server.handleGroupItem(response, request)
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
	server.handleNodeAction(response, httptest.NewRequest(http.MethodPost, "/api/nodes/broken/probe", nil))
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
			server.handleGroups(response, httptest.NewRequest(http.MethodPost, "/api/groups", bytes.NewReader(body)))
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
	server.handleGroupItem(response, httptest.NewRequest(http.MethodDelete,
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
	server.handleGroupItem(response, httptest.NewRequest(http.MethodPost, "/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/members/9/activate", nil))
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
	server.handleGroupItem(response, httptest.NewRequest(http.MethodPut,
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
	server.handleGroupItem(response, httptest.NewRequest(http.MethodPut,
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
