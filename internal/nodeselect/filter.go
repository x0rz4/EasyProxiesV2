// Package nodeselect is the pure membership predicate shared by query views
// and groupmember. It has no persistence or runtime dependencies.
package nodeselect

import (
	"easy_proxies/internal/noderegion"
	"strconv"
	"strings"
)

type Node struct {
	ID     int64
	Region string
	Tags   []string
}

type Spec struct {
	Enabled                 bool
	Regions, ExcludeRegions []string
	RegionRuleVersion       int
	IncludeUnknownRegion    bool
	// A group with no region selection admits only explicit IDs. A list query
	// without any region constraint instead includes all regions.
	RequireRegion              bool
	ExplicitIDs, ExcludedIDs   []int64
	TagWhitelist, TagBlacklist []int64
	TagMatch                   string
}

type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

type Filter struct {
	spec               Spec
	explicit, excluded map[int64]struct{}
	whitelist          []string
	blacklist          map[string]struct{}
}

func New(spec Spec, tagNames map[int64]string) Filter {
	spec.Regions = append([]string(nil), spec.Regions...)
	spec.ExcludeRegions = append([]string(nil), spec.ExcludeRegions...)
	f := Filter{spec: spec, explicit: map[int64]struct{}{}, excluded: map[int64]struct{}{}, blacklist: map[string]struct{}{}}
	for _, id := range spec.ExplicitIDs {
		f.explicit[id] = struct{}{}
	}
	for _, id := range spec.ExcludedIDs {
		f.excluded[id] = struct{}{}
	}
	resolve := func(id int64) string {
		if name := tagNames[id]; name != "" {
			return name
		}
		return "\x00unresolved-tag-" + strconv.FormatInt(id, 10)
	}
	for _, id := range spec.TagWhitelist {
		f.whitelist = append(f.whitelist, resolve(id))
	}
	for _, id := range spec.TagBlacklist {
		f.blacklist[resolve(id)] = struct{}{}
	}
	return f
}

func (f Filter) Allow(node Node) bool { return f.Explain(node).Allowed }

// Explain preserves the group contract: disabled > excluded > explicit > rules.
func (f Filter) Explain(node Node) Decision {
	if !f.spec.Enabled {
		return Decision{Reason: "group_disabled"}
	}
	if _, found := f.excluded[node.ID]; found {
		return Decision{Reason: "explicitly_excluded"}
	}
	if _, found := f.explicit[node.ID]; found {
		return Decision{Allowed: true, Reason: "explicitly_included"}
	}
	if (f.spec.RequireRegion || len(f.spec.Regions) > 0) && !noderegion.Matches(node.Region, f.spec.Regions, f.spec.IncludeUnknownRegion, f.spec.RegionRuleVersion) {
		if noderegion.Normalize(node.Region) == "" {
			return Decision{Reason: "unknown_region"}
		}
		return Decision{Reason: "region_mismatch"}
	}
	if noderegion.Matches(node.Region, f.spec.ExcludeRegions, false, f.spec.RegionRuleVersion) {
		return Decision{Reason: "region_excluded"}
	}
	carried := make(map[string]struct{}, len(node.Tags))
	for _, tag := range node.Tags {
		carried[tag] = struct{}{}
		if _, found := f.blacklist[tag]; found {
			return Decision{Reason: "tag_blacklisted"}
		}
	}
	matched := 0
	for _, tag := range f.whitelist {
		if _, found := carried[tag]; found {
			matched++
		}
	}
	if len(f.whitelist) > 0 && (matched == 0 || (strings.EqualFold(f.spec.TagMatch, "all") && matched != len(f.whitelist))) {
		return Decision{Reason: "tag_whitelist_mismatch"}
	}
	return Decision{Allowed: true, Reason: "rules_matched"}
}
