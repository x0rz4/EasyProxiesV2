package nodefacts

import (
	"testing"
	"time"
)

var evaluationNow = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// knownFacts is the reference fact set: one measured value per kind plus one
// deliberately unmeasured field per kind.
func knownFacts() NodeFacts {
	facts := NewNodeFacts(1, "HK-01", "hk")
	facts.Set(FieldLatencyMs, IntValue(KnownAt(int64(150), evaluationNow.Add(-time.Minute))))
	facts.Set(FieldNodeName, StringValue(Known("HK-01")))
	facts.Set(FieldNodeRegion, EnumValue(Known("hk")))
	facts.Set(FieldUnlockIPPure, BoolValue(KnownAt(true, evaluationNow.Add(-time.Minute))))
	facts.Set(FieldManualTags, SetValue(Known([]string{"game", "isp"})))
	facts.Set(FieldSpeedBps, IntValue(Unknown[int64]()))
	facts.Set(FieldNodeCountry, StringValue(Unknown[string]()))
	facts.Set(FieldExitIPFamily, EnumValue(Unknown[string]()))
	facts.Set(FieldUnlockedProviders, SetValue(Unknown[[]string]()))
	facts.Set(FieldBlacklisted, BoolValue(Unknown[bool]()))
	return facts
}

func leaf(field string, op Operator, value any) Condition {
	return Condition{FieldName: field, Op: op, Value: value}
}

func leafValues(field string, op Operator, values ...any) Condition {
	return Condition{FieldName: field, Op: op, Values: values}
}

func mustEvaluate(t *testing.T, condition Condition, facts NodeFacts) bool {
	t.Helper()
	matched, err := Evaluate(condition, facts, evaluationNow)
	if err != nil {
		t.Fatalf("Evaluate(%+v) error: %v", condition, err)
	}
	return matched
}

func TestEvaluateKnownOperators(t *testing.T) {
	facts := knownFacts()
	cases := []struct {
		name      string
		condition Condition
		want      bool
	}{
		{"int eq", leaf(FieldLatencyMs, OpEq, 150), true},
		{"int eq miss", leaf(FieldLatencyMs, OpEq, 100), false},
		{"int ne", leaf(FieldLatencyMs, OpNe, 100), true},
		{"int gt", leaf(FieldLatencyMs, OpGt, 100), true},
		{"int gte boundary", leaf(FieldLatencyMs, OpGte, 150), true},
		{"int lt", leaf(FieldLatencyMs, OpLt, 100), false},
		{"int lte boundary", leaf(FieldLatencyMs, OpLte, 150), true},
		{"int between", leafValues(FieldLatencyMs, OpBetween, 100, 200), true},
		{"int between reversed bounds", leafValues(FieldLatencyMs, OpBetween, 200, 100), true},
		{"int between outside", leafValues(FieldLatencyMs, OpBetween, 10, 20), false},
		{"int in", leafValues(FieldLatencyMs, OpIn, 100, 150), true},
		{"int not_in", leafValues(FieldLatencyMs, OpNotIn, 100, 200), true},
		{"int from string operand", leaf(FieldLatencyMs, OpLte, "300"), true},
		{"bool eq", leaf(FieldUnlockIPPure, OpEq, true), true},
		{"bool ne", leaf(FieldUnlockIPPure, OpNe, true), false},
		{"string eq ignores case", leaf(FieldNodeName, OpEq, "hk-01"), true},
		{"string ne", leaf(FieldNodeName, OpNe, "jp-01"), true},
		{"string contains ignores case", leaf(FieldNodeName, OpContains, "hk"), true},
		{"string not_contains", leaf(FieldNodeName, OpNotContains, "jp"), true},
		{"string regex", leaf(FieldNodeName, OpRegex, "^HK-\\d+$"), true},
		{"string regex miss", leaf(FieldNodeName, OpRegex, "^JP"), false},
		{"enum in", leafValues(FieldNodeRegion, OpIn, "HK", "TW"), true},
		{"enum not_in", leafValues(FieldNodeRegion, OpNotIn, "jp"), true},
		{"set contains ignores case", leaf(FieldManualTags, OpContains, "GAME"), true},
		{"set not_contains", leaf(FieldManualTags, OpNotContains, "vip"), true},
		{"set in intersects", leafValues(FieldManualTags, OpIn, "vip", "isp"), true},
		{"set not_in disjoint", leafValues(FieldManualTags, OpNotIn, "vip"), true},
		{"is_known", Condition{FieldName: FieldLatencyMs, Op: OpIsKnown}, true},
		{"is_unknown", Condition{FieldName: FieldLatencyMs, Op: OpIsUnknown}, false},
		{"negate flips a known match", Condition{FieldName: FieldLatencyMs, Op: OpEq, Value: 150, Negate: true}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mustEvaluate(t, testCase.condition, facts); got != testCase.want {
				t.Fatalf("Evaluate = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestEvaluateUnknownFailsEveryComparison is the semantic table the whole design
// rests on: an unmeasured fact must not satisfy a negative operator, otherwise
// every never-checked node drifts into every "not bad" tag.
func TestEvaluateUnknownFailsEveryComparison(t *testing.T) {
	facts := knownFacts()
	conditions := []Condition{
		leaf(FieldSpeedBps, OpEq, 1),
		leaf(FieldSpeedBps, OpNe, 1),
		leaf(FieldSpeedBps, OpGt, 1),
		leaf(FieldSpeedBps, OpGte, 1),
		leaf(FieldSpeedBps, OpLt, 1),
		leaf(FieldSpeedBps, OpLte, 1),
		leafValues(FieldSpeedBps, OpBetween, 0, 100),
		leafValues(FieldSpeedBps, OpIn, 1, 2),
		leafValues(FieldSpeedBps, OpNotIn, 1, 2),
		leaf(FieldNodeCountry, OpEq, "Japan"),
		leaf(FieldNodeCountry, OpNe, "Japan"),
		leaf(FieldNodeCountry, OpContains, "Ja"),
		leaf(FieldNodeCountry, OpNotContains, "Ja"),
		leaf(FieldNodeCountry, OpRegex, ".*"),
		leaf(FieldExitIPFamily, OpNe, "ipv6"),
		leaf(FieldBlacklisted, OpNe, true),
		leaf(FieldUnlockedProviders, OpContains, "netflix"),
		leaf(FieldUnlockedProviders, OpNotContains, "netflix"),
		leafValues(FieldUnlockedProviders, OpNotIn, "netflix"),
		// A field that was never loaded at all is unknown, not an error.
		leaf("unlock.netflix.status", OpNe, "unlocked"),
		{FieldName: FieldSpeedBps, Op: OpNe, Value: 1, Negate: true},
		{FieldName: FieldNodeCountry, Op: OpEq, Value: "Japan", Negate: true},
	}
	for _, condition := range conditions {
		if mustEvaluate(t, condition, facts) {
			t.Fatalf("unknown fact matched %s %s (negate=%v)", condition.FieldName, condition.Op, condition.Negate)
		}
	}
	// Only these two operators may observe an unknown fact.
	if !mustEvaluate(t, Condition{FieldName: FieldSpeedBps, Op: OpIsUnknown}, facts) {
		t.Fatal("is_unknown did not observe an unmeasured fact")
	}
	if mustEvaluate(t, Condition{FieldName: FieldSpeedBps, Op: OpIsKnown}, facts) {
		t.Fatal("is_known matched an unmeasured fact")
	}
	if !mustEvaluate(t, Condition{FieldName: FieldSpeedBps, Op: OpIsKnown, Negate: true}, facts) {
		t.Fatal("negate must apply to is_known, which can observe unknown")
	}
}

// TestEvaluateMaxAgeDemotesStaleFacts covers the time-driven half of the
// semantics: a stale success stops matching, and a timeless configuration fact
// is never expired by a rule.
func TestEvaluateMaxAgeDemotesStaleFacts(t *testing.T) {
	facts := NewNodeFacts(1, "node", "hk")
	facts.Set(FieldLatencyMs, IntValue(KnownAt(int64(80), evaluationNow.Add(-2*time.Hour))))
	facts.Set(FieldNodeName, StringValue(Known("node")))

	fresh := Condition{FieldName: FieldLatencyMs, Op: OpLte, Value: 100}
	if !mustEvaluate(t, fresh, facts) {
		t.Fatal("a stale fact must still match when the rule sets no max age")
	}
	stale := Condition{FieldName: FieldLatencyMs, Op: OpLte, Value: 100, MaxAgeSeconds: 3600}
	if mustEvaluate(t, stale, facts) {
		t.Fatal("max_age_seconds did not demote the stale measurement to unknown")
	}
	if !mustEvaluate(t, Condition{FieldName: FieldLatencyMs, Op: OpIsUnknown, MaxAgeSeconds: 3600}, facts) {
		t.Fatal("an expired fact must read as unknown")
	}
	if !mustEvaluate(t, Condition{FieldName: FieldLatencyMs, Op: OpLte, Value: 100, MaxAgeSeconds: 3 * 3600}, facts) {
		t.Fatal("a fact inside the window must stay known")
	}
	timeless := Condition{FieldName: FieldNodeName, Op: OpEq, Value: "node", MaxAgeSeconds: 1}
	if !mustEvaluate(t, timeless, facts) {
		t.Fatal("a fact with no timestamp is configuration and must never expire")
	}
}

func TestEvaluateGroups(t *testing.T) {
	facts := knownFacts()
	fast := leaf(FieldLatencyMs, OpLte, 200)
	slow := leaf(FieldLatencyMs, OpGt, 200)
	cases := []struct {
		name      string
		condition Condition
		want      bool
	}{
		{"all satisfied", Condition{Match: MatchAll, Children: []Condition{fast, leaf(FieldNodeRegion, OpEq, "hk")}}, true},
		{"all with one miss", Condition{Match: MatchAll, Children: []Condition{fast, slow}}, false},
		{"any satisfied", Condition{Match: MatchAny, Children: []Condition{slow, fast}}, true},
		{"any all miss", Condition{Match: MatchAny, Children: []Condition{slow, leaf(FieldNodeRegion, OpEq, "jp")}}, false},
		{"none is not any", Condition{Match: MatchNone, Children: []Condition{slow}}, true},
		{"none with a hit", Condition{Match: MatchNone, Children: []Condition{fast}}, false},
		{"nested", Condition{Match: MatchAll, Children: []Condition{
			fast,
			{Match: MatchAny, Children: []Condition{slow, leaf(FieldManualTags, OpContains, "game")}},
		}}, true},
		// A group of unknown-only leaves must not become true through "none".
		{"none over unknown leaves", Condition{Match: MatchNone, Children: []Condition{
			leaf(FieldSpeedBps, OpGt, 0),
		}}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mustEvaluate(t, testCase.condition, facts); got != testCase.want {
				t.Fatalf("Evaluate = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestEvaluateEmptyRuleNeverMatches(t *testing.T) {
	if mustEvaluate(t, Condition{}, knownFacts()) {
		t.Fatal("an empty rule must not match, otherwise a tag with no rule would tag everything")
	}
}

func TestEvaluateReportsOperandErrors(t *testing.T) {
	facts := knownFacts()
	for _, condition := range []Condition{
		leaf(FieldLatencyMs, OpEq, "fast"),
		leaf(FieldUnlockIPPure, OpEq, "maybe"),
		leaf(FieldNodeName, OpRegex, "([unclosed"),
		{FieldName: FieldLatencyMs, Op: "sideways", Value: 1},
		{Match: "sometimes", Children: []Condition{leaf(FieldLatencyMs, OpEq, 150)}},
	} {
		if _, err := Evaluate(condition, facts, evaluationNow); err == nil {
			t.Fatalf("Evaluate(%+v) accepted a malformed condition", condition)
		}
	}
}
