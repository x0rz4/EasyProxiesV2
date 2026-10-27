package nodeview

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodeselect"
)

func viewFixture() ([]Node, map[int64]string, []nodefacts.NodeFacts) {
	nodes := []Node{{ID: 2, Name: "second", Enabled: true, Region: "hk", Tags: []string{"fast"}, SubscriptionIDs: []int64{7}}, {ID: 1, Name: "first", Enabled: true, Tags: []string{}, SubscriptionIDs: []int64{}}}
	names := map[int64]string{7: "fast"}
	facts := []nodefacts.NodeFacts{nodefacts.NewNodeFacts(2, "second", "hk"), nodefacts.NewNodeFacts(1, "first", "")}
	facts[0].Set("test.set", nodefacts.SetValue(nodefacts.Known([]string{"a"})))
	facts[1].Set("test.empty", nodefacts.SetValue(nodefacts.Known([]string{})))
	return nodes, names, facts
}

func TestViewConstructorsAgreeAndDefensiveSnapshotIsDetached(t *testing.T) {
	nodes, names, facts := viewFixture()
	view := New(nodes, names, facts, time.Time{})
	ownedNodes, ownedNames, ownedFacts := viewFixture()
	owned := NewOwned(ownedNodes, ownedNames, ownedFacts, time.Time{})
	if view.Revision() != owned.Revision() {
		t.Fatal("ownership changed the snapshot identity")
	}
	nodes[0].Name = "changed"
	nodes[0].Tags[0] = "changed"
	nodes[0].SubscriptionIDs[0] = 99
	names[7] = "changed"
	value := facts[0].Value("test.set")
	value.Set[0] = "changed"
	facts[0].Values["new"] = nodefacts.StringValue(nodefacts.Known("changed"))
	result, err := view.Query(Spec{TagWhitelist: []int64{7}}, Page{})
	if err != nil || len(result.Items) != 1 || result.Items[0].Name != "second" || result.Items[0].Tags[0] != "fast" || result.Items[0].SubscriptionIDs[0] != 7 || view.facts[2].Value("test.set").Set[0] != "a" || view.facts[2].Value("new").Known {
		t.Fatalf("input mutation changed the view: %+v %v", result, err)
	}
	page, _ := owned.Query(Spec{}, Page{})
	if page.Items[0].Tags != nil || page.Items[0].SubscriptionIDs != nil {
		t.Fatal("empty collection wire shape changed")
	}
}

func TestViewResponsesCannotMutateSnapshot(t *testing.T) {
	nodes, names, facts := viewFixture()
	view := NewOwned(nodes, names, facts, time.Time{})
	page, _ := view.Query(Spec{}, Page{})
	page.Items[1].Tags[0] = "changed"
	page.Items[1].SubscriptionIDs[0] = 99
	candidates := view.Candidates(nodeselect.Spec{Enabled: true})
	candidates[1].Tags[0] = "changed-again"
	candidates[1].SubscriptionIDs[0] = 88
	result, _ := view.Query(Spec{}, Page{})
	if result.Items[1].Tags[0] != "fast" || result.Items[1].SubscriptionIDs[0] != 7 {
		t.Fatal("response mutated snapshot storage")
	}
}

func TestRevisionIsDeterministicAndIncludesNodesNamesAndFacts(t *testing.T) {
	nodes, names, facts := viewFixture()
	before := New(nodes, names, facts, time.Time{}).Revision()
	slices.Reverse(nodes)
	slices.Reverse(facts)
	if got := New(nodes, names, facts, time.Time{}).Revision(); got != before {
		t.Fatal("input ordering changed revision")
	}
	for _, change := range []func([]Node, map[int64]string, []nodefacts.NodeFacts){
		func(n []Node, _ map[int64]string, _ []nodefacts.NodeFacts) { n[0].Name = "different\nname" },
		func(_ []Node, n map[int64]string, _ []nodefacts.NodeFacts) { n[7] = "renamed" },
		func(_ []Node, _ map[int64]string, f []nodefacts.NodeFacts) {
			f[0].Set("new", nodefacts.IntValue(nodefacts.Known(int64(3))))
		},
	} {
		n, tags, f := viewFixture()
		change(n, tags, f)
		if New(n, tags, f, time.Time{}).Revision() == before {
			t.Fatal("snapshot change did not change revision")
		}
	}
}

func TestIDPagesKeepTotalsFiltersAndDirection(t *testing.T) {
	var nodes []Node
	for id := int64(1); id <= 113; id++ {
		nodes = append(nodes, Node{ID: id, Name: "node", Enabled: true, Healthy: id%2 == 0, RuntimeTag: "live"})
	}
	view := New(nodes, nil, nil, time.Time{})
	spec := Spec{HealthyOnly: true, ExcludedIDs: []int64{10}}
	all, err := view.IDs(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, descending := range []bool{false, true} {
		ids := slices.Clone(all)
		if descending {
			slices.Reverse(ids)
		}
		for _, offset := range []int{0, 1, 17, 55, 1000} {
			page, err := view.Query(spec, Page{Sort: "id", Descending: descending, Offset: offset, Limit: 7})
			if err != nil || page.Total != len(all) {
				t.Fatalf("total=%d err=%v", page.Total, err)
			}
			start := min(offset, len(ids))
			end := min(start+7, len(ids))
			var got []int64
			for _, node := range page.Items {
				got = append(got, node.ID)
			}
			if !slices.Equal(got, ids[start:end]) {
				t.Fatalf("desc=%v offset=%d got=%v want=%v", descending, offset, got, ids[start:end])
			}
		}
	}
}

func TestIndexedSortsPreserveKnownFirstAndStableTies(t *testing.T) {
	nodes := []Node{{ID: 1, Name: "Bravo", Region: "us", Source: "subscription", Status: "pending", Port: 200, LatencyMS: 20},
		{ID: 2, Name: "alpha", Region: "hk", Source: "manual", Status: "normal", Port: 100, LatencyMS: 10},
		{ID: 3, Name: "ALPHA", Region: "hk", Source: "manual", Status: "normal", Port: 100, LatencyMS: 10}}
	facts := []nodefacts.NodeFacts{nodefacts.NewNodeFacts(1, "", ""), nodefacts.NewNodeFacts(2, "", ""), nodefacts.NewNodeFacts(3, "", "")}
	for _, field := range []string{nodefacts.FieldSpeedBps, nodefacts.FieldIPQMaxFraudScore, nodefacts.FieldLatencyMs} {
		facts[0].Set(field, nodefacts.IntValue(nodefacts.Known(int64(2))))
		facts[1].Set(field, nodefacts.IntValue(nodefacts.Known(int64(1))))
	}
	view := New(nodes, nil, facts, time.Time{})
	for _, key := range []string{"name", "region", "source", "status", "port", "latency_ms", "speed_bps", "fraud_score", "diagnostic_latency"} {
		for _, desc := range []bool{false, true} {
			page, err := view.Query(Spec{}, Page{Sort: key, Descending: desc})
			var got []int64
			for _, node := range page.Items {
				got = append(got, node.ID)
			}
			want := []int64{2, 3, 1}
			if key == "speed_bps" || key == "fraud_score" || key == "diagnostic_latency" {
				want = []int64{2, 1, 3}
				if desc {
					want = []int64{1, 2, 3}
				}
			} else if desc {
				want = []int64{1, 2, 3}
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s desc=%v got=%v want=%v err=%v", key, desc, got, want, err)
			}
		}
	}
}
