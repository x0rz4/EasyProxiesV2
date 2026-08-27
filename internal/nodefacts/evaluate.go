package nodefacts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	// stdjson is only used for its Number type, so a decoder configured with
	// UseNumber still produces operands this package can read.
	stdjson "encoding/json"
)

// Evaluate reports whether a node's facts satisfy the condition. It is a pure
// function of (condition, facts, now): no I/O, no caching, no ordering effects,
// so a recompute over unchanged facts is guaranteed to produce the same answer.
//
// Unknown facts are the heart of the semantics: a fact that was never measured
// fails every comparison, negative operators (ne, not_in, not_contains) included.
// Only is_unknown and is_known can observe one. Without that rule an unmeasured
// node would drift into every "not bad" tag.
func Evaluate(condition Condition, facts NodeFacts, now time.Time) (bool, error) {
	if condition.IsEmpty() {
		return false, nil
	}
	if condition.IsGroup() {
		return evaluateGroup(condition, facts, now)
	}
	return evaluateLeaf(condition, facts, now)
}

func evaluateGroup(condition Condition, facts NodeFacts, now time.Time) (bool, error) {
	switch condition.Match {
	case MatchAll:
		for _, child := range condition.Children {
			matched, err := Evaluate(child, facts, now)
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		return true, nil
	case MatchAny, MatchNone:
		matchedAny := false
		for _, child := range condition.Children {
			matched, err := Evaluate(child, facts, now)
			if err != nil {
				return false, err
			}
			if matched {
				matchedAny = true
				break
			}
		}
		if condition.Match == MatchNone {
			return !matchedAny, nil
		}
		return matchedAny, nil
	default:
		return false, fmt.Errorf("未知的组合方式 %q", condition.Match)
	}
}

func evaluateLeaf(condition Condition, facts NodeFacts, now time.Time) (bool, error) {
	value := facts.Value(condition.FieldName)
	if condition.MaxAgeSeconds > 0 {
		value = value.Aged(time.Duration(condition.MaxAgeSeconds)*time.Second, now)
	}
	switch condition.Op {
	case OpIsUnknown:
		return condition.Negate != !value.Known, nil
	case OpIsKnown:
		return condition.Negate != value.Known, nil
	}
	if !value.Known {
		// Deliberately before Negate: "not slow" must not become true just
		// because the node was never measured.
		return false, nil
	}
	matched, err := compare(value, condition)
	if err != nil {
		return false, err
	}
	return condition.Negate != matched, nil
}

func compare(value Value, condition Condition) (bool, error) {
	switch value.Kind {
	case KindInt:
		return compareInt(value.Num, condition)
	case KindBool:
		return compareBool(value.Bool, condition)
	case KindSet:
		return compareSet(value.Set, condition)
	default:
		return compareString(value.Str, condition)
	}
}

func compareInt(actual int64, condition Condition) (bool, error) {
	switch condition.Op {
	case OpIn, OpNotIn:
		found := false
		for _, operand := range condition.Values {
			if wanted, ok := operandInt(operand); ok && wanted == actual {
				found = true
				break
			}
		}
		return found == (condition.Op == OpIn), nil
	case OpBetween:
		if len(condition.Values) != 2 {
			return false, fmt.Errorf("字段 %q 的区间需要两个边界值", condition.FieldName)
		}
		low, lowOK := operandInt(condition.Values[0])
		high, highOK := operandInt(condition.Values[1])
		if !lowOK || !highOK {
			return false, fmt.Errorf("字段 %q 的区间边界不是整数", condition.FieldName)
		}
		if low > high {
			low, high = high, low
		}
		return actual >= low && actual <= high, nil
	}
	wanted, ok := operandInt(condition.Value)
	if !ok {
		return false, fmt.Errorf("字段 %q 需要整数比较值", condition.FieldName)
	}
	switch condition.Op {
	case OpEq:
		return actual == wanted, nil
	case OpNe:
		return actual != wanted, nil
	case OpGt:
		return actual > wanted, nil
	case OpGte:
		return actual >= wanted, nil
	case OpLt:
		return actual < wanted, nil
	case OpLte:
		return actual <= wanted, nil
	default:
		return false, fmt.Errorf("整数字段 %q 不支持操作符 %q", condition.FieldName, condition.Op)
	}
}

func compareBool(actual bool, condition Condition) (bool, error) {
	wanted, ok := operandBool(condition.Value)
	if !ok {
		return false, fmt.Errorf("字段 %q 需要布尔比较值", condition.FieldName)
	}
	switch condition.Op {
	case OpEq:
		return actual == wanted, nil
	case OpNe:
		return actual != wanted, nil
	default:
		return false, fmt.Errorf("布尔字段 %q 不支持操作符 %q", condition.FieldName, condition.Op)
	}
}

// compareSet treats "contains" as element membership and "in" as a non-empty
// intersection, which is what an operator means by "属于" on a multi-valued fact.
func compareSet(actual []string, condition Condition) (bool, error) {
	switch condition.Op {
	case OpContains, OpNotContains:
		wanted, ok := operandString(condition.Value)
		if !ok {
			return false, fmt.Errorf("字段 %q 需要文本比较值", condition.FieldName)
		}
		return containsFold(actual, wanted) == (condition.Op == OpContains), nil
	case OpIn, OpNotIn:
		found := false
		for _, operand := range condition.Values {
			wanted, ok := operandString(operand)
			if !ok {
				continue
			}
			if containsFold(actual, wanted) {
				found = true
				break
			}
		}
		return found == (condition.Op == OpIn), nil
	default:
		return false, fmt.Errorf("集合字段 %q 不支持操作符 %q", condition.FieldName, condition.Op)
	}
}

func compareString(actual string, condition Condition) (bool, error) {
	switch condition.Op {
	case OpIn, OpNotIn:
		found := false
		for _, operand := range condition.Values {
			if wanted, ok := operandString(operand); ok && strings.EqualFold(wanted, actual) {
				found = true
				break
			}
		}
		return found == (condition.Op == OpIn), nil
	case OpBetween:
		if len(condition.Values) != 2 {
			return false, fmt.Errorf("字段 %q 的区间需要两个边界值", condition.FieldName)
		}
		low, lowOK := operandString(condition.Values[0])
		high, highOK := operandString(condition.Values[1])
		if !lowOK || !highOK {
			return false, fmt.Errorf("字段 %q 的区间边界不是文本", condition.FieldName)
		}
		if low > high {
			low, high = high, low
		}
		return actual >= low && actual <= high, nil
	}
	wanted, ok := operandString(condition.Value)
	if !ok {
		return false, fmt.Errorf("字段 %q 需要文本比较值", condition.FieldName)
	}
	switch condition.Op {
	case OpEq:
		return strings.EqualFold(actual, wanted), nil
	case OpNe:
		return !strings.EqualFold(actual, wanted), nil
	case OpContains:
		return containsFoldText(actual, wanted), nil
	case OpNotContains:
		return !containsFoldText(actual, wanted), nil
	case OpRegex:
		expression, err := regexp.Compile(wanted)
		if err != nil {
			return false, fmt.Errorf("字段 %q 的正则表达式无法编译: %w", condition.FieldName, err)
		}
		return expression.MatchString(actual), nil
	case OpGt:
		return actual > wanted, nil
	case OpGte:
		return actual >= wanted, nil
	case OpLt:
		return actual < wanted, nil
	case OpLte:
		return actual <= wanted, nil
	default:
		return false, fmt.Errorf("文本字段 %q 不支持操作符 %q", condition.FieldName, condition.Op)
	}
}

// Text comparisons are case-insensitive on purpose: region codes, ISO codes,
// unlock statuses and risk levels are stored with inconsistent casing across
// providers, and an operator typing "hk" means the same thing as "HK". A rule
// that needs exact casing uses regex, where the pattern is under its control.
func containsFoldText(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

// operandInt accepts the shapes a JSON decoder can produce for a number, plus a
// numeric string, so a rule authored by hand is not rejected over 100 vs "100".
func operandInt(operand any) (int64, bool) {
	switch typed := operand.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float32:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case stdjson.Number:
		parsed, err := typed.Int64()
		if err != nil {
			floating, floatErr := typed.Float64()
			if floatErr != nil {
				return 0, false
			}
			return int64(floating), true
		}
		return parsed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return parsed, true
		}
		if floating, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return int64(floating), true
		}
	}
	return 0, false
}

func operandBool(operand any) (bool, bool) {
	switch typed := operand.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		if err != nil {
			return false, false
		}
		return parsed, true
	}
	if number, ok := operandInt(operand); ok {
		return number != 0, true
	}
	return false, false
}

func operandString(operand any) (string, bool) {
	switch typed := operand.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case stdjson.Number:
		return typed.String(), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32), true
	case int:
		return strconv.Itoa(typed), true
	case int32:
		return strconv.FormatInt(int64(typed), 10), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	}
	return "", false
}
