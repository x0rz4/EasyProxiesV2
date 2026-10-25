package nodeview

import (
	"reflect"
	"testing"
	"time"

	"easy_proxies/internal/nodefacts"
)

func TestHealthyScopeAgreesAcrossQueryFacetsAndTargets(t *testing.T) {
	nodes := []Node{
		{ID: 1, RuntimeTag: "healthy", Healthy: true, Enabled: true, Status: "normal"},
		{ID: 2, RuntimeTag: "pending", Enabled: true, Status: "pending"},
		{ID: 3, RuntimeTag: "failed", Enabled: true, Status: "unavailable"},
		{ID: 4, RuntimeTag: "blocked", Enabled: true, Status: "blacklisted"},
		{ID: 5, RuntimeTag: "disabled", Status: "disabled"},
		{ID: 6, Healthy: true, Enabled: true, Status: "unloaded"},
		{ID: 7, RuntimeTag: "provisional", Enabled: true, Status: "normal"},
		{ID: 8, RuntimeTag: "healthy-2", Healthy: true, Enabled: true, Status: "normal"},
	}
	var facts []nodefacts.NodeFacts
	for _, node := range nodes {
		fact := nodefacts.NewNodeFacts(node.ID, "", "")
		fact.Set(nodefacts.FieldUnlockState, nodefacts.StringValue(nodefacts.Known("unlocked")))
		facts = append(facts, fact)
	}
	view := New(nodes, nil, facts, time.Time{})
	spec := Spec{Version: 2, HealthyOnly: true, Diagnostic: "unlocked"}
	ids, err := view.IDs(spec)
	if err != nil || !reflect.DeepEqual(ids, []int64{1, 8}) {
		t.Fatalf("healthy targets=%v err=%v", ids, err)
	}
	page, err := view.Query(spec, Page{Sort: "id", Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != 1 {
		t.Fatalf("healthy page=%+v err=%v", page, err)
	}
	facets := view.HealthyFacets()
	if facets.Total != len(ids) || len(facets.Statuses) != 1 || facets.Statuses[0].Value != "normal" {
		t.Fatalf("healthy facets=%+v", facets)
	}
	for _, facet := range facets.Diagnostics {
		if facet.Value == "unlocked" && facet.Count != 2 {
			t.Fatalf("diagnostic count=%d, want 2", facet.Count)
		}
	}
	if view.Facets().Total != 8 || view.RuntimeFacets().Total != 7 {
		t.Fatal("existing management/runtime scopes changed")
	}
	// A cached explicit selection must not bypass current health eligibility.
	spec.ExplicitIDs = []int64{2, 3, 4, 5, 6, 7}
	ids, err = view.IDs(spec)
	if err != nil || !reflect.DeepEqual(ids, []int64{1, 8}) {
		t.Fatalf("explicit IDs widened healthy scope: %v %v", ids, err)
	}
	nodes[0].Healthy = false
	updated := New(nodes, nil, facts, time.Time{})
	ids, err = updated.IDs(Spec{Version: 2, HealthyOnly: true})
	if err != nil || !reflect.DeepEqual(ids, []int64{8}) || updated.HealthyFacets().Total != 1 {
		t.Fatalf("health change did not converge: %v %v", ids, err)
	}
}
