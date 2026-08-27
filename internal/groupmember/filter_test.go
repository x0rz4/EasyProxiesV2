package groupmember

import (
	"reflect"
	"strings"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
)

// legacyBuilderMembers is a verbatim copy of the membership block that used to
// live in internal/builder/builder.go, kept here as the reference the refactor is
// measured against. Note that builder fed it an already-normalized region: an
// unclassified node arrived as geoip.RegionOther, never as "".
func legacyBuilderMembers(group config.GroupPoolConfig, nodes []config.NodeConfig) []int64 {
	regionSet := make(map[string]struct{}, len(group.Regions))
	for _, region := range group.Regions {
		regionSet[strings.ToLower(strings.TrimSpace(region))] = struct{}{}
	}
	explicitSet := make(map[int64]struct{}, len(group.ExplicitNodeIDs))
	for _, nodeID := range group.ExplicitNodeIDs {
		explicitSet[nodeID] = struct{}{}
	}
	excludedSet := make(map[int64]struct{}, len(group.ExcludedNodeIDs))
	for _, nodeID := range group.ExcludedNodeIDs {
		excludedSet[nodeID] = struct{}{}
	}
	members := make([]int64, 0)
	for _, node := range nodes {
		region := strings.ToLower(node.Region)
		if region == "" {
			region = geoip.RegionOther
		}
		if _, excluded := excludedSet[node.ID]; excluded {
			continue
		}
		_, explicit := explicitSet[node.ID]
		_, regional := regionSet[region]
		if !explicit && !regional {
			continue
		}
		members = append(members, node.ID)
	}
	return members
}

// legacyBoxmgrMembers is a verbatim copy of the old boxmgr groupMemberNodes body.
// It is the side that disagreed with the builder: an unclassified node keeps an
// empty region here instead of falling into "other".
func legacyBoxmgrMembers(cfg *config.Config, groupCfg config.GroupPoolConfig) []config.NodeConfig {
	if cfg == nil || !groupCfg.Enabled {
		return nil
	}
	regions := make(map[string]struct{}, len(groupCfg.Regions))
	for _, region := range groupCfg.Regions {
		regions[strings.ToLower(strings.TrimSpace(region))] = struct{}{}
	}
	explicit := make(map[int64]struct{}, len(groupCfg.ExplicitNodeIDs))
	for _, nodeID := range groupCfg.ExplicitNodeIDs {
		explicit[nodeID] = struct{}{}
	}
	excluded := make(map[int64]struct{}, len(groupCfg.ExcludedNodeIDs))
	for _, nodeID := range groupCfg.ExcludedNodeIDs {
		excluded[nodeID] = struct{}{}
	}
	result := make([]config.NodeConfig, 0)
	for _, node := range cfg.Nodes {
		if _, skip := excluded[node.ID]; skip {
			continue
		}
		_, manuallyIncluded := explicit[node.ID]
		_, regionIncluded := regions[strings.ToLower(strings.TrimSpace(node.Region))]
		if manuallyIncluded || regionIncluded {
			result = append(result, node)
		}
	}
	return result
}

// equivalenceNodes is the node set every case below is evaluated against. The
// regions are in the shapes the store actually produces: geoip codes, and the
// empty string for a node whose landing IP has not been classified yet.
var equivalenceNodes = []config.NodeConfig{
	{ID: 1, Name: "hk-01", Region: "hk"},
	{ID: 2, Name: "hk-02", Region: "HK"},
	{ID: 3, Name: "jp-01", Region: "jp"},
	{ID: 4, Name: "unclassified", Region: ""},
	{ID: 5, Name: "tw-01", Region: "tw"},
	{ID: 6, Name: "other", Region: "other"},
}

var equivalenceGroups = []struct {
	name  string
	group config.GroupPoolConfig
}{
	{"single region", config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"}}},
	{"mixed-case region", config.GroupPoolConfig{Enabled: true, Regions: []string{"HK", " Jp "}}},
	{"no regions", config.GroupPoolConfig{Enabled: true}},
	{"explicit only", config.GroupPoolConfig{Enabled: true, ExplicitNodeIDs: []int64{3, 4}}},
	{"explicit outside region", config.GroupPoolConfig{Enabled: true,
		Regions: []string{"hk"}, ExplicitNodeIDs: []int64{3}}},
	{"excluded beats region", config.GroupPoolConfig{Enabled: true,
		Regions: []string{"hk"}, ExcludedNodeIDs: []int64{2}}},
	{"excluded beats explicit", config.GroupPoolConfig{Enabled: true,
		Regions: []string{"hk"}, ExplicitNodeIDs: []int64{3}, ExcludedNodeIDs: []int64{3}}},
	{"unknown region", config.GroupPoolConfig{Enabled: true, Regions: []string{"sg"}}},
	{"disabled", config.GroupPoolConfig{Regions: []string{"hk"}}},
	{"everything at once", config.GroupPoolConfig{Enabled: true,
		Regions: []string{"hk", "jp", "tw"}, ExplicitNodeIDs: []int64{6}, ExcludedNodeIDs: []int64{1}}},
}

func allowedIDs(group config.GroupPoolConfig, nodes []config.NodeConfig) []int64 {
	filter := NewFilter(group)
	ids := make([]int64, 0)
	for _, node := range nodes {
		if filter.Allow(Node{ID: node.ID, Region: node.Region}) {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

func nodeIDs(nodes []config.NodeConfig) []int64 {
	ids := make([]int64, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

// TestFilterMatchesTheBuilderItReplaces is the gate on this refactor: the builder
// is the side whose behaviour must not move, because it decides what the running
// box actually contains.
func TestFilterMatchesTheBuilderItReplaces(t *testing.T) {
	for _, testCase := range equivalenceGroups {
		want := legacyBuilderMembers(testCase.group, equivalenceNodes)
		if !testCase.group.Enabled {
			// The builder skipped disabled groups before reaching this block, so
			// its copy has no opinion there; Allow answers "no members".
			want = []int64{}
		}
		got := allowedIDs(testCase.group, equivalenceNodes)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: members = %v, want %v", testCase.name, got, want)
		}
	}
}

// TestNodesMatchesBoxmgrExceptForUnclassifiedNodes records the one behaviour that
// does move. boxmgr compared members with an empty region literally, so a group
// selecting "other" could gain or lose an unclassified node without the topology
// comparison noticing — the builder would put it in the box either way. Aligning
// on the builder's answer is the point of the change.
func TestNodesMatchesBoxmgrExceptForUnclassifiedNodes(t *testing.T) {
	cfg := &config.Config{Nodes: equivalenceNodes}
	for _, testCase := range equivalenceGroups {
		legacy := legacyBoxmgrMembers(cfg, testCase.group)
		got := Nodes(cfg, testCase.group)
		selectsOther := false
		for _, region := range testCase.group.Regions {
			if NormalizeRegion(region) == geoip.RegionOther {
				selectsOther = true
			}
		}
		if !selectsOther {
			if !reflect.DeepEqual(got, legacy) {
				t.Fatalf("%s: members = %v, want %v", testCase.name, nodeIDs(got), nodeIDs(legacy))
			}
			continue
		}
		// The divergence is exactly the unclassified node, and only in the
		// direction of including it.
		if !reflect.DeepEqual(nodeIDs(got), []int64{4, 6}) || !reflect.DeepEqual(nodeIDs(legacy), []int64{6}) {
			t.Fatalf("%s: new = %v, legacy = %v, want the unclassified node added",
				testCase.name, nodeIDs(got), nodeIDs(legacy))
		}
	}

	// The "other" case has to actually be exercised, otherwise the loop above
	// proves nothing about it.
	other := config.GroupPoolConfig{Enabled: true, Regions: []string{"other"}}
	if got := nodeIDs(Nodes(cfg, other)); !reflect.DeepEqual(got, []int64{4, 6}) {
		t.Fatalf("members of an \"other\" group = %v", got)
	}
}

// TestNodesKeepsConfigOrderAndDistinguishesEmptyFromNil pins the two properties
// the callers rely on but no membership rule expresses: boxmgr compares two
// results with reflect.DeepEqual, where nil and empty differ, and the builder
// takes members[0] as the group's default outbound.
func TestNodesKeepsConfigOrderAndDistinguishesEmptyFromNil(t *testing.T) {
	cfg := &config.Config{Nodes: equivalenceNodes}
	if members := Nodes(cfg, config.GroupPoolConfig{Regions: []string{"hk"}}); members != nil {
		t.Fatalf("a disabled group returned %v, want nil", nodeIDs(members))
	}
	if members := Nodes(nil, config.GroupPoolConfig{Enabled: true}); members != nil {
		t.Fatalf("a nil config returned %v, want nil", nodeIDs(members))
	}
	empty := Nodes(cfg, config.GroupPoolConfig{Enabled: true, Regions: []string{"sg"}})
	if empty == nil || len(empty) != 0 {
		t.Fatalf("a group matching nothing returned %v, want an empty non-nil slice", empty)
	}
	if reflect.DeepEqual(empty, Nodes(cfg, config.GroupPoolConfig{Regions: []string{"sg"}})) {
		t.Fatal("an enabled group with no members must not compare equal to a disabled one")
	}

	// Membership never reorders: the group is a subsequence of cfg.Nodes.
	reversed := &config.Config{Nodes: []config.NodeConfig{
		{ID: 3, Region: "hk"}, {ID: 2, Region: "hk"}, {ID: 1, Region: "hk"},
	}}
	got := nodeIDs(Nodes(reversed, config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"}}))
	if !reflect.DeepEqual(got, []int64{3, 2, 1}) {
		t.Fatalf("members = %v, want cfg.Nodes order", got)
	}
}

// TestAllowOrdersItsRules states the decision order as its own case rather than
// leaving it implied by the equivalence tables.
func TestAllowOrdersItsRules(t *testing.T) {
	filter := NewFilter(config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"},
		ExplicitNodeIDs: []int64{2, 3}, ExcludedNodeIDs: []int64{3, 4}})
	for _, testCase := range []struct {
		name string
		node Node
		want bool
	}{
		{"region member", Node{ID: 1, Region: "hk"}, true},
		{"explicit outside the region", Node{ID: 2, Region: "jp"}, true},
		{"excluded outranks explicit", Node{ID: 3, Region: "hk"}, false},
		{"excluded outranks region", Node{ID: 4, Region: "hk"}, false},
		{"wrong region", Node{ID: 5, Region: "jp"}, false},
		{"padded and mixed case region", Node{ID: 6, Region: " Hk "}, true},
		{"unclassified is not in hk", Node{ID: 7, Region: ""}, false},
	} {
		if got := filter.Allow(testCase.node); got != testCase.want {
			t.Fatalf("%s: Allow(%+v) = %v", testCase.name, testCase.node, got)
		}
	}

	// A group with no regions at all admits only its explicit members, and the
	// zero Filter — a group nobody built rules for — admits nobody.
	explicitOnly := NewFilter(config.GroupPoolConfig{Enabled: true, ExplicitNodeIDs: []int64{1}})
	if !explicitOnly.Allow(Node{ID: 1, Region: "hk"}) || explicitOnly.Allow(Node{ID: 2, Region: "hk"}) {
		t.Fatal("a region-less group must admit exactly its explicit members")
	}
	if (Filter{}).Allow(Node{ID: 1, Region: "hk"}) {
		t.Fatal("the zero Filter admitted a node")
	}
}

// TestFilterTrimsRegionsNeitherCallerAgreedOn covers the input the two old copies
// answered differently: builder lowercased a node's region without trimming it
// while boxmgr trimmed it, so a padded region was in the box but absent from the
// topology comparison. Normalizing both sides is what removes that class of
// disagreement; the store has never written such a region, which is why the
// equivalence tables above do not carry one.
func TestFilterTrimsRegionsNeitherCallerAgreedOn(t *testing.T) {
	padded := []config.NodeConfig{{ID: 1, Region: " tw "}}
	group := config.GroupPoolConfig{Enabled: true, Regions: []string{"tw"}}
	if got := allowedIDs(group, padded); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("members = %v, want the padded node", got)
	}
	if legacy := legacyBuilderMembers(group, padded); len(legacy) != 0 {
		t.Fatalf("the legacy builder is expected to have missed it, got %v", legacy)
	}
	if legacy := legacyBoxmgrMembers(&config.Config{Nodes: padded}, group); len(legacy) != 1 {
		t.Fatalf("the legacy boxmgr is expected to have matched it, got %v", nodeIDs(legacy))
	}
}

func TestNormalizeRegion(t *testing.T) {
	for input, want := range map[string]string{
		"hk": "hk", "HK": "hk", " Hk ": "hk", "": geoip.RegionOther,
		"   ": geoip.RegionOther, "other": geoip.RegionOther, "SG": "sg",
	} {
		if got := NormalizeRegion(input); got != want {
			t.Fatalf("NormalizeRegion(%q) = %q, want %q", input, got, want)
		}
	}
}

// tagNames is the ID → name mapping a group's tag filter is resolved through.
// Groups store tag IDs so that renaming a tag cannot change who is in the group,
// while nodes carry tag names, so the two have to be joined somewhere.
var tagNames = map[int64]string{10: "原生IP", 11: "Netflix解锁", 12: "⚡极速", 13: "game"}

// taggedNodes covers the combinations the whitelist modes have to distinguish:
// both tags, one tag, the other tag, and none.
var taggedNodes = []config.NodeConfig{
	{ID: 1, Region: "hk", Tags: []string{"原生IP", "Netflix解锁"}},
	{ID: 2, Region: "hk", Tags: []string{"原生IP"}},
	{ID: 3, Region: "hk", Tags: []string{"Netflix解锁", "game"}},
	{ID: 4, Region: "hk"},
	{ID: 5, Region: "jp", Tags: []string{"原生IP", "Netflix解锁"}},
}

func taggedIDs(group config.GroupPoolConfig, opts ...Option) []int64 {
	filter := NewFilter(group, opts...)
	ids := make([]int64, 0)
	for _, node := range taggedNodes {
		if filter.Allow(Node{ID: node.ID, Region: node.Region, Tags: node.Tags}) {
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// TestTagWhitelistMatchModes pins the difference between the two match modes,
// which is the whole reason tag_filter_match exists: "any" is a union ("香港里
// 任何一种增值节点"), "all" is an intersection ("既是原生IP又解锁 Netflix").
func TestTagWhitelistMatchModes(t *testing.T) {
	hk := config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"}}
	for _, testCase := range []struct {
		name  string
		group config.GroupPoolConfig
		want  []int64
	}{
		{"no tag filter admits every regional node", hk, []int64{1, 2, 3, 4}},
		{"any of two tags", withTags(hk, []int64{10, 11}, nil, "any"), []int64{1, 2, 3}},
		{"all of two tags", withTags(hk, []int64{10, 11}, nil, "all"), []int64{1}},
		{"an empty match mode defaults to any", withTags(hk, []int64{10, 11}, nil, ""), []int64{1, 2, 3}},
		{"a single-tag whitelist reads the same either way",
			withTags(hk, []int64{10}, nil, "all"), []int64{1, 2}},
		{"blacklist removes a regional member", withTags(hk, nil, []int64{13}, ""), []int64{1, 2, 4}},
		{"blacklist outranks the whitelist",
			withTags(hk, []int64{11}, []int64{13}, "any"), []int64{1}},
		{"the tag filter never widens the region",
			withTags(hk, []int64{10}, nil, "any"), []int64{1, 2}},
	} {
		if got := taggedIDs(testCase.group, WithTagNames(tagNames)); !reflect.DeepEqual(got, testCase.want) {
			t.Fatalf("%s: members = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

func withTags(group config.GroupPoolConfig, whitelist, blacklist []int64, match string) config.GroupPoolConfig {
	group.TagWhitelist = whitelist
	group.TagBlacklist = blacklist
	group.TagFilterMatch = match
	return group
}

// TestExplicitNodesBypassTheTagFilter states the escape hatch. explicit_node_ids
// means "this node regardless of the rules"; if the tag blacklist outranked it,
// an operator would have no way to make an exception without editing the tags of
// the node itself.
func TestExplicitNodesBypassTheTagFilter(t *testing.T) {
	// Node 5 is in jp, carries no whitelisted tag requirement it could satisfy
	// by region, and node 3 carries a blacklisted tag. Both are explicit.
	group := withTags(config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"},
		ExplicitNodeIDs: []int64{3, 5}}, []int64{10}, []int64{13}, "all")
	if got := taggedIDs(group, WithTagNames(tagNames)); !reflect.DeepEqual(got, []int64{1, 2, 3, 5}) {
		t.Fatalf("members = %v, want the explicit nodes admitted alongside the tag matches", got)
	}

	// excluded still outranks everything, including an explicit tag match.
	group.ExcludedNodeIDs = []int64{1, 5}
	if got := taggedIDs(group, WithTagNames(tagNames)); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("members = %v, want the excluded nodes gone", got)
	}
}

// TestUnresolvedTagIDsFailClosed is the safety property. A group that requires
// 原生IP must never admit every node just because the name mapping was missing
// or the tag was deleted — the group narrows instead of silently widening.
func TestUnresolvedTagIDsFailClosed(t *testing.T) {
	hk := config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"}}
	unknown := withTags(hk, []int64{999}, nil, "any")
	if got := taggedIDs(unknown, WithTagNames(tagNames)); len(got) != 0 {
		t.Fatalf("an unresolvable whitelist admitted %v, want nobody", got)
	}
	if got := taggedIDs(unknown); len(got) != 0 {
		t.Fatalf("a whitelist with no name mapping at all admitted %v, want nobody", got)
	}
	// One resolvable entry out of two still narrows under "all" and still works
	// under "any": the unknown entry can simply never be matched.
	if got := taggedIDs(withTags(hk, []int64{10, 999}, nil, "all"), WithTagNames(tagNames)); len(got) != 0 {
		t.Fatalf("an all-match with an unresolvable entry admitted %v, want nobody", got)
	}
	if got := taggedIDs(withTags(hk, []int64{10, 999}, nil, "any"),
		WithTagNames(tagNames)); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("an any-match = %v, want the nodes carrying the resolvable tag", got)
	}
	// An unresolvable blacklist entry must not exclude anybody, and in
	// particular must not collide with a node carrying no tags at all.
	if got := taggedIDs(withTags(hk, nil, []int64{999}, ""),
		WithTagNames(tagNames)); !reflect.DeepEqual(got, []int64{1, 2, 3, 4}) {
		t.Fatalf("an unresolvable blacklist excluded somebody: %v", got)
	}
}

// TestNodesAppliesTheTagFilter checks the slice-returning entry point boxmgr
// compares topologies with, including that it still tells "disabled" apart from
// "enabled but the tag filter matched nothing".
func TestNodesAppliesTheTagFilter(t *testing.T) {
	cfg := &config.Config{Nodes: taggedNodes}
	group := withTags(config.GroupPoolConfig{Enabled: true, Regions: []string{"hk"}},
		[]int64{10, 11}, nil, "all")
	if got := nodeIDs(Nodes(cfg, group, WithTagNames(tagNames))); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("members = %v, want only the node carrying both tags", got)
	}
	if got := Nodes(cfg, withTags(group, []int64{12}, nil, "all"), WithTagNames(tagNames)); got == nil || len(got) != 0 {
		t.Fatalf("a group whose tag filter matched nothing returned %v, want an empty non-nil slice", got)
	}
	group.Enabled = false
	if got := Nodes(cfg, group, WithTagNames(tagNames)); got != nil {
		t.Fatalf("a disabled group returned %v, want nil", nodeIDs(got))
	}
}
