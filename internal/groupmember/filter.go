// Package groupmember decides which nodes belong to a group pool.
//
// It exists because that decision used to be written out twice: once in
// internal/builder while assembling a group's outbounds, and once in
// internal/boxmgr while deciding whether a reload has to rebuild a group's box.
// Two copies of the same set arithmetic had already drifted — builder folded an
// unclassified node into the "other" region while boxmgr left it region-less, so
// a group selecting "other" could gain or lose a member without boxmgr noticing
// it had to rebuild. Every membership question now goes through Filter.Allow.
package groupmember

import (
	"strconv"
	"strings"

	"easy_proxies/internal/config"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/store"
)

// Node is the view of a node that a membership decision needs. Callers hold the
// node in different shapes — builder has a poolout.MemberMeta, boxmgr has a
// config.NodeConfig — so the decision takes the few fields both can produce.
type Node struct {
	ID     int64
	Region string
	// Tags is the node's projected tag set (manual ∪ auto names), in any order.
	Tags []string
}

// Filter answers membership for one group pool. The zero value allows nothing;
// build one with NewFilter.
type Filter struct {
	enabled  bool
	regions  map[string]struct{}
	explicit map[int64]struct{}
	excluded map[int64]struct{}
	// whitelist and blacklist hold tag names resolved from the group's tag IDs.
	// An ID with no known name resolves to a sentinel no node can carry, so a
	// stale reference narrows the group instead of quietly widening it.
	whitelist []string
	blacklist map[string]struct{}
	matchAll  bool
}

// Option adjusts how a Filter reads its group config.
type Option func(*filterOptions)

type filterOptions struct {
	tagNames map[int64]string
}

// WithTagNames supplies the tag ID → name mapping the tag whitelist and
// blacklist are resolved through. Without it a group that filters by tag matches
// nothing, which is the safe direction: the alternative is a group silently
// dropping its tag requirement.
func WithTagNames(tagNames map[int64]string) Option {
	return func(opts *filterOptions) { opts.tagNames = tagNames }
}

// NewFilter snapshots one group's membership rules. The group config is not
// retained, so a Filter stays valid while the caller iterates.
func NewFilter(groupCfg config.GroupPoolConfig, opts ...Option) Filter {
	var options filterOptions
	for _, apply := range opts {
		apply(&options)
	}
	filter := Filter{
		enabled:  groupCfg.Enabled,
		regions:  make(map[string]struct{}, len(groupCfg.Regions)),
		explicit: make(map[int64]struct{}, len(groupCfg.ExplicitNodeIDs)),
		excluded: make(map[int64]struct{}, len(groupCfg.ExcludedNodeIDs)),
		matchAll: strings.EqualFold(strings.TrimSpace(groupCfg.TagFilterMatch), store.TagFilterMatchAll),
	}
	for _, region := range groupCfg.Regions {
		filter.regions[NormalizeRegion(region)] = struct{}{}
	}
	for _, nodeID := range groupCfg.ExplicitNodeIDs {
		filter.explicit[nodeID] = struct{}{}
	}
	for _, nodeID := range groupCfg.ExcludedNodeIDs {
		filter.excluded[nodeID] = struct{}{}
	}
	for _, tagID := range groupCfg.TagWhitelist {
		filter.whitelist = append(filter.whitelist, resolveTagName(options.tagNames, tagID))
	}
	if len(groupCfg.TagBlacklist) > 0 {
		filter.blacklist = make(map[string]struct{}, len(groupCfg.TagBlacklist))
		for _, tagID := range groupCfg.TagBlacklist {
			filter.blacklist[resolveTagName(options.tagNames, tagID)] = struct{}{}
		}
	}
	return filter
}

// unresolvedTagPrefix names a tag ID whose name is unknown. It contains a NUL
// byte, which a tag name cannot, so such an entry never matches a node.
const unresolvedTagPrefix = "\x00unresolved-tag-"

func resolveTagName(tagNames map[int64]string, tagID int64) string {
	if name := tagNames[tagID]; name != "" {
		return name
	}
	return unresolvedTagPrefix + strconv.FormatInt(tagID, 10)
}

// Allow reports whether the node belongs to the group. The order is the contract
// callers depend on:
//
//  1. a disabled group has no members at all;
//  2. excluded_node_ids removes a node unconditionally — it is the operator's
//     override and nothing may override it back;
//  3. explicit_node_ids adds a node whatever its region and tags. It is the
//     escape hatch for "this one node regardless of the rules", so letting the
//     tag blacklist outrank it would leave operators no way to make exceptions;
//  4. otherwise the node's region has to be one the group selects, it must carry
//     the whitelisted tags (any or all of them), and none of the blacklisted
//     ones.
func (f Filter) Allow(node Node) bool {
	if !f.enabled {
		return false
	}
	if _, excluded := f.excluded[node.ID]; excluded {
		return false
	}
	if _, explicit := f.explicit[node.ID]; explicit {
		return true
	}
	if _, regional := f.regions[NormalizeRegion(node.Region)]; !regional {
		return false
	}
	return f.tagsAllow(node.Tags)
}

func (f Filter) tagsAllow(tags []string) bool {
	if len(f.whitelist) == 0 && len(f.blacklist) == 0 {
		return true
	}
	carried := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		carried[tag] = struct{}{}
	}
	for tag := range f.blacklist {
		if _, found := carried[tag]; found {
			return false
		}
	}
	if len(f.whitelist) == 0 {
		return true
	}
	matched := 0
	for _, tag := range f.whitelist {
		if _, found := carried[tag]; found {
			matched++
		}
	}
	if f.matchAll {
		return matched == len(f.whitelist)
	}
	return matched > 0
}

// Nodes returns the group's members in cfg.Nodes order.
//
// The empty-but-not-nil result for a group that matches nothing is deliberate:
// boxmgr compares two of these with reflect.DeepEqual, where a nil slice and an
// empty one are not equal, so "disabled" and "enabled with no members" have to
// stay distinguishable.
func Nodes(cfg *config.Config, groupCfg config.GroupPoolConfig, opts ...Option) []config.NodeConfig {
	if cfg == nil || !groupCfg.Enabled {
		return nil
	}
	filter := NewFilter(groupCfg, opts...)
	members := make([]config.NodeConfig, 0)
	for _, node := range cfg.Nodes {
		if filter.Allow(Node{ID: node.ID, Region: node.Region, Tags: node.Tags}) {
			members = append(members, node)
		}
	}
	return members
}

// NormalizeRegion puts a region code into the one form membership compares.
// A node whose landing IP has not been classified yet has no region, and both
// sides of the comparison have to agree that this means geoip.RegionOther —
// which is the bucket the builder has always put such a node in.
func NormalizeRegion(region string) string {
	normalized := strings.ToLower(strings.TrimSpace(region))
	if normalized == "" {
		return geoip.RegionOther
	}
	return normalized
}
