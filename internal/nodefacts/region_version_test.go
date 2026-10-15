package nodefacts

import (
	"easy_proxies/internal/store"
	"testing"
	"time"
)

func TestRegionVersionPreservesLegacyAndPropagatesUnknown(t *testing.T) {
	facts := NewNodeFacts(1, "pending", "")
	applyNodeFields(facts, store.Node{ID: 1})
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		condition Condition
		want      bool
	}{
		{"legacy other", Condition{RegionVersion: 1, FieldName: FieldNodeRegion, Op: OpEq, Value: "other"}, true},
		{"modern other", Condition{FieldName: FieldNodeRegion, Op: OpEq, Value: "other"}, false},
		{"unknown", Condition{FieldName: FieldNodeRegion, Op: OpIsUnknown}, true},
		{"negative", Condition{FieldName: FieldNodeRegion, Op: OpNe, Value: "hk"}, false},
		{"none unknown", Condition{Match: MatchNone, Children: []Condition{{FieldName: FieldNodeRegion, Op: OpEq, Value: "hk"}}}, false},
		{"explicit unknown", Condition{Match: MatchAny, Children: []Condition{{FieldName: FieldNodeRegion, Op: OpIsUnknown}, {FieldName: FieldNodeRegion, Op: OpEq, Value: "hk"}}}, true},
		{"legacy group", Condition{RegionVersion: 1, Match: MatchAny, Children: []Condition{{FieldName: FieldNodeRegion, Op: OpEq, Value: "other"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(tc.condition, facts, now)
			if err != nil || got != tc.want {
				t.Fatalf("Evaluate=%v,%v want %v", got, err, tc.want)
			}
		})
	}
	applyNodeFields(facts, store.Node{ID: 1, Region: "sg"})
	if got, err := Evaluate(Condition{FieldName: FieldNodeRegion, Op: OpEq, Value: "other"}, facts, now); err != nil || !got {
		t.Fatalf("known other: %v %v", got, err)
	}
	applyNodeFields(facts, store.Node{ID: 1, Region: "gb"})
	if got, err := Evaluate(Condition{FieldName: FieldNodeRegion, Op: OpEq, Value: "英国"}, facts, now); err != nil || !got {
		t.Fatalf("alias: %v %v", got, err)
	}
}
