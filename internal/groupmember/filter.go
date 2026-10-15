// Package groupmember is the group adapter for the shared membership predicate.
// Builder, boxmgr and preview all use this entry point for group membership.
package groupmember

import (
	"easy_proxies/internal/config"
	"easy_proxies/internal/noderegion"
	"easy_proxies/internal/nodeselect"
)

type Node = nodeselect.Node
type Filter struct{ core nodeselect.Filter }
type Option func(*filterOptions)
type filterOptions struct{ tagNames map[int64]string }

func WithTagNames(names map[int64]string) Option {
	return func(o *filterOptions) { o.tagNames = names }
}
func NewFilter(group config.GroupPoolConfig, opts ...Option) Filter {
	var options filterOptions
	for _, apply := range opts {
		apply(&options)
	}
	return Filter{core: nodeselect.New(nodeselect.Spec{
		Enabled: group.Enabled, Regions: group.Regions, ExcludeRegions: group.ExcludeRegions,
		RegionRuleVersion: group.RegionRuleVersion, IncludeUnknownRegion: group.IncludeUnknownRegion, RequireRegion: true,
		ExplicitIDs: group.ExplicitNodeIDs, ExcludedIDs: group.ExcludedNodeIDs,
		TagWhitelist: group.TagWhitelist, TagBlacklist: group.TagBlacklist, TagMatch: group.TagFilterMatch,
	}, options.tagNames)}
}
func (f Filter) Allow(node Node) bool                  { return f.core.Allow(node) }
func (f Filter) Explain(node Node) nodeselect.Decision { return f.core.Explain(node) }

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

// NormalizeRegion preserves the historical v1 normalization for compatibility
// callers. Membership itself uses the versioned nodeselect predicate.
func NormalizeRegion(region string) string {
	return noderegion.Legacy(region)
}
