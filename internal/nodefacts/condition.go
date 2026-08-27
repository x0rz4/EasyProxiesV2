package nodefacts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	json "easy_proxies/internal/jsonx"
)

// Match combinators. "none" is !any over the children, not a per-leaf negation.
const (
	MatchAll  = "all"
	MatchAny  = "any"
	MatchNone = "none"
)

// Condition is one node of the rule AST. A node is either a group (Match set,
// Children non-empty) or a leaf (Field + Op set); never both.
type Condition struct {
	Match    string      `json:"match,omitempty"`
	Children []Condition `json:"children,omitempty"`

	// FieldName is the registry key of the fact being compared. It is named
	// FieldName rather than Field so it does not read as the Field type.
	FieldName string   `json:"field,omitempty"`
	Op        Operator `json:"op,omitempty"`
	Value     any      `json:"value,omitempty"`
	Values    []any    `json:"values,omitempty"`

	// MaxAgeSeconds turns a measurement older than the window into an unknown
	// fact, so a stale success stops matching instead of matching forever.
	MaxAgeSeconds int64 `json:"max_age_seconds,omitempty"`
	// Negate flips the leaf result after the unknown short-circuit, so negating
	// an unknown fact still yields false.
	Negate bool `json:"negate,omitempty"`
}

// Limits bound a rule so a single condition cannot turn a recompute into a
// denial of service. Values are enforced by Validate, never silently clamped.
type Limits struct {
	MaxConditions  int
	MaxDepth       int
	MaxValueItems  int
	MaxRegexLength int
	MaxRuleBytes   int
}

// DefaultLimits are the limits the HTTP layer and the tag service use.
func DefaultLimits() Limits {
	return Limits{
		MaxConditions:  50,
		MaxDepth:       3,
		MaxValueItems:  100,
		MaxRegexLength: 200,
		MaxRuleBytes:   16384,
	}
}

// IsGroup reports whether the node combines children.
func (c Condition) IsGroup() bool { return c.Match != "" }

// IsLeaf reports whether the node is a field comparison.
func (c Condition) IsLeaf() bool { return c.Match == "" && c.FieldName != "" }

// IsEmpty reports whether the node carries neither a combinator nor a field,
// which is how "this tag has no rule" is represented.
func (c Condition) IsEmpty() bool {
	return c.Match == "" && c.FieldName == "" && c.Op == "" && len(c.Children) == 0
}

// ParseRule decodes a stored rule. An empty or whitespace-only payload is a
// valid "no rule" condition rather than an error.
func ParseRule(data []byte) (Condition, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Condition{}, nil
	}
	var condition Condition
	if err := json.Unmarshal(data, &condition); err != nil {
		return Condition{}, fmt.Errorf("规则 JSON 解析失败: %w", err)
	}
	return condition, nil
}

// MarshalRule encodes a rule with sorted keys so an unchanged rule always
// serializes to the same bytes (rule_version only advances on real edits).
func MarshalRule(condition Condition) ([]byte, error) {
	if condition.IsEmpty() {
		return nil, nil
	}
	return json.MarshalCanonical(condition)
}

// ReferencedFields returns the sorted, deduplicated field names the rule reads.
// Previews use it to return only the facts that actually drove the decision.
func ReferencedFields(condition Condition) []string {
	seen := map[string]struct{}{}
	collectFields(condition, seen)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectFields(condition Condition, seen map[string]struct{}) {
	if condition.FieldName != "" {
		seen[condition.FieldName] = struct{}{}
	}
	for _, child := range condition.Children {
		collectFields(child, seen)
	}
}

// Validate checks the rule against the field registry and the limits. It is the
// only gate between operator input and the evaluator, so it verifies structure,
// field existence, field/operator compatibility, value arity and value types.
func (c Condition) Validate(registry *Registry, limits Limits) error {
	if c.IsEmpty() {
		return nil
	}
	if limits.MaxRuleBytes > 0 {
		encoded, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("规则序列化失败: %w", err)
		}
		if len(encoded) > limits.MaxRuleBytes {
			return fmt.Errorf("规则过大: %d 字节，上限 %d 字节", len(encoded), limits.MaxRuleBytes)
		}
	}
	count := 0
	return c.validate(registry, limits, 1, &count)
}

func (c Condition) validate(registry *Registry, limits Limits, depth int, count *int) error {
	if limits.MaxDepth > 0 && depth > limits.MaxDepth {
		return fmt.Errorf("规则嵌套层数超过上限 %d", limits.MaxDepth)
	}
	*count++
	if limits.MaxConditions > 0 && *count > limits.MaxConditions {
		return fmt.Errorf("规则条件数超过上限 %d", limits.MaxConditions)
	}
	if c.IsGroup() {
		return c.validateGroup(registry, limits, depth, count)
	}
	if c.FieldName == "" {
		return fmt.Errorf("条件缺少字段名")
	}
	if len(c.Children) > 0 {
		return fmt.Errorf("字段条件 %q 不能带子条件", c.FieldName)
	}
	return c.validateLeaf(registry, limits)
}

func (c Condition) validateGroup(registry *Registry, limits Limits, depth int, count *int) error {
	switch c.Match {
	case MatchAll, MatchAny, MatchNone:
	default:
		return fmt.Errorf("未知的组合方式 %q，只支持 all/any/none", c.Match)
	}
	if c.FieldName != "" || c.Op != "" {
		return fmt.Errorf("组合条件不能同时携带字段 %q", c.FieldName)
	}
	if len(c.Children) == 0 {
		return fmt.Errorf("组合条件 %q 至少需要一个子条件", c.Match)
	}
	for index, child := range c.Children {
		if err := child.validate(registry, limits, depth+1, count); err != nil {
			return fmt.Errorf("第 %d 个子条件: %w", index+1, err)
		}
	}
	return nil
}

func (c Condition) validateLeaf(registry *Registry, limits Limits) error {
	field, ok := registry.Field(c.FieldName)
	if !ok {
		return fmt.Errorf("未知字段 %q", c.FieldName)
	}
	arity, ok := OperatorArity(c.Op)
	if !ok {
		return fmt.Errorf("未知操作符 %q", c.Op)
	}
	if !field.SupportsOperator(c.Op) {
		return fmt.Errorf("字段 %q 不支持操作符 %q", c.FieldName, c.Op)
	}
	if c.MaxAgeSeconds < 0 {
		return fmt.Errorf("字段 %q 的事实时效不能为负数", c.FieldName)
	}
	if c.MaxAgeSeconds > 0 && !field.SupportsMaxAge {
		return fmt.Errorf("字段 %q 不是检测结果，无法设置事实时效", c.FieldName)
	}
	if err := c.validateArity(field, arity, limits); err != nil {
		return err
	}
	if c.Op == OpRegex {
		return validateRegex(c.Value, limits)
	}
	return nil
}

// validateArity checks how many values the operator needs and that each one fits
// the field's value domain, so a malformed rule fails at save time instead of
// silently never matching.
func (c Condition) validateArity(field Field, arity string, limits Limits) error {
	switch arity {
	case ArityNone:
		if c.Value != nil || len(c.Values) > 0 {
			return fmt.Errorf("操作符 %q 不接受比较值", c.Op)
		}
		return nil
	case ArityOne:
		if c.Value == nil {
			return fmt.Errorf("字段 %q 的操作符 %q 需要一个比较值", c.FieldName, c.Op)
		}
		if len(c.Values) > 0 {
			return fmt.Errorf("字段 %q 的操作符 %q 只接受单个比较值", c.FieldName, c.Op)
		}
		return validateOperand(field, c.Op, c.Value)
	case ArityTwo:
		if len(c.Values) != 2 {
			return fmt.Errorf("字段 %q 的操作符 %q 需要两个边界值", c.FieldName, c.Op)
		}
		for _, operand := range c.Values {
			if err := validateOperand(field, c.Op, operand); err != nil {
				return err
			}
		}
		return nil
	case ArityMany:
		if len(c.Values) == 0 {
			return fmt.Errorf("字段 %q 的操作符 %q 需要至少一个比较值", c.FieldName, c.Op)
		}
		if limits.MaxValueItems > 0 && len(c.Values) > limits.MaxValueItems {
			return fmt.Errorf("字段 %q 的取值列表有 %d 项，上限 %d 项",
				c.FieldName, len(c.Values), limits.MaxValueItems)
		}
		for _, operand := range c.Values {
			if err := validateOperand(field, c.Op, operand); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("操作符 %q 的取值个数未定义", c.Op)
	}
}

// validateOperand rejects operands the evaluator could only ever read as a zero
// value. A set field is compared against its element type, which is a string.
func validateOperand(field Field, op Operator, operand any) error {
	switch field.Kind {
	case KindInt:
		if _, ok := operandInt(operand); !ok {
			return fmt.Errorf("字段 %q 需要整数比较值，收到 %v", field.Name, operand)
		}
	case KindBool:
		if _, ok := operandBool(operand); !ok {
			return fmt.Errorf("字段 %q 需要布尔比较值，收到 %v", field.Name, operand)
		}
	default:
		if _, ok := operandString(operand); !ok {
			return fmt.Errorf("字段 %q 需要文本比较值，收到 %v", field.Name, operand)
		}
	}
	if op == OpRegex && field.Kind != KindString && field.Kind != KindEnum && field.Kind != KindSet {
		return fmt.Errorf("字段 %q 不支持正则匹配", field.Name)
	}
	return nil
}

// validateRegex compiles the pattern here so an invalid one can never reach a
// recompute, and bounds its length so a pathological pattern cannot be stored.
func validateRegex(operand any, limits Limits) error {
	pattern, ok := operandString(operand)
	if !ok {
		return fmt.Errorf("正则表达式必须是文本")
	}
	if limits.MaxRegexLength > 0 && utf8.RuneCountInString(pattern) > limits.MaxRegexLength {
		return fmt.Errorf("正则表达式长度 %d 超过上限 %d",
			utf8.RuneCountInString(pattern), limits.MaxRegexLength)
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("正则表达式无法编译: %w", err)
	}
	return nil
}
