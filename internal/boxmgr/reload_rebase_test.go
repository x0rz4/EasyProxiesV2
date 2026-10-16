package boxmgr

import (
	"testing"

	"easy_proxies/internal/config"
)

func TestRebaseReloadedNodesMergesWindowMutations(t *testing.T) {
	snapshot := &config.Config{Nodes: []config.NodeConfig{
		{ID: 1, Name: "a", URI: "http://a:1", IdentityHash: "hash-a", Tags: []string{"old"}, Region: "us"},
		{ID: 2, Name: "b", URI: "http://b:1", IdentityHash: "hash-b"},
	}}
	runtime := &config.Config{Nodes: []config.NodeConfig{
		// Retagged + reclassified during the window.
		{ID: 1, Name: "a", URI: "http://a:1", IdentityHash: "hash-a", Tags: []string{"new", "fast"}, Region: "hk", Country: "Hong Kong"},
		// Created during the window.
		{ID: 3, Name: "c", URI: "http://c:1", IdentityHash: "hash-c", Tags: []string{"fresh"}},
		// Deleted during the window: must NOT reappear.
		{ID: 2, Name: "b", URI: "http://b:1", IdentityHash: "hash-b"},
	}}
	newCfg := &config.Config{Nodes: []config.NodeConfig{
		{ID: 1, Name: "a", URI: "http://a:1", IdentityHash: "hash-a", Tags: []string{"old"}, Region: "us"},
	}}

	mutated := rebaseReloadedNodes(snapshot, runtime, newCfg)
	if mutated != 2 {
		t.Fatalf("mutated = %d, want 2 (retag + create)", mutated)
	}
	if len(newCfg.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (rebased + created, deleted stays gone)", len(newCfg.Nodes))
	}
	merged := newCfg.Nodes[0]
	if merged.Tags[0] != "new" || merged.Region != "hk" || merged.Country != "Hong Kong" {
		t.Fatalf("window facts were not merged: %+v", merged)
	}
	if newCfg.Nodes[1].ID != 3 || newCfg.Nodes[1].Tags[0] != "fresh" {
		t.Fatalf("window-created node missing: %+v", newCfg.Nodes[1])
	}
}

func TestRebaseReloadedNodesIsNoopWithoutMutations(t *testing.T) {
	snapshot := &config.Config{Nodes: []config.NodeConfig{
		{ID: 1, Name: "a", URI: "http://a:1", IdentityHash: "hash-a", Tags: []string{"t"}, Region: "us"},
	}}
	runtime := &config.Config{Nodes: append([]config.NodeConfig(nil), snapshot.Nodes...)}
	newCfg := &config.Config{Nodes: append([]config.NodeConfig(nil), snapshot.Nodes...)}

	if mutated := rebaseReloadedNodes(snapshot, runtime, newCfg); mutated != 0 {
		t.Fatalf("mutated = %d, want 0", mutated)
	}
	// nil inputs must be tolerated.
	if mutated := rebaseReloadedNodes(nil, runtime, newCfg); mutated != 0 {
		t.Fatalf("nil snapshot: mutated = %d, want 0", mutated)
	}
	if mutated := rebaseReloadedNodes(snapshot, nil, newCfg); mutated != 0 {
		t.Fatalf("nil runtime: mutated = %d, want 0", mutated)
	}
}
