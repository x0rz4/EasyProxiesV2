package runtimenodes

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

func openStore(t *testing.T) store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "runtime-nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLoadIncludesOnlyEffectiveSubscriptionNodes(t *testing.T) {
	ctx := context.Background()
	db := openStore(t)

	manual := &store.Node{URI: "http://127.0.0.1:8080", Name: "manual", Source: store.NodeSourceManual, Enabled: true}
	disabled := &store.Node{URI: "http://127.0.0.2:8080", Name: "disabled", Source: store.NodeSourceManual, Enabled: false}
	for _, node := range []*store.Node{manual, disabled} {
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}

	enabledSub := &store.Subscription{Name: "enabled", URL: "https://example.com/a", Enabled: true}
	disabledSub := &store.Subscription{Name: "disabled", URL: "https://example.com/b", Enabled: false}
	for _, sub := range []*store.Subscription{enabledSub, disabledSub} {
		if err := db.CreateSubscription(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.CommitSnapshot(ctx, enabledSub.ID, []store.SubscriptionNodeInput{{
		URI: "http://127.0.0.3:8080", Name: "from-enabled", Enabled: true,
	}}, store.SubscriptionSnapshot{Attempt: now, Success: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSnapshot(ctx, disabledSub.ID, []store.SubscriptionNodeInput{{
		URI: "http://127.0.0.4:8080", Name: "from-disabled", Enabled: true,
	}}, store.SubscriptionSnapshot{Attempt: now, Success: now}); err != nil {
		t.Fatal(err)
	}

	nodes, err := Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		got[node.Name] = true
	}
	if len(nodes) != 2 || !got["manual"] || !got["from-enabled"] {
		t.Fatalf("runtime nodes = %+v, want exactly manual and from-enabled", nodes)
	}
	if got["disabled"] || got["from-disabled"] {
		t.Fatalf("disabled nodes leaked into the runtime set: %+v", nodes)
	}
}

// duplicateIdentityStore feeds Load a manual row and a subscription row that
// resolve to the same identity, which the schema normally prevents but imported
// or legacy data can still produce.
type duplicateIdentityStore struct {
	store.Store
}

func (duplicateIdentityStore) ListNodes(context.Context, store.NodeFilter) ([]store.Node, error) {
	return []store.Node{{ID: 1, URI: "http://user:pass@example.com:8080", Name: "manual-first", Enabled: true, IdentityHash: "same"}}, nil
}

func (duplicateIdentityStore) ListEffectiveSubscriptionNodes(context.Context) ([]store.Node, error) {
	return []store.Node{{ID: 2, URI: "http://user:pass@example.com", Name: "subscription-copy", Enabled: true, IdentityHash: "same"}}, nil
}

func TestLoadCollapsesDuplicateIdentities(t *testing.T) {
	nodes, err := Load(context.Background(), duplicateIdentityStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "manual-first" {
		t.Fatalf("runtime nodes = %+v, want the first identity holder only", nodes)
	}
}

func TestConvertCopiesTagsWithoutAliasing(t *testing.T) {
	tags := []string{"a", "b"}
	nodes := Convert([]store.Node{{ID: 7, URI: "http://127.0.0.1:1080", Name: "n", Enabled: true, Tags: tags}})
	if len(nodes) != 1 || nodes[0].ID != 7 {
		t.Fatalf("converted=%+v", nodes)
	}
	nodes[0].Tags[0] = "changed"
	if tags[0] != "a" {
		t.Fatal("Convert aliased the store row's tag slice")
	}
}

func TestIdentityKeyFallsBackToURI(t *testing.T) {
	if key := IdentityKey(store.Node{URI: " ss://a ", IdentityHash: ""}); key != "ss://a" {
		t.Fatalf("identity key = %q", key)
	}
	if key := IdentityKey(store.Node{URI: "ss://a", IdentityHash: "hash"}); key != "hash" {
		t.Fatalf("identity key = %q", key)
	}
}

func TestNodeConfigSourceIsPreserved(t *testing.T) {
	nodes := Convert([]store.Node{{ID: 1, URI: "http://127.0.0.1:1080", Source: store.NodeSourceManual, Enabled: true}})
	if nodes[0].Source != config.NodeSourceManual {
		t.Fatalf("source = %q", nodes[0].Source)
	}
}
