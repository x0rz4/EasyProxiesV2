package groupmember

import (
	"easy_proxies/internal/config"
	"testing"
)

func TestModernRegionRulesKeepOverridePriority(t *testing.T) {
	filter := NewFilter(config.GroupPoolConfig{Enabled: true, RegionRuleVersion: 2, Regions: []string{"other"}, ExcludeRegions: []string{"de"}, ExplicitNodeIDs: []int64{2, 3}, ExcludedNodeIDs: []int64{3}})
	for _, tc := range []struct {
		node Node
		want bool
	}{
		{Node{ID: 1}, false}, {Node{ID: 1, Region: "sg"}, true}, {Node{ID: 1, Region: "de"}, false},
		{Node{ID: 2, Region: "de"}, true}, {Node{ID: 3, Region: "sg"}, false},
	} {
		if got := filter.Allow(tc.node); got != tc.want {
			t.Errorf("Allow(%+v)=%v want %v", tc.node, got, tc.want)
		}
	}
}
