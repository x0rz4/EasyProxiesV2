package nodeview

import (
	"easy_proxies/internal/nodefacts"
	"testing"
	"time"
)

func TestDiagnosticQueryFacetsAndBulkTargetsAgree(t *testing.T) {
	nodes := []Node{}
	facts := []nodefacts.NodeFacts{}
	for id := int64(1); id <= 601; id++ {
		nodes = append(nodes, Node{ID: id, RuntimeTag: "live", Enabled: true})
		f := nodefacts.NewNodeFacts(id, "", "")
		state := "unlocked"
		if id%2 == 0 {
			state = "mixed"
		}
		f.Set(nodefacts.FieldUnlockState, nodefacts.StringValue(nodefacts.Known(state)))
		f.Set(nodefacts.FieldSpeedBps, nodefacts.IntValue(nodefacts.Known(id)))
		facts = append(facts, f)
	}
	nodes = append(nodes, Node{ID: 602}) // off-runtime, untested
	view := New(nodes, nil, facts, time.Time{})
	spec := Spec{Version: 2, Diagnostic: "locked", RuntimeOnly: true}
	ids, err := view.IDs(spec)
	if err != nil || len(ids) != 300 || ids[0] != 2 || ids[299] != 600 {
		t.Fatalf("bulk ids: %d %v", len(ids), err)
	}
	page, err := view.Query(spec, Page{Sort: "speed_bps", Descending: true, Limit: 1})
	if err != nil || page.Total != 300 || page.Items[0].ID != 600 {
		t.Fatalf("page: %+v %v", page, err)
	}
	all, err := view.IDs(Spec{Version: 2, RuntimeOnly: true})
	if err != nil || len(all) != 601 {
		t.Fatalf("bulk unexpectedly paged: %d %v", len(all), err)
	}
	facets := view.RuntimeFacets()
	if facets.Total != 601 || view.Facets().Total != 602 {
		t.Fatal("runtime scope mismatch")
	}
	for _, facet := range facets.Diagnostics {
		if facet.Value == "locked" && facet.Count != len(ids) {
			t.Fatalf("count diverged: %+v", facet)
		}
		if facet.Value == "untested" && facet.Count != 0 {
			t.Fatalf("offline leaked: %+v", facet)
		}
	}
	if err := (Spec{Diagnostic: "invalid"}).Validate(nil); err == nil {
		t.Fatal("invalid diagnostic accepted")
	}
}
