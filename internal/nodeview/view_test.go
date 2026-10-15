package nodeview

import (
	"easy_proxies/internal/config"
	"easy_proxies/internal/groupmember"
	"easy_proxies/internal/nodeselect"
	"testing"
	"time"
)

func TestQueryPaginationFacetsAndRegionSemantics(t *testing.T) {
	v := New([]Node{{ID: 3, Name: "equal", Region: "sg", Status: "normal", Healthy: true, Enabled: true, Tags: []string{"fast"}},
		{ID: 1, Name: "equal", Region: "", Status: "pending", Enabled: true},
		{ID: 2, Name: "equal", Region: "hk", Status: "normal", Healthy: true, Enabled: true}}, map[int64]string{7: "fast"}, nil, time.Time{})
	r, err := v.Query(Spec{Version: 2}, Page{Sort: "name", Limit: 1, Offset: 1})
	if err != nil || r.Total != 3 || len(r.Items) != 1 || r.Items[0].ID != 2 {
		t.Fatalf("stable page: %+v %v", r, err)
	}
	r, err = v.Query(Spec{Version: 2, Regions: []string{"other"}}, Page{})
	if err != nil || r.Total != 1 || r.Items[0].ID != 3 {
		t.Fatalf("known other: %+v %v", r, err)
	}
	r, err = v.Query(Spec{Version: 2, IncludeUnknownRegion: true}, Page{})
	if err != nil || r.Total != 1 || r.Items[0].ID != 1 {
		t.Fatalf("unknown only: %+v %v", r, err)
	}
	r, err = v.Query(Spec{Version: 2, TagWhitelist: []int64{999}}, Page{})
	if err != nil || r.Total != 0 {
		t.Fatalf("unresolved tag widened query: %+v %v", r, err)
	}
	facets := v.Facets()
	if facets.Total != 3 || facets.SnapshotRevision != r.SnapshotRevision {
		t.Fatalf("facets differ from snapshot: %+v", facets)
	}
	unknown := 0
	for _, facet := range facets.Regions {
		if facet.Value == "unknown" {
			unknown = facet.Count
		}
	}
	if unknown != 1 {
		t.Fatalf("unknown count=%d", unknown)
	}
}

func TestGroupAndViewUseSamePredicate(t *testing.T) {
	group := config.GroupPoolConfig{Enabled: true, RegionRuleVersion: 2, Regions: []string{"hk"}, TagWhitelist: []int64{7}, TagFilterMatch: "all", ExplicitNodeIDs: []int64{4, 5}, ExcludedNodeIDs: []int64{5}}
	names := map[int64]string{7: "fast"}
	filter := groupmember.NewFilter(group, groupmember.WithTagNames(names))
	nodes := []Node{{ID: 1, Region: "hk", Tags: []string{"fast"}, Enabled: true}, {ID: 2, Region: "hk", Enabled: true}, {ID: 3, Enabled: true}, {ID: 4, Region: "jp", Enabled: true}, {ID: 5, Region: "hk", Tags: []string{"fast"}, Enabled: true}}
	v := New(nodes, names, nil, time.Time{})
	candidates := v.Candidates(nodeselect.Spec{Enabled: true, RequireRegion: true, RegionRuleVersion: 2, Regions: group.Regions, TagWhitelist: group.TagWhitelist, TagMatch: group.TagFilterMatch, ExplicitIDs: group.ExplicitNodeIDs, ExcludedIDs: group.ExcludedNodeIDs})
	for _, candidate := range candidates {
		want := filter.Allow(groupmember.Node{ID: candidate.ID, Region: candidate.Region, Tags: candidate.Tags})
		if candidate.Decision.Allowed != want {
			t.Fatalf("preview mismatch: %+v want %v", candidate, want)
		}
	}
}
