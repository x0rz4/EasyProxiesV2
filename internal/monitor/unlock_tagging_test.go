package monitor

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"easy_proxies/internal/store"
	"easy_proxies/internal/unlock"
)

// fakeRetagQueue records what the detection pipeline asked to have re-tagged.
type fakeRetagQueue struct {
	nodeIDs []int64
	all     int
}

func (q *fakeRetagQueue) Enqueue(nodeIDs ...int64) { q.nodeIDs = append(q.nodeIDs, nodeIDs...) }
func (q *fakeRetagQueue) EnqueueAll()              { q.all++ }

// TestPersistUnlockResultKeepsManualTagsAndEnqueues is the regression test for
// the bug the tagging system replaces: persistUnlockResult used to derive
// 原生IP / 高风险 / <服务>解锁 itself and assign them with UpdateNode, which
// deleted every hand-placed tag on the node. It must now only persist the result
// and hand the node to the tag queue.
func TestPersistUnlockResultKeepsManualTagsAndEnqueues(t *testing.T) {
	ctx := context.Background()
	dataStore, err := store.Open(filepath.Join(t.TempDir(), "unlock-tags.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer dataStore.Close()

	node := &store.Node{
		URI:     "ss://unlock@127.0.0.1:1080",
		Name:    "hk-01",
		Source:  store.NodeSourceManual,
		Region:  "hk",
		Enabled: true,
	}
	if err := dataStore.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	game := &store.Tag{Name: "game", Description: "运营手工维护"}
	if err := dataStore.CreateTag(ctx, game); err != nil {
		t.Fatalf("create manual tag: %v", err)
	}
	if err := dataStore.SetManualNodeTags(ctx, node.ID, []int64{game.ID}); err != nil {
		t.Fatalf("assign manual tag: %v", err)
	}

	queue := &fakeRetagQueue{}
	server := &Server{store: dataStore, logger: log.New(io.Discard, "", 0), retag: queue}
	snap := Snapshot{NodeInfo: NodeInfo{NodeID: node.ID, Tag: "hk-01", Name: "hk-01"}}
	result := &unlock.Result{
		Tag: "hk-01", Name: "hk-01",
		IP: unlock.IPInfo{IP: "203.0.113.7", Pure: true, RiskLevel: "High"},
		Services: []unlock.ServiceResult{
			{Name: "netflix", DisplayName: "Netflix", Status: unlock.StatusUnlocked, Region: "HK"},
		},
	}
	server.persistUnlockResult(&snap, result)

	// The facts are persisted — the tags are derived from these, not written here.
	stored, err := dataStore.GetUnlockResult(ctx, node.ID)
	if err != nil || stored == nil {
		t.Fatalf("unlock result was not persisted: %v", err)
	}
	if !stored.IP.Pure || len(stored.Services) != 1 ||
		stored.Services[0].Status != unlock.StatusUnlocked {
		t.Fatalf("stored result = %+v", stored)
	}

	// The hand-placed tag survived, and no auto tag was invented behind the
	// service's back.
	reloaded, err := dataStore.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if len(reloaded.Tags) != 1 || reloaded.Tags[0] != "game" {
		t.Fatalf("node tags = %v, want only the manual tag", reloaded.Tags)
	}
	autoRows, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{
		NodeIDs: []int64{node.ID}, Source: store.NodeTagSourceAuto,
	})
	if err != nil {
		t.Fatalf("list auto tags: %v", err)
	}
	if len(autoRows) != 0 {
		t.Fatalf("persistUnlockResult wrote %d auto assignments itself", len(autoRows))
	}
	if len(queue.nodeIDs) != 1 || queue.nodeIDs[0] != node.ID {
		t.Fatalf("enqueued %v, want the checked node", queue.nodeIDs)
	}

	// A server with no queue wired must still persist the result.
	quiet := &Server{store: dataStore, logger: log.New(io.Discard, "", 0)}
	quiet.persistUnlockResult(&snap, result)
}

// TestNodeMutationsRequestAFullRecompute covers the other trigger class: node
// CRUD changes which nodes exist rather than what is known about one node, so it
// asks for a full recompute instead of naming IDs.
func TestNodeMutationsRequestAFullRecompute(t *testing.T) {
	queue := &fakeRetagQueue{}
	server := &Server{nodeMgr: &reloadNodeManager{}, logger: log.New(io.Discard, "", 0), retag: queue}
	payload := `{"name":"node-a","uri":"socks5://127.0.0.1:1080","port":1080}`

	for _, mutation := range []struct {
		name    string
		request *http.Request
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"create", httptest.NewRequest(http.MethodPost, "/api/nodes/config",
			strings.NewReader(payload)), server.handleConfigNodes},
		{"update", httptest.NewRequest(http.MethodPut, "/api/nodes/config/node-a",
			strings.NewReader(payload)), server.handleConfigNodeItem},
		{"toggle", httptest.NewRequest(http.MethodPatch, "/api/nodes/config/node-a",
			strings.NewReader(`{"enabled":false}`)), server.handleConfigNodeItem},
		{"delete", httptest.NewRequest(http.MethodDelete, "/api/nodes/config/node-a",
			nil), server.handleConfigNodeItem},
		{"batch-toggle", httptest.NewRequest(http.MethodPost, "/api/nodes/config/batch-toggle",
			strings.NewReader(`{"names":["node-a"],"enabled":true}`)), server.handleConfigNodesBatchToggle},
		{"batch-delete", httptest.NewRequest(http.MethodPost, "/api/nodes/config/batch-delete",
			strings.NewReader(`{"names":["node-a"]}`)), server.handleConfigNodesBatchDelete},
	} {
		before := queue.all
		mutation.handler(httptest.NewRecorder(), mutation.request)
		if queue.all != before+1 {
			t.Fatalf("%s did not request a recompute", mutation.name)
		}
	}
	if len(queue.nodeIDs) != 0 {
		t.Fatalf("node CRUD named specific nodes: %v", queue.nodeIDs)
	}

	// Nothing wired and nothing at all must both be safe: probe goroutines call
	// these without knowing the shutdown order.
	(&Server{}).enqueueRetag(1)
	(&Server{}).enqueueRetagAll()
	var absent *Server
	absent.enqueueRetag(1)
	absent.enqueueRetagAll()
}
