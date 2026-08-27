package nodetag

import (
	"fmt"
	"math/rand"
	"testing"
)

// Two mutex groups, matching the builtin templates' shape: one an operator also
// assigns by hand, one they do not.
const (
	groupLatency = int64(11)
	groupRisk    = int64(22)
)

func rule(tagID int64, name string, groupID int64, priority, version int) Rule {
	return Rule{
		TagMeta:     TagMeta{TagID: tagID, Name: name, MutexGroupID: groupID, Priority: priority},
		RuleVersion: version,
	}
}

// metaOf builds the tag metadata the resolver reads. Extra entries stand for tags
// that carry no rule, which is how a manual-only tag reaches the resolver.
func metaOf(entries ...TagMeta) map[int64]TagMeta {
	meta := make(map[int64]TagMeta, len(entries))
	for _, entry := range entries {
		meta[entry.TagID] = entry
	}
	return meta
}

func metaFrom(rules ...Rule) map[int64]TagMeta {
	entries := make([]TagMeta, 0, len(rules))
	for _, one := range rules {
		entries = append(entries, one.TagMeta)
	}
	return metaOf(entries...)
}

func assertTagIDs(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tag IDs = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("tag IDs = %v, want %v", got, want)
		}
	}
}

func TestResolveAppliesEveryUngroupedMatch(t *testing.T) {
	rules := []Rule{
		rule(7, "netflix解锁", 0, 0, 3),
		rule(2, "原生IP", 0, 0, 1),
		rule(5, "游戏专线", 0, 0, 9),
	}
	decision := Resolve(42, rules, nil, metaFrom(rules...))
	if decision.NodeID != 42 {
		t.Fatalf("NodeID = %d, want 42", decision.NodeID)
	}
	assertTagIDs(t, decision.TagIDs, 2, 5, 7)
	// Rule versions travel with their tag through the sort, not with their input
	// position: tag 2 keeps version 1, tag 5 keeps 9, tag 7 keeps 3.
	wantVersions := []int{1, 9, 3}
	for index, version := range wantVersions {
		if decision.RuleVersions[index] != version {
			t.Fatalf("RuleVersions = %v, want %v", decision.RuleVersions, wantVersions)
		}
	}
	if len(decision.Shadowed) != 0 {
		t.Fatalf("ungrouped tags cannot shadow each other, got %+v", decision.Shadowed)
	}
}

func TestResolveKeepsTheHighestPriorityInAGroup(t *testing.T) {
	fast := rule(3, "⚡极速", groupLatency, 30, 1)
	normal := rule(4, "✅正常", groupLatency, 20, 1)
	slow := rule(5, "🐌较慢", groupLatency, 10, 1)
	native := rule(9, "原生IP", 0, 0, 1)

	// A node under 100ms matches all three latency rules; exactly one may apply.
	decision := Resolve(1, []Rule{slow, normal, fast, native},
		nil, metaFrom(fast, normal, slow, native))
	assertTagIDs(t, decision.TagIDs, 3, 9)
	if len(decision.Shadowed) != 2 {
		t.Fatalf("want the two losing latency tags shadowed, got %+v", decision.Shadowed)
	}
	for _, note := range decision.Shadowed {
		if note.Reason != ReasonLowerPriority {
			t.Fatalf("reason = %q, want %q", note.Reason, ReasonLowerPriority)
		}
		if note.WinnerTagID != 3 || note.WinnerTagName != "⚡极速" {
			t.Fatalf("winner = %d/%q, want 3/⚡极速", note.WinnerTagID, note.WinnerTagName)
		}
		if note.MutexGroupID != groupLatency {
			t.Fatalf("MutexGroupID = %d, want %d", note.MutexGroupID, groupLatency)
		}
	}
	if decision.Shadowed[0].TagID != 4 || decision.Shadowed[1].TagID != 5 {
		t.Fatalf("shadow notes are not sorted by tag ID: %+v", decision.Shadowed)
	}
}

func TestResolveBreaksPriorityTiesOnTheLowerTagID(t *testing.T) {
	later := rule(80, "后建的标签", groupRisk, 10, 1)
	earlier := rule(8, "先建的标签", groupRisk, 10, 1)
	decision := Resolve(1, []Rule{later, earlier}, nil, metaFrom(later, earlier))
	assertTagIDs(t, decision.TagIDs, 8)
	if len(decision.Shadowed) != 1 || decision.Shadowed[0].TagID != 80 {
		t.Fatalf("the higher tag ID must lose the tie, got %+v", decision.Shadowed)
	}
}

// TestResolveManualAssignmentBlocksTheWholeGroup is the operator-intent
// guarantee: a hand-placed tag in a mutex group outranks every rule in it, so a
// recompute cannot flip the node onto a different tag in that group.
func TestResolveManualAssignmentBlocksTheWholeGroup(t *testing.T) {
	fast := rule(3, "⚡极速", groupLatency, 30, 1)
	slow := rule(5, "🐌较慢", groupLatency, 10, 1)
	native := rule(9, "原生IP", 0, 0, 1)
	manual := TagMeta{TagID: 6, Name: "人工核定档", MutexGroupID: groupLatency}

	meta := metaFrom(fast, slow, native)
	meta[manual.TagID] = manual
	decision := Resolve(1, []Rule{fast, slow, native}, []int64{manual.TagID}, meta)

	assertTagIDs(t, decision.TagIDs, 9)
	if len(decision.Shadowed) != 2 {
		t.Fatalf("both latency rules must be shadowed, got %+v", decision.Shadowed)
	}
	for _, note := range decision.Shadowed {
		if note.Reason != ReasonManualOccupiesGroup {
			t.Fatalf("reason = %q, want %q", note.Reason, ReasonManualOccupiesGroup)
		}
		if note.WinnerTagID != manual.TagID || note.WinnerTagName != manual.Name {
			t.Fatalf("winner = %d/%q, want the manual tag", note.WinnerTagID, note.WinnerTagName)
		}
	}
}

func TestResolveIgnoresManualTagsThatCannotOccupyAGroup(t *testing.T) {
	fast := rule(3, "⚡极速", groupLatency, 30, 1)
	free := TagMeta{TagID: 6, Name: "vip"} // manual, but in no mutex group
	meta := metaFrom(fast)
	meta[free.TagID] = free

	// A manual tag outside the group, and a manual ID for a tag that no longer
	// exists, must both leave the group open.
	decision := Resolve(1, []Rule{fast}, []int64{free.TagID, 999}, meta)
	assertTagIDs(t, decision.TagIDs, 3)
	if len(decision.Shadowed) != 0 {
		t.Fatalf("nothing should be shadowed, got %+v", decision.Shadowed)
	}
}

// TestResolveIsDeterministic pins the property the whole recompute rests on:
// the same facts must produce byte-identical assignments however the matched
// rules arrive, or every recompute would look like a change and rebuild groups.
func TestResolveIsDeterministic(t *testing.T) {
	rules := []Rule{
		rule(3, "⚡极速", groupLatency, 30, 1),
		rule(4, "✅正常", groupLatency, 20, 2),
		rule(5, "🐌较慢", groupLatency, 10, 3),
		rule(6, "高风险", groupRisk, 20, 4),
		rule(7, "低风险", groupRisk, 20, 5),
		rule(8, "原生IP", 0, 0, 6),
		rule(9, "netflix解锁", 0, 0, 7),
	}
	meta := metaFrom(rules...)
	manual := []int64{8}
	want := fmt.Sprintf("%+v", Resolve(1, rules, manual, meta))

	shuffler := rand.New(rand.NewSource(7))
	for attempt := 0; attempt < 50; attempt++ {
		shuffled := make([]Rule, len(rules))
		copy(shuffled, rules)
		shuffler.Shuffle(len(shuffled), func(first, second int) {
			shuffled[first], shuffled[second] = shuffled[second], shuffled[first]
		})
		got := fmt.Sprintf("%+v", Resolve(1, shuffled, manual, meta))
		if got != want {
			t.Fatalf("attempt %d changed the decision:\n want %s\n got  %s", attempt, want, got)
		}
	}
}

func TestResolveWithoutMatchesProducesAnEmptyAssignment(t *testing.T) {
	decision := Resolve(77, nil, []int64{1}, metaOf(TagMeta{TagID: 1, Name: "vip"}))
	if len(decision.TagIDs) != 0 || len(decision.RuleVersions) != 0 {
		t.Fatalf("want no auto tags, got %+v", decision)
	}
	assignment := decision.Assignment()
	if assignment.NodeID != 77 || len(assignment.TagIDs) != 0 {
		t.Fatalf("Assignment = %+v, want an empty assignment for node 77", assignment)
	}
}
