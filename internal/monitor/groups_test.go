package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
)

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

	server := &Server{store: db, mgr: mgr}
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
	server := &Server{store: db, mgr: mgr}
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
	var activated int64
	unregister := group.RegisterActivationHandler(77, func(nodeID int64) error { activated = nodeID; return nil })
	defer unregister()
	server := &Server{store: db}
	response := httptest.NewRecorder()
	server.handleGroupItem(response, httptest.NewRequest(http.MethodPost, "/api/groups/77/members/9/activate", nil))
	if response.Code != http.StatusOK || activated != 9 {
		t.Fatalf("status=%d activated=%d body=%s", response.Code, activated, response.Body.String())
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
