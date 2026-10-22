package boxmgr

import (
	"easy_proxies/internal/config"
)

// rebaseReloadedNodes merges node-level mutations that landed on the runtime
// config during a reload window into the config about to be committed.
//
// TriggerReload snapshots the store-backed config before a reload starts, and
// a full reload can run for tens of seconds (drain, two builds, landing
// detection). Meanwhile ApplyGroupMembershipChanges (retagging), landing
// reclassification and node CRUD write their results into m.cfg. Committing
// the stale snapshot verbatim rolled those facts back until the next reload:
// tags reverted, countries reverted, freshly created nodes vanished from the
// base topology.
//
// Matching is by NodeKey (identity hash first), which is stable across
// renames. Only the facts the window can legitimately change are merged
// (Tags/Region/Country); the reload's own topology decisions still win.
// Group definitions are hydrated from the store by the reload path itself
// and are not rebased here.
func rebaseReloadedNodes(snapshotCfg, runtimeCfg, newCfg *config.Config) int {
	if snapshotCfg == nil || runtimeCfg == nil || newCfg == nil {
		return 0
	}
	oldByKey := make(map[string]config.NodeConfig, len(snapshotCfg.Nodes))
	for _, node := range snapshotCfg.Nodes {
		oldByKey[node.NodeKey()] = node
	}
	// Keep indices, not element pointers: appending window-created nodes can
	// reallocate Nodes before later entries receive their updated facts.
	newByKey := make(map[string]int, len(newCfg.Nodes))
	for index := range newCfg.Nodes {
		newByKey[newCfg.Nodes[index].NodeKey()] = index
	}
	mutated := 0
	for _, runtime := range runtimeCfg.Nodes {
		key := runtime.NodeKey()
		old, hadOld := oldByKey[key]
		index, inNew := newByKey[key]
		if !hadOld {
			// Created during the window: keep it in the committed topology.
			if !inNew {
				newByKey[key] = len(newCfg.Nodes)
				newCfg.Nodes = append(newCfg.Nodes, runtime)
				mutated++
			}
			continue
		}
		if !inNew {
			// The reload decided this node is gone; the removal wins.
			continue
		}
		if nodeFactsDiffer(old, runtime) {
			target := &newCfg.Nodes[index]
			target.Tags = append([]string(nil), runtime.Tags...)
			target.Region = runtime.Region
			target.Country = runtime.Country
			mutated++
		}
	}
	return mutated
}

func nodeFactsDiffer(a, b config.NodeConfig) bool {
	if a.Region != b.Region || a.Country != b.Country || len(a.Tags) != len(b.Tags) {
		return true
	}
	for index := range a.Tags {
		if a.Tags[index] != b.Tags[index] {
			return true
		}
	}
	return false
}
