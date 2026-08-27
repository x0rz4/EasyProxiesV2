package nodetag

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/store"
)

// The service is tested against a real SQLite store: the guarantee under test —
// "a recompute never touches a manual assignment" — lives half in the resolver
// and half in ReplaceAutoNodeTags, so a fake store would test neither.
func openTagStore(t *testing.T) store.Store {
	t.Helper()
	opened, err := store.Open(filepath.Join(t.TempDir(), "nodetag.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

// newNode creates a node whose only measured fact is its latency. A negative
// latency means "never measured", which is what a node with no stats row has.
func newNode(t *testing.T, dataStore store.Store, name string, latencyMs int64) int64 {
	t.Helper()
	ctx := context.Background()
	node := &store.Node{
		URI:     "ss://" + name + "@127.0.0.1:1080",
		Name:    name,
		Source:  store.NodeSourceManual,
		Region:  "hk",
		Enabled: true,
	}
	if err := dataStore.CreateNode(ctx, node); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	if err := dataStore.UpsertNodeStats(ctx, &store.NodeStats{
		NodeID:           node.ID,
		LastLatencyMs:    latencyMs,
		Available:        true,
		InitialCheckDone: true,
	}); err != nil {
		t.Fatalf("upsert stats for %s: %v", name, err)
	}
	return node.ID
}

func tagNames(t *testing.T, dataStore store.Store, nodeID int64, source string) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{
		NodeIDs: []int64{nodeID}, Source: source,
	})
	if err != nil {
		t.Fatalf("list node tags: %v", err)
	}
	tags, err := dataStore.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	byID := make(map[int64]string, len(tags))
	for _, tag := range tags {
		byID[tag.ID] = tag.Name
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, byID[row.TagID])
	}
	sort.Strings(names)
	return names
}

func projectionOf(t *testing.T, dataStore store.Store, nodeID int64) []string {
	t.Helper()
	node, err := dataStore.GetNode(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("get node %d: %v", nodeID, err)
	}
	return node.Tags
}

func assertNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

// newAutoTag creates a rule-driven tag directly, for cases the builtin templates
// do not cover.
func newAutoTag(t *testing.T, dataStore store.Store, name string, groupID int64, priority int, condition nodefacts.Condition) int64 {
	t.Helper()
	encoded, err := nodefacts.MarshalRule(condition)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	tag := &store.Tag{
		Name:         name,
		MutexGroupID: groupID,
		Priority:     priority,
		AutoEnabled:  true,
		RuleJSON:     string(encoded),
	}
	if err := dataStore.CreateTag(context.Background(), tag); err != nil {
		t.Fatalf("create tag %q: %v", name, err)
	}
	return tag.ID
}

// TestRecomputeReplacesAutoTagsAndKeepsManualOnes is the regression test for the
// bug this whole system replaces: the old unlock pipeline rewrote nodes.tags
// wholesale and deleted every hand-placed tag.
func TestRecomputeReplacesAutoTagsAndKeepsManualOnes(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	fast := newNode(t, dataStore, "fast", 50)
	slow := newNode(t, dataStore, "slow", 500)
	unmeasured := newNode(t, dataStore, "unmeasured", -1)

	service := NewService(dataStore)
	if _, err := service.SeedTemplates(ctx); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
	game := &store.Tag{Name: "game", Description: "运营手工维护"}
	if err := dataStore.CreateTag(ctx, game); err != nil {
		t.Fatalf("create manual tag: %v", err)
	}
	if err := dataStore.SetManualNodeTags(ctx, fast, []int64{game.ID}); err != nil {
		t.Fatalf("assign manual tag: %v", err)
	}

	changed, err := service.RecomputeAll(ctx)
	if err != nil {
		t.Fatalf("RecomputeAll: %v", err)
	}
	assertTagIDs(t, changed, fast, slow)
	assertNames(t, tagNames(t, dataStore, fast, store.NodeTagSourceAuto), "⚡极速")
	assertNames(t, tagNames(t, dataStore, fast, store.NodeTagSourceManual), "game")
	assertNames(t, projectionOf(t, dataStore, fast), "game", "⚡极速")
	assertNames(t, tagNames(t, dataStore, slow, store.NodeTagSourceAuto), "🐌较慢")
	// An unmeasured latency is unknown, and unknown matches no comparison — not
	// even "> 300ms", which is why the slow bucket does not swallow new nodes.
	assertNames(t, tagNames(t, dataStore, unmeasured, store.NodeTagSourceAuto))
	assertNames(t, projectionOf(t, dataStore, unmeasured))

	// The node crosses into another latency bucket: the old auto tag must go and
	// the manual one must stay.
	if err := dataStore.UpsertNodeStats(ctx, &store.NodeStats{
		NodeID: fast, LastLatencyMs: 500, Available: true, InitialCheckDone: true,
	}); err != nil {
		t.Fatalf("update stats: %v", err)
	}
	changed, err = service.Recompute(ctx, []int64{fast})
	if err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	assertTagIDs(t, changed, fast)
	assertNames(t, tagNames(t, dataStore, fast, store.NodeTagSourceAuto), "🐌较慢")
	assertNames(t, tagNames(t, dataStore, fast, store.NodeTagSourceManual), "game")
	assertNames(t, projectionOf(t, dataStore, fast), "game", "🐌较慢")
}

// TestRecomputeIsIdempotentAndScoped pins the two properties the incremental
// group rebuild depends on: an unchanged recompute reports no change, and a
// subset recompute does not rewrite rows outside the subset.
func TestRecomputeIsIdempotentAndScoped(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	first := newNode(t, dataStore, "first", 50)
	second := newNode(t, dataStore, "second", 400)

	service := NewService(dataStore)
	if _, err := service.SeedTemplates(ctx); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
	if _, err := service.RecomputeAll(ctx); err != nil {
		t.Fatalf("first RecomputeAll: %v", err)
	}
	changed, err := service.RecomputeAll(ctx)
	if err != nil {
		t.Fatalf("second RecomputeAll: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("a recompute over unchanged facts reported %v as changed", changed)
	}

	untouched, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{NodeIDs: []int64{second}})
	if err != nil {
		t.Fatalf("list node tags: %v", err)
	}
	if err := dataStore.UpsertNodeStats(ctx, &store.NodeStats{
		NodeID: first, LastLatencyMs: 400, Available: true, InitialCheckDone: true,
	}); err != nil {
		t.Fatalf("update stats: %v", err)
	}
	changed, err = service.Recompute(ctx, []int64{first})
	if err != nil {
		t.Fatalf("subset Recompute: %v", err)
	}
	assertTagIDs(t, changed, first)

	after, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{NodeIDs: []int64{second}})
	if err != nil {
		t.Fatalf("list node tags: %v", err)
	}
	if len(after) != len(untouched) {
		t.Fatalf("the other node has %d assignments, want %d", len(after), len(untouched))
	}
	for index := range after {
		if after[index].TagID != untouched[index].TagID ||
			!after[index].UpdatedAt.Equal(untouched[index].UpdatedAt) {
			t.Fatalf("a subset recompute rewrote another node's row: %+v", after[index])
		}
	}
	if changed, err := service.Recompute(ctx, []int64{}); err != nil || changed != nil {
		t.Fatalf("an empty selection must be a no-op, got %v / %v", changed, err)
	}
}

// TestRecomputeNotifiesOnlyVisibleChanges covers the case that separates "rows
// rewritten" from "membership changed": an auto row for a tag the node already
// carries by hand does not alter nodes.tags, and a group must not rebuild for it.
func TestRecomputeNotifiesOnlyVisibleChanges(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	node := newNode(t, dataStore, "fast", 50)

	var notified []int64
	service := NewService(dataStore, WithMembershipNotifier(func(nodeIDs []int64) {
		notified = append(notified, nodeIDs...)
	}))
	quick := newAutoTag(t, dataStore, "快", 0, 0,
		nodefacts.Condition{FieldName: nodefacts.FieldLatencyMs, Op: nodefacts.OpLte, Value: 100})
	if err := dataStore.SetManualNodeTags(ctx, node, []int64{quick}); err != nil {
		t.Fatalf("assign manual tag: %v", err)
	}

	changed, err := service.RecomputeAll(ctx)
	if err != nil {
		t.Fatalf("RecomputeAll: %v", err)
	}
	if len(changed) != 0 || len(notified) != 0 {
		t.Fatalf("an invisible auto row reported changed=%v notified=%v", changed, notified)
	}
	// The row is still written — the rule did match, and rule_version has to be
	// recorded so a later rule edit can be told apart.
	assertNames(t, tagNames(t, dataStore, node, store.NodeTagSourceAuto), "快")
	assertNames(t, projectionOf(t, dataStore, node), "快")

	newAutoTag(t, dataStore, "亚洲", 0, 0,
		nodefacts.Condition{FieldName: nodefacts.FieldNodeRegion, Op: nodefacts.OpEq, Value: "hk"})
	changed, err = service.RecomputeAll(ctx)
	if err != nil {
		t.Fatalf("RecomputeAll after adding a tag: %v", err)
	}
	assertTagIDs(t, changed, node)
	assertTagIDs(t, notified, node)
	assertNames(t, projectionOf(t, dataStore, node), "亚洲", "快")
}

// TestSeedTemplatesIsIdempotentAndSkipsNameConflicts covers the two ways a seed
// can be asked to create something that already exists.
func TestSeedTemplatesIsIdempotentAndSkipsNameConflicts(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	// An operator already maintains a tag under a builtin's name by hand.
	handMade := &store.Tag{Name: "原生IP", Description: "手工维护"}
	if err := dataStore.CreateTag(ctx, handMade); err != nil {
		t.Fatalf("create hand-made tag: %v", err)
	}
	service := NewService(dataStore, WithUnlockProviders(
		[]nodefacts.ProviderInfo{{Name: "Netflix", Label: "Netflix"}}))

	result, err := service.SeedTemplates(ctx)
	if err != nil {
		t.Fatalf("SeedTemplates: %v", err)
	}
	assertNames(t, result.Conflicts, "原生IP")
	if len(result.Skipped) != 0 {
		t.Fatalf("nothing was seeded before, got Skipped=%v", result.Skipped)
	}
	if len(result.Created) != 6 {
		t.Fatalf("Created=%v, want the six templates whose name is free", result.Created)
	}
	// The hand-made tag keeps its meaning: seeding must never turn an operator's
	// tag into a rule-driven one behind their back.
	kept, err := dataStore.GetTag(ctx, handMade.ID)
	if err != nil {
		t.Fatalf("get hand-made tag: %v", err)
	}
	if kept.AutoEnabled || kept.RuleJSON != "" || kept.BuiltinKey != "" {
		t.Fatalf("the hand-made tag was adopted by the seeder: %+v", kept)
	}

	// The provider name is lowercased into the field path while the label drives
	// the display name.
	unlockTag, err := dataStore.GetTagByName(ctx, "Netflix解锁")
	if err != nil || unlockTag == nil {
		t.Fatalf("the unlock template was not seeded: %v", err)
	}
	if !strings.Contains(unlockTag.RuleJSON, nodefacts.UnlockField("netflix", "status")) {
		t.Fatalf("unlock rule = %s", unlockTag.RuleJSON)
	}
	if unlockTag.BuiltinKey != UnlockTemplateKey("netflix") {
		t.Fatalf("builtin key = %q", unlockTag.BuiltinKey)
	}

	// Mutex groups are created on demand, once, and shared by their members.
	groups, err := dataStore.ListTagMutexGroups(ctx)
	if err != nil {
		t.Fatalf("list mutex groups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("want the two builtin groups, got %+v", groups)
	}
	fastTag := mustTag(t, dataStore, "⚡极速")
	slowTag := mustTag(t, dataStore, "🐌较慢")
	if fastTag.MutexGroupID == 0 || fastTag.MutexGroupID != slowTag.MutexGroupID {
		t.Fatalf("latency tags are not in one group: %d vs %d",
			fastTag.MutexGroupID, slowTag.MutexGroupID)
	}
	if fastTag.Priority <= slowTag.Priority {
		t.Fatalf("priority %d must outrank %d", fastTag.Priority, slowTag.Priority)
	}

	// A renamed builtin is recognised by its key, not its name, so a second seed
	// creates nothing.
	fastTag.Name = "运营改过的名字"
	if err := dataStore.UpdateTag(ctx, fastTag); err != nil {
		t.Fatalf("rename tag: %v", err)
	}
	again, err := service.SeedTemplates(ctx)
	if err != nil {
		t.Fatalf("second SeedTemplates: %v", err)
	}
	if len(again.Created) != 0 || len(again.Skipped) != 6 || len(again.Conflicts) != 1 {
		t.Fatalf("second seed was not idempotent: %+v", again)
	}
}

func mustTag(t *testing.T, dataStore store.Store, name string) *store.Tag {
	t.Helper()
	tag, err := dataStore.GetTagByName(context.Background(), name)
	if err != nil || tag == nil {
		t.Fatalf("tag %q missing: %v", name, err)
	}
	return tag
}

// TestPreviewCountsEveryNodeAndWritesNothing pins the three things a dry run has
// to get right: it counts every node while returning only a page of examples, it
// explains a non-match that comes from a missing fact, and it writes nothing.
func TestPreviewCountsEveryNodeAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	newNode(t, dataStore, "fast", 50)
	newNode(t, dataStore, "mid", 200)
	newNode(t, dataStore, "slow", 500)

	service := NewService(dataStore)
	if _, err := service.SeedTemplates(ctx); err != nil {
		t.Fatalf("seed templates: %v", err)
	}

	result, err := service.Preview(ctx, PreviewRequest{
		Condition: nodefacts.Condition{
			FieldName: nodefacts.FieldLatencyMs, Op: nodefacts.OpLte, Value: 300},
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if result.TotalNodes != 3 || result.MatchCount != 2 || result.AppliedCount != 2 {
		t.Fatalf("counts = %+v, want 3 nodes / 2 matched / 2 applied", result)
	}
	if result.ShadowedCount != 0 || result.UnknownCount != 0 {
		t.Fatalf("an ungrouped rule over measured nodes shadows nothing: %+v", result)
	}
	assertNames(t, result.Fields, nodefacts.FieldLatencyMs)
	// The limit caps the samples, not the counting above.
	if len(result.Samples) != 1 {
		t.Fatalf("Samples = %+v, want one", result.Samples)
	}
	if !result.Samples[0].Matched || result.Samples[0].Facts[nodefacts.FieldLatencyMs] != "50" {
		t.Fatalf("sample = %+v", result.Samples[0])
	}
	rows, err := dataStore.ListNodeTags(ctx, store.NodeTagFilter{})
	if err != nil {
		t.Fatalf("list node tags: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a preview wrote %d assignments", len(rows))
	}

	// A rule reading a fact nobody has measured matches nothing, and says so —
	// that count is what tells an operator the rule is fine but the data is not.
	unmeasured, err := service.Preview(ctx, PreviewRequest{
		Condition: nodefacts.Condition{
			FieldName: nodefacts.FieldUnlockIPPure, Op: nodefacts.OpEq, Value: true},
	})
	if err != nil {
		t.Fatalf("Preview over an unmeasured fact: %v", err)
	}
	if unmeasured.MatchCount != 0 || unmeasured.UnknownCount != 3 {
		t.Fatalf("counts = %+v, want 0 matched / 3 unknown", unmeasured)
	}
	if len(unmeasured.Samples) != 3 {
		t.Fatalf("a rule matching nothing must still show why: %+v", unmeasured.Samples)
	}

	// A draft joining an occupied mutex group reports the shadowing it would get
	// rather than an applied count it will never reach.
	group := mustTag(t, dataStore, "⚡极速").MutexGroupID
	drafted, err := service.Preview(ctx, PreviewRequest{
		Condition: nodefacts.Condition{
			FieldName: nodefacts.FieldLatencyMs, Op: nodefacts.OpLte, Value: 1000},
		MutexGroupID: group,
		Priority:     5,
	})
	if err != nil {
		t.Fatalf("Preview of a grouped draft: %v", err)
	}
	if drafted.MatchCount != 3 || drafted.AppliedCount != 0 || drafted.ShadowedCount != 3 {
		t.Fatalf("counts = %+v, want 3 matched / 0 applied / 3 shadowed", drafted)
	}
	note := drafted.Samples[0].Shadowed
	if note == nil || note.Reason != ReasonLowerPriority || note.WinnerTagName != "⚡极速" {
		t.Fatalf("shadow note = %+v", note)
	}
}

// TestRecomputeSurvivesBrokenRules covers the isolation guarantee: one tag whose
// rule cannot be parsed and one whose rule cannot be evaluated must not stop the
// other tags from being applied.
func TestRecomputeSurvivesBrokenRules(t *testing.T) {
	ctx := context.Background()
	dataStore := openTagStore(t)
	node := newNode(t, dataStore, "fast", 50)

	unparseable := &store.Tag{Name: "坏JSON", AutoEnabled: true, RuleJSON: "{"}
	if err := dataStore.CreateTag(ctx, unparseable); err != nil {
		t.Fatalf("create unparseable tag: %v", err)
	}
	// This one parses. It only fails once a node is measured, because comparing an
	// int fact against text is a mistake nothing can catch earlier.
	unevaluable := &store.Tag{Name: "坏类型", AutoEnabled: true,
		RuleJSON: `{"field":"latency_ms","op":"lte","value":"很快"}`}
	if err := dataStore.CreateTag(ctx, unevaluable); err != nil {
		t.Fatalf("create unevaluable tag: %v", err)
	}
	newAutoTag(t, dataStore, "快", 0, 0,
		nodefacts.Condition{FieldName: nodefacts.FieldLatencyMs, Op: nodefacts.OpLte, Value: 100})

	var logged []string
	service := NewService(dataStore, WithLogf(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}))
	changed, err := service.RecomputeAll(ctx)
	if err != nil {
		t.Fatalf("RecomputeAll: %v", err)
	}
	assertTagIDs(t, changed, node)
	assertNames(t, tagNames(t, dataStore, node, store.NodeTagSourceAuto), "快")
	// Once per broken rule, not once per node.
	if len(logged) != 2 {
		t.Fatalf("logged %d lines, want one per broken rule: %v", len(logged), logged)
	}
	changed, err = service.RecomputeAll(ctx)
	if err != nil || len(changed) != 0 {
		t.Fatalf("a second recompute reported %v / %v", changed, err)
	}
}
