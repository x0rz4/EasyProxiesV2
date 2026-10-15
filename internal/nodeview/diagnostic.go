package nodeview

import (
	"easy_proxies/internal/nodefacts"
	"fmt"
)

// DiagnosticCondition exposes named UI presets through the existing fact DSL.
// These predicates never publish health events or alter pool membership.
func DiagnosticCondition(name string) (*nodefacts.Condition, error) {
	leaf := func(field string, op nodefacts.Operator, value any) nodefacts.Condition {
		return nodefacts.Condition{FieldName: field, Op: op, Value: value}
	}
	var result nodefacts.Condition
	switch name {
	case "":
		return nil, nil
	case "untested":
		result = leaf(nodefacts.FieldUnlockState, nodefacts.OpIsUnknown, nil)
	case "failed", "unlocked", "mixed":
		result = leaf(nodefacts.FieldUnlockState, nodefacts.OpEq, name)
	case "locked":
		result = nodefacts.Condition{FieldName: nodefacts.FieldUnlockState, Op: nodefacts.OpIn, Values: []any{"locked", "mixed"}}
	case "residential":
		result = leaf(nodefacts.ReducedIPQualityField(nodefacts.ReduceAny, "is_residential"), nodefacts.OpEq, true)
	case "broadcast":
		result = leaf(nodefacts.ReducedIPQualityField(nodefacts.ReduceAny, "is_broadcast"), nodefacts.OpEq, true)
	case "proxy":
		result = nodefacts.Condition{Match: nodefacts.MatchAny, Children: []nodefacts.Condition{
			leaf(nodefacts.ReducedIPQualityField(nodefacts.ReduceAny, "proxy"), nodefacts.OpEq, true),
			leaf(nodefacts.ReducedIPQualityField(nodefacts.ReduceAny, "hosting"), nodefacts.OpEq, true),
		}}
	case "quality_success":
		result = nodefacts.Condition{FieldName: nodefacts.FieldQualityStatuses, Op: nodefacts.OpIn, Values: []any{"success", "partial"}}
	case "quality_failed":
		result = leaf(nodefacts.FieldQualityStatuses, nodefacts.OpContains, "failed")
	case "speed_fast":
		result = leaf(nodefacts.FieldSpeedBps, nodefacts.OpGte, int64(5*1024*1024))
	case "speed_failed":
		result = leaf(nodefacts.FieldSpeedStatus, nodefacts.OpEq, "failed")
	case "fraud_high":
		result = leaf(nodefacts.FieldIPQMaxFraudScore, nodefacts.OpGte, 71)
	default:
		return nil, fmt.Errorf("invalid diagnostic filter %q", name)
	}
	result.RegionVersion = 2
	return &result, nil
}
