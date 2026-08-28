package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// migrateUpTo applies migrations with Version <= target, mirroring Migrate's
// bookkeeping so a later Migrate call resumes from that version.
func migrateUpTo(t *testing.T, db *sql.DB, target int) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			applied_at  TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range allMigrations() {
		if migration.Version > target {
			break
		}
		if _, err := db.Exec(migration.Up); err != nil {
			t.Fatalf("apply migration %d: %v", migration.Version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations (version, applied_at, description) VALUES (?, ?, ?)",
			migration.Version, time.Now().UTC().Format(time.RFC3339), migration.Description,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMigration13BackfillsExistingTags is the json_each availability gate for
// migration 13: every legacy nodes.tags entry must survive the upgrade as a
// manual assignment, emoji included.
func TestMigration13BackfillsExistingTags(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	migrateUpTo(t, db, 12)

	if _, err := db.Exec(
		`INSERT INTO nodes (uri, name, tags) VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?), (?, ?, ?)`,
		"ss://a@127.0.0.1:1", "hk", `["🇭🇰香港","game","  "]`,
		"ss://b@127.0.0.1:2", "jp", `[" game ","isp"]`,
		"ss://c@127.0.0.1:3", "broken", `not json at all`,
		"ss://d@127.0.0.1:4", "empty", `[]`,
	); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate to 13::%v", err)
	}
	version, err := CurrentVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 13 {
		t.Fatalf("schema version = %d, want 13", version)
	}

	names := map[string]int64{}
	rows, err := db.Query("SELECT id, name FROM tags ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		names[name] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(names) != 3 {
		t.Fatalf("tags = %v, want exactly 🇭🇰香港/game/isp", names)
	}
	for _, want := range []string{"🇭🇰香港", "game", "isp"} {
		if names[want] == 0 {
			t.Fatalf("tag %q missing after backfill: %v", want, names)
		}
	}

	type assignment struct {
		nodeName string
		tagName  string
		source   string
	}
	var got []assignment
	rows, err = db.Query(`
		SELECT nodes.name, tags.name, node_tags.source
		FROM node_tags
		JOIN nodes ON nodes.id = node_tags.node_id
		JOIN tags ON tags.id = node_tags.tag_id
		ORDER BY nodes.name, tags.name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var row assignment
		if err := rows.Scan(&row.nodeName, &row.tagName, &row.source); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	want := []assignment{
		{"hk", "game", NodeTagSourceManual},
		{"hk", "🇭🇰香港", NodeTagSourceManual},
		{"jp", "game", NodeTagSourceManual},
		{"jp", "isp", NodeTagSourceManual},
	}
	if len(got) != len(want) {
		t.Fatalf("node_tags = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("node_tags[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}

	var whitelist, blacklist, match string
	if err := db.QueryRow(
		"SELECT tag_whitelist_json, tag_blacklist_json, tag_filter_match FROM group_pools LIMIT 1",
	).Scan(&whitelist, &blacklist, &match); err != nil && err != sql.ErrNoRows {
		t.Fatalf("group_pools tag columns missing: %v", err)
	}
}

// openTagStore returns a fully migrated store plus one node to hang tags on.
func openTagStore(t *testing.T) (*sqliteStore, context.Context, int64) {
	t.Helper()
	opened, err := Open(filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	db := opened.(*sqliteStore)
	ctx := context.Background()
	node := &Node{URI: "ss://node@127.0.0.1:1080", Name: "node-a", Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	return db, ctx, node.ID
}

func mustCreateTag(t *testing.T, db *sqliteStore, ctx context.Context, name string) *Tag {
	t.Helper()
	tag := &Tag{Name: name}
	if err := db.CreateTag(ctx, tag); err != nil {
		t.Fatalf("create tag %q: %v", name, err)
	}
	return tag
}

func nodeTagProjection(t *testing.T, db *sqliteStore, ctx context.Context, nodeID int64) []string {
	t.Helper()
	node, err := db.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if node == nil {
		t.Fatalf("node %d disappeared", nodeID)
	}
	return node.Tags
}

func assertStrings(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// TestCreateNodeMaterializesTags covers the invariant the whole design rests on:
// nodes.tags is only a projection, so a node created with tag names must end up
// with real manual assignments or the first recompute would erase them.
func TestCreateNodeMaterializesTags(t *testing.T) {
	opened, err := Open(filepath.Join(t.TempDir(), "create.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	db := opened.(*sqliteStore)
	ctx := context.Background()

	node := &Node{URI: "ss://seed@127.0.0.1:1080", Name: "seed", Tags: []string{"game", "isp", "game"}}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	assignments, err := db.ListNodeTags(ctx, NodeTagFilter{NodeIDs: []int64{node.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 2 {
		t.Fatalf("assignments = %+v, want 2 manual rows", assignments)
	}
	for _, assignment := range assignments {
		if assignment.Source != NodeTagSourceManual {
			t.Fatalf("assignment %+v is not manual", assignment)
		}
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, node.ID), []string{"game", "isp"}, "projection")

	// A recompute with no auto matches must leave the manual tags in place.
	if err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{{NodeID: node.ID}}); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, node.ID), []string{"game", "isp"}, "projection after recompute")
}

// TestUpdateNodeLeavesTagsAlone pins that nodes.tags is owned by the tagging
// layer: an UpdateNode caller carrying a stale or empty Tags slice must not be
// able to drop assignments.
func TestUpdateNodeLeavesTagsAlone(t *testing.T) {
	db, ctx, nodeID := openTagStore(t)
	tag := mustCreateTag(t, db, ctx, "keep-me")
	if err := db.SetManualNodeTags(ctx, nodeID, []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}

	node, err := db.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	node.Tags = nil
	node.Name = "renamed"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, nodeID), []string{"keep-me"}, "projection")
}

// TestReplaceAutoNodeTagsPreservesManual is the anti-clobber regression test:
// recompute owns source='auto' rows and nothing else.
func TestReplaceAutoNodeTagsPreservesManual(t *testing.T) {
	db, ctx, nodeID := openTagStore(t)
	manual := mustCreateTag(t, db, ctx, "manual-only")
	shared := mustCreateTag(t, db, ctx, "both-sources")
	autoOnly := mustCreateTag(t, db, ctx, "auto-only")
	replacement := mustCreateTag(t, db, ctx, "auto-second")

	if err := db.SetManualNodeTags(ctx, nodeID, []int64{manual.ID, shared.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{{
		NodeID:       nodeID,
		TagIDs:       []int64{shared.ID, autoOnly.ID},
		RuleVersions: []int{7},
	}}); err != nil {
		t.Fatal(err)
	}

	// The same (node, tag) pair is legal from both sources simultaneously.
	sharedRows, err := db.ListNodeTags(ctx, NodeTagFilter{NodeIDs: []int64{nodeID}, TagIDs: []int64{shared.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedRows) != 2 {
		t.Fatalf("shared tag rows = %+v, want one manual and one auto", sharedRows)
	}
	for _, row := range sharedRows {
		if row.Source == NodeTagSourceAuto && row.RuleVersion != 7 {
			t.Fatalf("auto row rule_version = %d, want 7", row.RuleVersion)
		}
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, nodeID),
		[]string{"auto-only", "both-sources", "manual-only"}, "projection")

	// A second recompute replaces the auto set wholesale, manual rows untouched.
	if err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{{
		NodeID: nodeID,
		TagIDs: []int64{replacement.ID},
	}}); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, nodeID),
		[]string{"auto-second", "both-sources", "manual-only"}, "projection after second recompute")

	autoRows, err := db.ListNodeTags(ctx, NodeTagFilter{NodeIDs: []int64{nodeID}, Source: NodeTagSourceAuto})
	if err != nil {
		t.Fatal(err)
	}
	if len(autoRows) != 1 || autoRows[0].TagID != replacement.ID {
		t.Fatalf("auto rows = %+v, want only tag %d", autoRows, replacement.ID)
	}
}

// TestReplaceAutoNodeTagsRollsBack asserts a mid-batch failure leaves no
// partially applied recompute behind.
func TestReplaceAutoNodeTagsRollsBack(t *testing.T) {
	db, ctx, nodeID := openTagStore(t)
	good := mustCreateTag(t, db, ctx, "good")
	if err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{{NodeID: nodeID, TagIDs: []int64{good.ID}}}); err != nil {
		t.Fatal(err)
	}

	// The second assignment references a tag that does not exist, so the foreign
	// key rejects it after the first node's rows were already rewritten.
	other := &Node{URI: "ss://other@127.0.0.1:1081", Name: "node-b"}
	if err := db.CreateNode(ctx, other); err != nil {
		t.Fatal(err)
	}
	err := db.ReplaceAutoNodeTags(ctx, []NodeAutoTagAssignment{
		{NodeID: nodeID, TagIDs: nil},
		{NodeID: other.ID, TagIDs: []int64{good.ID + 9999}},
	})
	if err == nil {
		t.Fatal("expected a foreign key failure")
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, nodeID), []string{"good"}, "projection after rollback")
}

// TestBatchUpdateManualNodeTags checks that removals win over additions and that
// every touched node gets a fresh projection.
func TestBatchUpdateManualNodeTags(t *testing.T) {
	db, ctx, firstID := openTagStore(t)
	second := &Node{URI: "ss://second@127.0.0.1:1081", Name: "node-b"}
	if err := db.CreateNode(ctx, second); err != nil {
		t.Fatal(err)
	}
	keep := mustCreateTag(t, db, ctx, "keep")
	drop := mustCreateTag(t, db, ctx, "drop")

	if err := db.BatchUpdateManualNodeTags(ctx, []int64{firstID, second.ID},
		[]int64{keep.ID, drop.ID}, []int64{drop.ID}); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, firstID), []string{"keep"}, "first projection")
	assertStrings(t, nodeTagProjection(t, db, ctx, second.ID), []string{"keep"}, "second projection")

	counts, err := db.CountNodesByTag(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[keep.ID] != 2 {
		t.Fatalf("CountNodesByTag[%d] = %d, want 2", keep.ID, counts[keep.ID])
	}
	if counts[drop.ID] != 0 {
		t.Fatalf("CountNodesByTag[%d] = %d, want 0", drop.ID, counts[drop.ID])
	}
}

// TestDeleteTagCleansUpEverywhere covers the assignment cascade, the group pool
// filter cleanup, and the projection rewrite in one pass.
func TestDeleteTagCleansUpEverywhere(t *testing.T) {
	db, ctx, nodeID := openTagStore(t)
	doomed := mustCreateTag(t, db, ctx, "doomed")
	survivor := mustCreateTag(t, db, ctx, "survivor")
	if err := db.SetManualNodeTags(ctx, nodeID, []int64{doomed.ID, survivor.ID}); err != nil {
		t.Fatal(err)
	}

	pool := &GroupPool{
		Name: "pool", BindAddress: "127.0.0.1", BindPort: 20001, Protocol: "mixed",
		DispatchMode: "fixed", Enabled: true,
		TagWhitelist: []int64{doomed.ID, survivor.ID},
		TagBlacklist: []int64{doomed.ID},
	}
	if err := db.CreateGroupPool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteTag(ctx, doomed.ID); err != nil {
		t.Fatal(err)
	}
	assertStrings(t, nodeTagProjection(t, db, ctx, nodeID), []string{"survivor"}, "projection")

	reloaded, err := db.GetGroupPool(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.TagWhitelist) != 1 || reloaded.TagWhitelist[0] != survivor.ID {
		t.Fatalf("whitelist = %v, want [%d]", reloaded.TagWhitelist, survivor.ID)
	}
	if len(reloaded.TagBlacklist) != 0 {
		t.Fatalf("blacklist = %v, want empty", reloaded.TagBlacklist)
	}
	if reloaded.TagFilterMatch != TagFilterMatchAny {
		t.Fatalf("tag_filter_match = %q, want %q", reloaded.TagFilterMatch, TagFilterMatchAny)
	}

	rows, err := db.ListNodeTags(ctx, NodeTagFilter{NodeIDs: []int64{nodeID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.TagID == doomed.ID {
			t.Fatalf("assignment %+v survived tag deletion", row)
		}
	}
}

// TestDeleteNodeCascadesTags asserts assignments die with their node.
func TestDeleteNodeCascadesTags(t *testing.T) {
	db, ctx, nodeID := openTagStore(t)
	tag := mustCreateTag(t, db, ctx, "orphan-check")
	if err := db.SetManualNodeTags(ctx, nodeID, []int64{tag.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteNode(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListNodeTags(ctx, NodeTagFilter{TagIDs: []int64{tag.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("assignments = %+v, want none after node deletion", rows)
	}
}

// TestGroupPoolTagFilterRoundTrip covers create, update and the normalization of
// the three new group_pools columns.
func TestGroupPoolTagFilterRoundTrip(t *testing.T) {
	db, ctx, _ := openTagStore(t)
	first := mustCreateTag(t, db, ctx, "hk")
	second := mustCreateTag(t, db, ctx, "native-ip")

	pool := &GroupPool{
		Name: "hk-vip", BindAddress: "127.0.0.1", BindPort: 20101, Protocol: "mixed",
		DispatchMode: "fixed", Enabled: true,
		TagWhitelist:   []int64{first.ID, first.ID, 0, -3, second.ID},
		TagBlacklist:   []int64{second.ID},
		TagFilterMatch: "all",
	}
	if err := db.CreateGroupPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	created, err := db.GetGroupPool(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.TagWhitelist) != 2 || created.TagWhitelist[0] != first.ID || created.TagWhitelist[1] != second.ID {
		t.Fatalf("whitelist = %v, want deduplicated [%d %d]", created.TagWhitelist, first.ID, second.ID)
	}
	if created.TagFilterMatch != TagFilterMatchAll {
		t.Fatalf("tag_filter_match = %q, want %q", created.TagFilterMatch, TagFilterMatchAll)
	}

	created.TagWhitelist = nil
	created.TagBlacklist = []int64{first.ID}
	created.TagFilterMatch = "garbage"
	if err := db.UpdateGroupPool(ctx, created); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetGroupPool(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TagWhitelist) != 0 {
		t.Fatalf("whitelist = %v, want empty", updated.TagWhitelist)
	}
	if len(updated.TagBlacklist) != 1 || updated.TagBlacklist[0] != first.ID {
		t.Fatalf("blacklist = %v, want [%d]", updated.TagBlacklist, first.ID)
	}
	if updated.TagFilterMatch != TagFilterMatchAny {
		t.Fatalf("tag_filter_match = %q, want %q for an unknown value", updated.TagFilterMatch, TagFilterMatchAny)
	}
}

// TestBatchFactReadsSubset checks the ID-filtered reads the tagging engine uses
// to load a whole fact set in a fixed number of queries.
func TestBatchFactReadsSubset(t *testing.T) {
	db, ctx, firstID := openTagStore(t)
	second := &Node{URI: "ss://second@127.0.0.1:1081", Name: "node-b", Enabled: true}
	third := &Node{URI: "ss://third@127.0.0.1:1082", Name: "node-c", Enabled: true}
	if err := db.CreateNode(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNode(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeDetectionResult(ctx, &NodeDetectionResult{
		NodeID: second.ID, LatencyStatus: "success", SpeedStatus: "untested", ExitIPStatus: "untested",
	}); err != nil {
		t.Fatal(err)
	}
	sub := &Subscription{Name: "sub", URL: "https://example.test/sub", Enabled: true,
		RefreshIntervalSeconds: 60, RefreshTimeoutSeconds: 10}
	if err := db.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writerDB.Exec(
		"INSERT INTO subscription_nodes(subscription_id,node_id,position) VALUES(?,?,?)",
		sub.ID, second.ID, 0); err != nil {
		t.Fatal(err)
	}

	nodes, err := db.ListNodes(ctx, NodeFilter{NodeIDs: []int64{firstID, third.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != firstID || nodes[1].ID != third.ID {
		t.Fatalf("ListNodes by IDs = %+v, want %d and %d", nodes, firstID, third.ID)
	}
	count, err := db.CountNodes(ctx, NodeFilter{NodeIDs: []int64{firstID}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CountNodes by IDs = %d, want 1", count)
	}

	stats, err := db.ListNodeStats(ctx, []int64{firstID})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[firstID] == nil {
		t.Fatalf("ListNodeStats = %+v, want only node %d", stats, firstID)
	}

	detections, err := db.ListNodeDetectionResultsByIDs(ctx, []int64{firstID})
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != 0 {
		t.Fatalf("detections = %+v, want none for a node with no results", detections)
	}
	detections, err = db.ListNodeDetectionResultsByIDs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != 1 || detections[second.ID] == nil {
		t.Fatalf("detections = %+v, want the one stored result", detections)
	}

	subscriptionIDs, err := db.ListNodeSubscriptionIDs(ctx, []int64{second.ID, third.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptionIDs) != 1 || len(subscriptionIDs[second.ID]) != 1 || subscriptionIDs[second.ID][0] != sub.ID {
		t.Fatalf("subscription IDs = %+v, want node %d in subscription %d", subscriptionIDs, second.ID, sub.ID)
	}
}

// TestDeleteTagMutexGroupKeepsTags asserts deleting a mutex group only removes
// the mutual exclusion, never the tags in it.
func TestDeleteTagMutexGroupKeepsTags(t *testing.T) {
	db, ctx, _ := openTagStore(t)
	group := &TagMutexGroup{Name: "risk", Description: "risk tier"}
	if err := db.CreateTagMutexGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	tag := &Tag{Name: "risk-high", MutexGroupID: group.ID, Priority: 20}
	if err := db.CreateTag(ctx, tag); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteTagMutexGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.GetTag(ctx, tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil {
		t.Fatal("tag deleted along with its mutex group")
	}
	if reloaded.MutexGroupID != 0 {
		t.Fatalf("mutex_group_id = %d, want 0", reloaded.MutexGroupID)
	}
	if reloaded.Priority != 20 {
		t.Fatalf("priority = %d, want 20 (untouched)", reloaded.Priority)
	}
}
