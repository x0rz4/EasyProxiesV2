package nodefacts

import (
	"strings"
	"testing"

	json "easy_proxies/internal/jsonx"
)

func testRegistry() *Registry {
	return DefaultRegistry(
		WithUnlockProviders([]ProviderInfo{{Name: "netflix", Label: "Netflix"}}),
		WithIPQualityProviders([]ProviderInfo{{Name: "ippure", Label: "IPPure"}}),
	)
}

func TestValidateAcceptsWellFormedRules(t *testing.T) {
	registry, limits := testRegistry(), DefaultLimits()
	rules := []Condition{
		{},
		leaf(FieldLatencyMs, OpLte, 200),
		{FieldName: FieldLatencyMs, Op: OpLte, Value: 200, MaxAgeSeconds: 3600},
		{FieldName: FieldSpeedBps, Op: OpIsUnknown},
		leafValues(FieldLatencyMs, OpBetween, 50, 200),
		leafValues(FieldNodeRegion, OpIn, "hk", "tw"),
		leaf(FieldNodeName, OpRegex, "^HK-\\d+$"),
		leaf(FieldManualTags, OpContains, "vip"),
		leaf(UnlockField("netflix", "status"), OpEq, "unlocked"),
		leaf(IPQualityField("ippure", "fraud_score"), OpLte, 30),
		leaf(ReducedIPQualityField(ReduceAny, "proxy"), OpEq, false),
		{Match: MatchAll, Children: []Condition{
			leaf(FieldLatencyMs, OpLte, 200),
			{Match: MatchAny, Children: []Condition{
				leaf(FieldUnlockIPPure, OpEq, true),
				leaf(FieldNodeRegion, OpEq, "hk"),
			}},
		}},
	}
	for _, rule := range rules {
		if err := rule.Validate(registry, limits); err != nil {
			t.Fatalf("Validate(%+v) rejected a valid rule: %v", rule, err)
		}
	}
}

// TestValidateRejectsMalformedRules is the save-time gate: every one of these
// would otherwise reach a recompute and either never match or cost unbounded work.
func TestValidateRejectsMalformedRules(t *testing.T) {
	registry, limits := testRegistry(), DefaultLimits()
	cases := []struct {
		name string
		rule Condition
	}{
		{"unknown field", leaf("latency", OpEq, 1)},
		{"unknown operator", leaf(FieldLatencyMs, "近似", 1)},
		{"operator not on this kind", leaf(FieldLatencyMs, OpRegex, "^1")},
		{"regex on an int field", leaf(FieldNodePort, OpRegex, "^80")},
		{"non-numeric operand", leaf(FieldLatencyMs, OpEq, "fast")},
		{"non-boolean operand", leaf(FieldNodeEnabled, OpEq, "perhaps")},
		{"missing value", Condition{FieldName: FieldLatencyMs, Op: OpEq}},
		{"value on a no-arity operator", Condition{FieldName: FieldLatencyMs, Op: OpIsKnown, Value: 1}},
		{"between with one bound", leafValues(FieldLatencyMs, OpBetween, 1)},
		{"in with no values", Condition{FieldName: FieldNodeRegion, Op: OpIn}},
		{"single value on a many operator", Condition{FieldName: FieldNodeRegion, Op: OpIn, Value: "hk"}},
		{"negative max age", Condition{FieldName: FieldLatencyMs, Op: OpEq, Value: 1, MaxAgeSeconds: -1}},
		{"max age on configuration", Condition{FieldName: FieldNodeName, Op: OpEq, Value: "x", MaxAgeSeconds: 60}},
		{"leaf without a field", Condition{Op: OpEq, Value: 1}},
		{"leaf with children", Condition{FieldName: FieldLatencyMs, Op: OpEq, Value: 1,
			Children: []Condition{leaf(FieldLatencyMs, OpEq, 1)}}},
		{"unknown match", Condition{Match: "maybe", Children: []Condition{leaf(FieldLatencyMs, OpEq, 1)}}},
		{"group with no children", Condition{Match: MatchAll}},
		{"group carrying a field", Condition{Match: MatchAll, FieldName: FieldLatencyMs,
			Children: []Condition{leaf(FieldLatencyMs, OpEq, 1)}}},
		{"uncompilable regex", leaf(FieldNodeName, OpRegex, "([unclosed")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.rule.Validate(registry, limits); err == nil {
				t.Fatalf("Validate accepted %+v", testCase.rule)
			}
		})
	}
}

func TestValidateEnforcesLimits(t *testing.T) {
	registry, limits := testRegistry(), DefaultLimits()

	tooMany := Condition{Match: MatchAny}
	for index := 0; index <= limits.MaxConditions; index++ {
		tooMany.Children = append(tooMany.Children, leaf(FieldLatencyMs, OpEq, index))
	}
	if err := tooMany.Validate(registry, limits); err == nil {
		t.Fatal("a rule with more than MaxConditions leaves was accepted")
	}

	tooDeep := Condition{Match: MatchAll, Children: []Condition{
		{Match: MatchAll, Children: []Condition{
			{Match: MatchAll, Children: []Condition{leaf(FieldLatencyMs, OpEq, 1)}},
		}},
	}}
	if err := tooDeep.Validate(registry, limits); err == nil {
		t.Fatalf("a rule nested deeper than %d was accepted", limits.MaxDepth)
	}

	tooManyValues := Condition{FieldName: FieldNodeRegion, Op: OpIn}
	for index := 0; index <= limits.MaxValueItems; index++ {
		tooManyValues.Values = append(tooManyValues.Values, "hk")
	}
	if err := tooManyValues.Validate(registry, limits); err == nil {
		t.Fatalf("a value list longer than %d was accepted", limits.MaxValueItems)
	}

	longPattern := leaf(FieldNodeName, OpRegex, strings.Repeat("节", limits.MaxRegexLength+1))
	if err := longPattern.Validate(registry, limits); err == nil {
		t.Fatalf("a regex longer than %d runes was accepted", limits.MaxRegexLength)
	}

	// A hundred long operands stay inside MaxValueItems yet blow the byte budget,
	// which is why the size check exists separately from the item count.
	oversized := Condition{FieldName: FieldNodeName, Op: OpIn}
	for index := 0; index < limits.MaxValueItems; index++ {
		oversized.Values = append(oversized.Values, strings.Repeat("x", 200))
	}
	if err := oversized.Validate(registry, limits); err == nil {
		t.Fatalf("a rule larger than %d bytes was accepted", limits.MaxRuleBytes)
	}
}

func TestValidateNilRegistryRejectsEveryField(t *testing.T) {
	if err := leaf(FieldLatencyMs, OpEq, 1).Validate(nil, DefaultLimits()); err == nil {
		t.Fatal("a rule validated against no registry must be rejected, not trusted")
	}
}

// TestRuleRoundTrip pins the stored form: re-encoding a decoded rule must produce
// the same bytes, otherwise rule_version would advance on every save.
func TestRuleRoundTrip(t *testing.T) {
	original := Condition{Match: MatchAll, Children: []Condition{
		{FieldName: FieldLatencyMs, Op: OpLte, Value: 200, MaxAgeSeconds: 3600},
		{FieldName: FieldNodeRegion, Op: OpIn, Values: []any{"hk", "tw"}},
		{FieldName: FieldUnlockIPPure, Op: OpEq, Value: true, Negate: true},
		{Match: MatchNone, Children: []Condition{
			{FieldName: FieldManualTags, Op: OpContains, Value: "blocked"},
		}},
	}}
	encoded, err := MarshalRule(original)
	if err != nil {
		t.Fatalf("MarshalRule: %v", err)
	}
	parsed, err := ParseRule(encoded)
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	again, err := MarshalRule(parsed)
	if err != nil {
		t.Fatalf("MarshalRule after round trip: %v", err)
	}
	if string(again) != string(encoded) {
		t.Fatalf("round trip changed the stored rule:\n before %s\n after  %s", encoded, again)
	}
	if err := parsed.Validate(testRegistry(), DefaultLimits()); err != nil {
		t.Fatalf("a decoded rule stopped validating: %v", err)
	}
	// A decoded operand arrives as a JSON number, which the evaluator must still
	// read as an integer.
	if !mustEvaluate(t, parsed.Children[0], knownFacts()) {
		t.Fatal("a decoded numeric operand did not compare as an integer")
	}
}

func TestMarshalRuleDropsEmptyRules(t *testing.T) {
	encoded, err := MarshalRule(Condition{})
	if err != nil {
		t.Fatalf("MarshalRule: %v", err)
	}
	if encoded != nil {
		t.Fatalf("an empty rule must serialize to nil, got %q", encoded)
	}
	for _, payload := range []string{"", "   ", "\n\t"} {
		condition, err := ParseRule([]byte(payload))
		if err != nil {
			t.Fatalf("ParseRule(%q): %v", payload, err)
		}
		if !condition.IsEmpty() {
			t.Fatalf("ParseRule(%q) did not produce the no-rule condition", payload)
		}
	}
	if _, err := ParseRule([]byte("{not json")); err == nil {
		t.Fatal("ParseRule accepted malformed JSON")
	}
}

func TestParseRuleReadsCanonicalFieldNames(t *testing.T) {
	condition, err := ParseRule([]byte(`{"field":"latency_ms","op":"lte","value":200,"max_age_seconds":600}`))
	if err != nil {
		t.Fatalf("ParseRule: %v", err)
	}
	if condition.FieldName != FieldLatencyMs || condition.Op != OpLte || condition.MaxAgeSeconds != 600 {
		t.Fatalf("decoded rule lost data: %+v", condition)
	}
}

func TestReferencedFieldsIsSortedAndDeduplicated(t *testing.T) {
	condition := Condition{Match: MatchAll, Children: []Condition{
		leaf(FieldNodeRegion, OpEq, "hk"),
		leaf(FieldLatencyMs, OpLte, 200),
		{Match: MatchAny, Children: []Condition{
			leaf(FieldLatencyMs, OpGt, 10),
			leaf(FieldUnlockIPPure, OpEq, true),
		}},
	}}
	want := []string{FieldLatencyMs, FieldNodeRegion, FieldUnlockIPPure}
	got := ReferencedFields(condition)
	if len(got) != len(want) {
		t.Fatalf("ReferencedFields = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ReferencedFields = %v, want %v", got, want)
		}
	}
	if fields := ReferencedFields(Condition{}); len(fields) != 0 {
		t.Fatalf("an empty rule references no fields, got %v", fields)
	}
}

func TestConditionShapePredicates(t *testing.T) {
	group := Condition{Match: MatchAll, Children: []Condition{leaf(FieldLatencyMs, OpEq, 1)}}
	if !group.IsGroup() || group.IsLeaf() || group.IsEmpty() {
		t.Fatalf("group misclassified: %+v", group)
	}
	single := leaf(FieldLatencyMs, OpEq, 1)
	if single.IsGroup() || !single.IsLeaf() || single.IsEmpty() {
		t.Fatalf("leaf misclassified: %+v", single)
	}
	if empty := (Condition{}); empty.IsGroup() || empty.IsLeaf() || !empty.IsEmpty() {
		t.Fatal("the zero condition must read as empty")
	}
}

func TestDefaultRegistryOmitsAutoTags(t *testing.T) {
	registry := testRegistry()
	if _, ok := registry.Field("tags.auto"); ok {
		t.Fatal("tags.auto must not be a rule field: an auto tag reading auto tags is order-dependent")
	}
	if _, ok := registry.Field(FieldManualTags); !ok {
		t.Fatal("tags.manual is operator input and must stay available")
	}
	for _, field := range registry.Fields() {
		if field.Label == "" || field.Group == "" {
			t.Fatalf("field %q is missing display metadata", field.Name)
		}
		if len(field.Operators) == 0 {
			t.Fatalf("field %q has no operators", field.Name)
		}
		for _, op := range field.Operators {
			if _, ok := OperatorArity(op); !ok {
				t.Fatalf("field %q advertises unknown operator %q", field.Name, op)
			}
		}
	}
}

func TestConditionJSONOmitsUnsetFields(t *testing.T) {
	encoded, err := json.Marshal(leaf(FieldLatencyMs, OpEq, 1))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{"match", "children", "values", "max_age_seconds", "negate"} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("encoded leaf carries unset key %q: %s", key, encoded)
		}
	}
}
