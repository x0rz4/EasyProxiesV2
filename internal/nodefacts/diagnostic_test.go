package nodefacts

import (
	"easy_proxies/internal/noderesult"
	"easy_proxies/internal/store"
	"testing"
	"time"
)

func TestDiagnosticSummarySeparatesUnknownAndRestricted(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		result *store.UnlockResult
		want   string
	}{
		{"unknown", nil, ""},
		{"complete", &store.UnlockResult{Services: []noderesult.Service{{Status: "unlocked"}}}, "unlocked"},
		{"limited", &store.UnlockResult{Services: []noderesult.Service{{Status: "partial"}}}, "locked"},
		{"mixed", &store.UnlockResult{Services: []noderesult.Service{{Status: "unlocked"}, {Status: "locked"}}}, "mixed"},
		{"error", &store.UnlockResult{Error: "timeout", Services: []noderesult.Service{{Status: "unlocked"}}}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := NewNodeFacts(1, "", "")
			if tc.result != nil {
				tc.result.CheckedAt = now
			}
			applyDiagnosticFields(facts, nil, tc.result, nil)
			got := facts.Value(FieldUnlockState)
			if got.Known != (tc.result != nil) || got.Str != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
	facts := NewNodeFacts(1, "", "")
	yes := true
	quality := []store.NodeIPQualityResult{{Provider: "test", Status: "partial", IsBroadcast: &yes, CheckedAt: now}}
	applyIPQualityFields(facts, quality)
	applyDiagnosticFields(facts, &store.NodeDetectionResult{SpeedStatus: "failed", SpeedCheckedAt: now}, nil, quality)
	if f := facts.Value(ReducedIPQualityField(ReduceAny, "is_broadcast")); !f.Known || !f.Bool {
		t.Fatalf("partial valid field lost: %+v", f)
	}
	if f := facts.Value(FieldSpeedStatus); f.Str != "failed" || !f.CheckedAt.Equal(now) {
		t.Fatalf("speed status: %+v", f)
	}
}
