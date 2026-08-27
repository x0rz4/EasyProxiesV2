package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodetag"
	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"
)

func newTagAPITestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	dataStore, err := store.Open(filepath.Join(t.TempDir(), "tags-api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	service := nodetag.NewService(dataStore,
		nodetag.WithRegistry(NewTagFactRegistry()),
		nodetag.WithUnlockProviders(TagUnlockFactProviders()))
	return &Server{store: dataStore, tagSvc: service}, dataStore
}

func serveTagAPI(server *Server, method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if path == "/api/tags" {
		server.handleTags(response, request)
	} else {
		server.handleTagItem(response, request)
	}
	return response
}

func TestTagRouteDispatchesLiteralResourcesBeforeIDs(t *testing.T) {
	server, _ := newTagAPITestServer(t)
	tests := []struct {
		method, path, body, want string
		status                   int
	}{
		{http.MethodGet, "/api/tags/schema", "", `"version"`, http.StatusOK},
		{http.MethodGet, "/api/tags/mutex-groups/7", "", "互斥组不存在", http.StatusNotFound},
		{http.MethodPut, "/api/tags/nodes/12", `{"tag_ids":[]}`, "节点不存在", http.StatusBadRequest},
		{http.MethodGet, "/api/tags/12", "", "标签不存在", http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			response := serveTagAPI(server, test.method, test.path, test.body)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response %q does not contain %q", response.Body.String(), test.want)
			}
		})
	}
}

func TestTagMutationValidation(t *testing.T) {
	server, dataStore := newTagAPITestServer(t)
	ctx := context.Background()
	if err := dataStore.CreateTag(ctx, &store.Tag{Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, body string
		status     int
	}{
		{"oversized body", `{"name":"large","description":"` + strings.Repeat("x", maxTagRequestBytes) + `"}`, http.StatusBadRequest},
		{"unknown field", `{"name":"unknown","surprise":true}`, http.StatusBadRequest},
		{"invalid rule", `{"name":"invalid","rule":{"field":"not.registered","op":"eq","value":"x"}}`, http.StatusBadRequest},
		{"duplicate name", `{"name":"  existing  "}`, http.StatusConflict},
		{"auto without rule", `{"name":"automatic","auto_enabled":true}`, http.StatusBadRequest},
		{"priority out of range", `{"name":"priority","priority":1001}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveTagAPI(server, http.MethodPost, "/api/tags", test.body)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestTagSchemaCoversEveryUnlockProvider(t *testing.T) {
	server, _ := newTagAPITestServer(t)
	response := serveTagAPI(server, http.MethodGet, "/api/tags/schema", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var schema tagSchemaResponse
	if err := json.Unmarshal(response.Body.Bytes(), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	fields := make(map[string]struct{}, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Name] = struct{}{}
	}
	for _, provider := range unlock.ListProviderMetas() {
		for _, attribute := range []string{"status", "region", "detail"} {
			name := nodefacts.UnlockField(provider.Value, attribute)
			if _, ok := fields[name]; !ok {
				t.Errorf("schema is missing field %q", name)
			}
		}
	}
	for _, key := range []string{
		nodefacts.EnumRegion, nodefacts.EnumCountryCode, nodefacts.EnumProtocol,
		nodefacts.EnumNodeSource, nodefacts.EnumIPFamily, nodefacts.EnumRiskLevel,
		nodefacts.EnumUnlockStatus, nodefacts.EnumUnlockProvider, nodefacts.EnumTagName,
		nodefacts.EnumSubscriptionID, nodefacts.EnumSubscriptionName,
	} {
		if _, ok := schema.Enums[key]; !ok {
			t.Errorf("schema is missing enum %q", key)
		}
	}
}

func TestTagSchemaWithoutStoreStillReturnsStaticEnums(t *testing.T) {
	server := &Server{}
	response := serveTagAPI(server, http.MethodGet, "/api/tags/schema", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	var schema tagSchemaResponse
	if err := json.Unmarshal(response.Body.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Enums[nodefacts.EnumProtocol].Options) == 0 || len(schema.Fields) == 0 {
		t.Fatalf("static schema is incomplete: %+v", schema)
	}
}

func TestTagPreviewIsReadOnlyAndDoesNotExposeCredentials(t *testing.T) {
	server, dataStore := newTagAPITestServer(t)
	ctx := context.Background()
	node := &store.Node{
		Name: "secret-node", URI: "socks5://secret-user:secret-pass@127.0.0.1:1080",
		Username: "credential-user", Password: "credential-pass", Source: store.NodeSourceManual,
		Region: "hk", Enabled: true,
	}
	if err := dataStore.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	tag := &store.Tag{Name: "manual-only"}
	if err := dataStore.CreateTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.SetManualNodeTags(ctx, node.ID, []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}
	before, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{})
	if err != nil {
		t.Fatal(err)
	}
	response := serveTagAPI(server, http.MethodPost, "/api/tags/preview",
		`{"rule":{"field":"node.name","op":"eq","value":"secret-node"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, secret := range []string{node.URI, node.Username, node.Password, "secret-user", "secret-pass"} {
		if strings.Contains(body, secret) {
			t.Errorf("preview leaked %q: %s", secret, body)
		}
	}
	after, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("preview changed assignments: before=%+v after=%+v", before, after)
	}
	for index := range before {
		if before[index].NodeID != after[index].NodeID || before[index].TagID != after[index].TagID || before[index].Source != after[index].Source {
			t.Fatalf("preview changed assignments: before=%+v after=%+v", before, after)
		}
	}
}

func TestManualTagAssignmentRefreshesProjectionAndQueuesRetag(t *testing.T) {
	server, dataStore := newTagAPITestServer(t)
	queue := &fakeRetagQueue{}
	runtimeManager := &tagAPIRuntimeManager{}
	server.retag = queue
	server.nodeMgr = runtimeManager
	ctx := context.Background()
	node := &store.Node{Name: "node", URI: "socks5://127.0.0.1:1080", Source: store.NodeSourceManual, Enabled: true}
	if err := dataStore.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	tag := &store.Tag{Name: "old-name"}
	if err := dataStore.CreateTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	assign := serveTagAPI(server, http.MethodPut, "/api/tags/nodes/"+strconv.FormatInt(node.ID, 10),
		`{"tag_ids":[`+strconv.FormatInt(tag.ID, 10)+`]}`)
	if assign.Code != http.StatusOK {
		t.Fatalf("assign status = %d; body=%s", assign.Code, assign.Body.String())
	}
	if len(queue.nodeIDs) != 1 || queue.nodeIDs[0] != node.ID {
		t.Fatalf("queued IDs = %v", queue.nodeIDs)
	}
	if len(runtimeManager.membershipChanges) != 1 || len(runtimeManager.membershipChanges[0]) != 1 || runtimeManager.membershipChanges[0][0] != node.ID {
		t.Fatalf("membership refreshes = %v", runtimeManager.membershipChanges)
	}
	rename := serveTagAPI(server, http.MethodPut, "/api/tags/"+strconv.FormatInt(tag.ID, 10), `{"name":"new-name"}`)
	if rename.Code != http.StatusOK {
		t.Fatalf("rename status = %d; body=%s", rename.Code, rename.Body.String())
	}
	reloaded, err := dataStore.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Tags) != 1 || reloaded.Tags[0] != "new-name" {
		t.Fatalf("projected tags = %v, want [new-name]", reloaded.Tags)
	}
}

func TestForceDeleteTagHotUpdatesReferencedGroups(t *testing.T) {
	server, dataStore := newTagAPITestServer(t)
	queue := &fakeRetagQueue{}
	runtimeManager := &tagAPIRuntimeManager{}
	server.retag = queue
	server.nodeMgr = runtimeManager
	ctx := context.Background()
	node := &store.Node{Name: "node", URI: "socks5://127.0.0.1:1080", Source: store.NodeSourceManual, Enabled: true}
	if err := dataStore.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	tag := &store.Tag{Name: "used"}
	if err := dataStore.CreateTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.SetManualNodeTags(ctx, node.ID, []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}
	groupPool := &store.GroupPool{
		Name: "filtered", BindAddress: "127.0.0.1", BindPort: 18080, Protocol: "http",
		DispatchMode: "round_robin", Regions: []string{}, ExplicitNodeIDs: []int64{}, ExcludedNodeIDs: []int64{},
		TagWhitelist: []int64{tag.ID}, TagBlacklist: []int64{}, TagFilterMatch: store.TagFilterMatchAny,
	}
	if err := dataStore.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}

	blocked := serveTagAPI(server, http.MethodDelete, "/api/tags/"+strconv.FormatInt(tag.ID, 10), "")
	if blocked.Code != http.StatusConflict {
		t.Fatalf("non-force status = %d; body=%s", blocked.Code, blocked.Body.String())
	}
	if stored, err := dataStore.GetTag(ctx, tag.ID); err != nil || stored == nil {
		t.Fatalf("blocked delete removed tag: tag=%+v err=%v", stored, err)
	}

	forced := serveTagAPI(server, http.MethodDelete, "/api/tags/"+strconv.FormatInt(tag.ID, 10)+"?force=1", "")
	if forced.Code != http.StatusOK {
		t.Fatalf("force status = %d; body=%s", forced.Code, forced.Body.String())
	}
	if len(runtimeManager.groupMutations) != 1 {
		t.Fatalf("runtime mutations = %d", len(runtimeManager.groupMutations))
	}
	mutation := runtimeManager.groupMutations[0]
	if mutation.before == nil || len(mutation.before.TagWhitelist) != 1 || mutation.after == nil || len(mutation.after.TagWhitelist) != 0 {
		t.Fatalf("runtime mutation = before:%+v after:%+v", mutation.before, mutation.after)
	}
	updatedGroup, err := dataStore.GetGroupPool(ctx, groupPool.ID)
	if err != nil || updatedGroup == nil || len(updatedGroup.TagWhitelist) != 0 {
		t.Fatalf("persisted group = %+v, err=%v", updatedGroup, err)
	}
	if runtimeManager.reloads.Load() != 0 {
		t.Fatalf("force delete triggered full reload %d times", runtimeManager.reloads.Load())
	}
}

type tagAPIGroupMutation struct {
	before *store.GroupPool
	after  *store.GroupPool
}

type tagAPIRuntimeManager struct {
	reloadNodeManager
	groupMutations    []tagAPIGroupMutation
	membershipChanges [][]int64
}

func (m *tagAPIRuntimeManager) ApplyGroupRuntime(_ context.Context, before, after *store.GroupPool) error {
	m.groupMutations = append(m.groupMutations, tagAPIGroupMutation{cloneGroupPool(before), cloneGroupPool(after)})
	return nil
}

func (m *tagAPIRuntimeManager) ActivateGroupMember(context.Context, int64, int64) error { return nil }

func (m *tagAPIRuntimeManager) GroupRuntimeStatus(int64) GroupRuntimeStatus {
	return GroupRuntimeStatus{}
}

func (m *tagAPIRuntimeManager) ApplyGroupMembershipChanges(_ context.Context, nodeIDs []int64) error {
	m.membershipChanges = append(m.membershipChanges, append([]int64(nil), nodeIDs...))
	return nil
}
