package nodefacts

import "sort"

// Operator is a leaf comparison. is_unknown/is_known are the only operators that
// can observe an unknown fact.
type Operator string

const (
	OpEq          Operator = "eq"
	OpNe          Operator = "ne"
	OpIn          Operator = "in"
	OpNotIn       Operator = "not_in"
	OpContains    Operator = "contains"
	OpNotContains Operator = "not_contains"
	OpGt          Operator = "gt"
	OpGte         Operator = "gte"
	OpLt          Operator = "lt"
	OpLte         Operator = "lte"
	OpBetween     Operator = "between"
	OpRegex       Operator = "regex"
	OpIsUnknown   Operator = "is_unknown"
	OpIsKnown     Operator = "is_known"
)

// OperatorDef describes an operator for the schema endpoint. ValueArity is the
// number of entries the operator reads from Condition.Values ("1" means it reads
// Condition.Value instead).
type OperatorDef struct {
	Value      Operator `json:"value"`
	Label      string   `json:"label"`
	ValueArity string   `json:"value_arity"` // one | many | two | none
}

// ValueArity values.
const (
	ArityOne  = "one"
	ArityMany = "many"
	ArityTwo  = "two"
	ArityNone = "none"
)

var operatorDefs = []OperatorDef{
	{OpEq, "等于", ArityOne},
	{OpNe, "不等于", ArityOne},
	{OpIn, "属于", ArityMany},
	{OpNotIn, "不属于", ArityMany},
	{OpContains, "包含", ArityOne},
	{OpNotContains, "不包含", ArityOne},
	{OpGt, "大于", ArityOne},
	{OpGte, "大于等于", ArityOne},
	{OpLt, "小于", ArityOne},
	{OpLte, "小于等于", ArityOne},
	{OpBetween, "介于", ArityTwo},
	{OpRegex, "正则匹配", ArityOne},
	{OpIsUnknown, "未检测", ArityNone},
	{OpIsKnown, "已检测", ArityNone},
}

// Operators returns every operator definition in display order.
func Operators() []OperatorDef {
	out := make([]OperatorDef, len(operatorDefs))
	copy(out, operatorDefs)
	return out
}

// OperatorArity returns the arity of an operator and whether it is known.
func OperatorArity(op Operator) (string, bool) {
	for _, def := range operatorDefs {
		if def.Value == op {
			return def.ValueArity, true
		}
	}
	return "", false
}

// Field groups, used by the UI to bucket the field picker.
const (
	GroupBasic   = "基础"
	GroupPerf    = "性能"
	GroupIP      = "IP 质量"
	GroupUnlock  = "解锁"
	GroupSources = "来源"
)

// Enum keys let the HTTP layer attach option lists that only it can build
// (provider registries, distinct node columns, subscription names).
const (
	EnumRegion           = "region"
	EnumCountryCode      = "country_code"
	EnumProtocol         = "protocol"
	EnumNodeSource       = "node_source"
	EnumIPFamily         = "ip_family"
	EnumRiskLevel        = "risk_level"
	EnumUnlockStatus     = "unlock_status"
	EnumUnlockProvider   = "unlock_provider"
	EnumTagName          = "tag_name"
	EnumSubscriptionID   = "subscription_id"
	EnumSubscriptionName = "subscription_name"
)

// Field is one addressable fact.
type Field struct {
	Name string `json:"name"`
	// Label and Group are display metadata; Source documents where the fact is
	// read from so an operator can tell why it is unknown.
	Label          string     `json:"label"`
	Group          string     `json:"group"`
	Kind           Kind       `json:"kind"`
	Operators      []Operator `json:"operators"`
	SupportsMaxAge bool       `json:"supports_max_age"`
	Unit           string     `json:"unit,omitempty"`
	EnumKey        string     `json:"enum_key,omitempty"`
	Source         string     `json:"source,omitempty"`
}

// Registry is the set of fields a condition may reference. Rules are validated
// against it, so an unregistered field is a hard error rather than a silently
// unknown fact.
type Registry struct {
	fields map[string]Field
	order  []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{fields: map[string]Field{}}
}

// Register adds or replaces a field, preserving first-registration order.
func (r *Registry) Register(field Field) {
	if field.Name == "" {
		return
	}
	if len(field.Operators) == 0 {
		field.Operators = OperatorsForKind(field.Kind)
	}
	if _, exists := r.fields[field.Name]; !exists {
		r.order = append(r.order, field.Name)
	}
	r.fields[field.Name] = field
}

// Field looks up a field by name.
func (r *Registry) Field(name string) (Field, bool) {
	if r == nil {
		return Field{}, false
	}
	field, ok := r.fields[name]
	return field, ok
}

// Fields returns every field in registration order.
func (r *Registry) Fields() []Field {
	if r == nil {
		return nil
	}
	out := make([]Field, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.fields[name])
	}
	return out
}

// Groups returns the field group names in registration order.
func (r *Registry) Groups() []string {
	seen := map[string]struct{}{}
	var groups []string
	for _, field := range r.Fields() {
		if _, exists := seen[field.Group]; exists {
			continue
		}
		seen[field.Group] = struct{}{}
		groups = append(groups, field.Group)
	}
	return groups
}

// OperatorsForKind returns the default operator set of a value domain.
func OperatorsForKind(kind Kind) []Operator {
	switch kind {
	case KindInt:
		return []Operator{OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpBetween, OpIn, OpNotIn, OpIsKnown, OpIsUnknown}
	case KindBool:
		return []Operator{OpEq, OpNe, OpIsKnown, OpIsUnknown}
	case KindEnum:
		return []Operator{OpEq, OpNe, OpIn, OpNotIn, OpIsKnown, OpIsUnknown}
	case KindSet:
		return []Operator{OpContains, OpNotContains, OpIn, OpNotIn, OpIsKnown, OpIsUnknown}
	default:
		return []Operator{OpEq, OpNe, OpIn, OpNotIn, OpContains, OpNotContains, OpRegex, OpIsKnown, OpIsUnknown}
	}
}

// SupportsOperator reports whether the field accepts the operator.
func (f Field) SupportsOperator(op Operator) bool {
	for _, candidate := range f.Operators {
		if candidate == op {
			return true
		}
	}
	return false
}

// sortedNames is only used by tests and error messages that need stability.
func sortedNames(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	sort.Strings(out)
	return out
}

// Field names. Dynamic per-provider fields are built by UnlockField and
// IPQualityField so a newly registered checker needs no code change here.
const (
	FieldNodeName           = "node.name"
	FieldNodeCountry        = "node.country"
	FieldNodeRegion         = "node.region"
	FieldNodeProtocol       = "node.protocol"
	FieldNodePort           = "node.port"
	FieldNodeEnabled        = "node.enabled"
	FieldNodeSource         = "node.source"
	FieldSubscriptionIDs    = "node.subscription_ids"
	FieldSubscriptionNames  = "node.subscription_names"
	FieldManualTags         = "tags.manual"
	FieldLatencyMs          = "latency_ms"
	FieldAvailable          = "available"
	FieldBlacklisted        = "blacklisted"
	FieldFailureCount       = "failure_count"
	FieldSuccessCount       = "success_count"
	FieldSpeedBps           = "speed_bps"
	FieldSpeedPeakBps       = "speed_peak_bps"
	FieldExitCountryCode    = "exit_country_code"
	FieldExitIPFamily       = "exit_ip_family"
	FieldUnlockIPPure       = "unlock.ip.pure"
	FieldUnlockIPRiskLevel  = "unlock.ip.risk_level"
	FieldUnlockIPType       = "unlock.ip.ip_type"
	FieldUnlockIPUsageType  = "unlock.ip.usage_type"
	FieldUnlockIPFraudScore = "unlock.ip.fraud_score"
	FieldUnlockIPASN        = "unlock.ip.asn"
	FieldUnlockIPOrg        = "unlock.ip.org"
	FieldUnlockedCount      = "unlock.unlocked_count"
	FieldUnlockedProviders  = "unlock.unlocked_providers"
	FieldIPQMaxFraudScore   = "ipq.max.fraud_score"
)

// UnlockField builds the field name of a per-provider unlock attribute, e.g.
// UnlockField("netflix", "status") == "unlock.netflix.status".
func UnlockField(provider, attribute string) string {
	return "unlock." + provider + "." + attribute
}

// IPQualityField builds the field name of a per-provider IP quality attribute.
func IPQualityField(provider, attribute string) string {
	return "ipq." + provider + "." + attribute
}

// ReducedIPQualityField builds a cross-provider reduction field name. The
// reduction is part of the name (ipq.any.proxy) on purpose: an implicit merge
// would change meaning whenever the enabled provider set changes.
func ReducedIPQualityField(reduction, attribute string) string {
	return "ipq." + reduction + "." + attribute
}

// Cross-provider reductions.
const (
	ReduceAny = "any"
	ReduceAll = "all"
	ReduceMax = "max"
)

// ProviderInfo names an unlock or IP quality provider for registry building.
type ProviderInfo struct {
	Name  string
	Label string
}

// RegistryOption customizes DefaultRegistry.
type RegistryOption func(*Registry)

// WithUnlockProviders registers unlock.<provider>.status/.region/.detail.
func WithUnlockProviders(providers []ProviderInfo) RegistryOption {
	return func(registry *Registry) {
		for _, provider := range providers {
			label := provider.Label
			if label == "" {
				label = provider.Name
			}
			registry.Register(Field{
				Name: UnlockField(provider.Name, "status"), Label: label + " 解锁状态",
				Group: GroupUnlock, Kind: KindEnum, SupportsMaxAge: true,
				EnumKey: EnumUnlockStatus, Source: "node_unlock_results.result_json services[]",
			})
			registry.Register(Field{
				Name: UnlockField(provider.Name, "region"), Label: label + " 解锁区域",
				Group: GroupUnlock, Kind: KindString, SupportsMaxAge: true,
				Source: "node_unlock_results.result_json services[].region",
			})
			registry.Register(Field{
				Name: UnlockField(provider.Name, "detail"), Label: label + " 检测摘要",
				Group: GroupUnlock, Kind: KindString, SupportsMaxAge: true,
				Source: "node_unlock_results.result_json services[].detail",
			})
		}
	}
}

// WithIPQualityProviders registers the per-provider ipq.* fields plus the
// explicit cross-provider reductions over the same provider set.
func WithIPQualityProviders(providers []ProviderInfo) RegistryOption {
	return func(registry *Registry) {
		for _, provider := range providers {
			label := provider.Label
			if label == "" {
				label = provider.Name
			}
			source := "node_ip_quality_results (provider=" + provider.Name + ")"
			registry.Register(Field{
				Name: IPQualityField(provider.Name, "fraud_score"), Label: label + " 风险分",
				Group: GroupIP, Kind: KindInt, SupportsMaxAge: true, Source: source,
			})
			for _, flag := range []struct{ attribute, label string }{
				{"is_residential", "住宅 IP"},
				{"proxy", "代理"},
				{"hosting", "机房"},
				{"mobile", "移动网络"},
				{"is_broadcast", "广播 IP"},
			} {
				registry.Register(Field{
					Name: IPQualityField(provider.Name, flag.attribute), Label: label + " " + flag.label,
					Group: GroupIP, Kind: KindBool, SupportsMaxAge: true, Source: source,
				})
			}
			for _, text := range []struct {
				attribute, label, enumKey string
			}{
				{"asn", "ASN", ""},
				{"org", "组织", ""},
				{"isp", "ISP", ""},
				{"country_code", "国家代码", EnumCountryCode},
			} {
				kind := KindString
				if text.enumKey != "" {
					kind = KindEnum
				}
				registry.Register(Field{
					Name: IPQualityField(provider.Name, text.attribute), Label: label + " " + text.label,
					Group: GroupIP, Kind: kind, SupportsMaxAge: true,
					EnumKey: text.enumKey, Source: source,
				})
			}
		}
		if len(providers) == 0 {
			return
		}
		names := make([]string, 0, len(providers))
		for _, provider := range providers {
			names = append(names, provider.Name)
		}
		participants := "参与提供商: " + joinNames(sortedNames(names))
		registry.Register(Field{
			Name: FieldIPQMaxFraudScore, Label: "风险分（任一提供商的最高值）",
			Group: GroupIP, Kind: KindInt, SupportsMaxAge: true, Source: participants,
		})
		for _, flag := range []struct{ attribute, label string }{
			{"is_residential", "住宅 IP"},
			{"proxy", "代理"},
			{"hosting", "机房"},
		} {
			registry.Register(Field{
				Name:  ReducedIPQualityField(ReduceAny, flag.attribute),
				Label: flag.label + "（任一提供商判定为是）",
				Group: GroupIP, Kind: KindBool, SupportsMaxAge: true, Source: participants,
			})
			registry.Register(Field{
				Name:  ReducedIPQualityField(ReduceAll, flag.attribute),
				Label: flag.label + "（全部提供商判定为是）",
				Group: GroupIP, Kind: KindBool, SupportsMaxAge: true, Source: participants,
			})
		}
	}
}

func joinNames(names []string) string {
	out := ""
	for index, name := range names {
		if index > 0 {
			out += ", "
		}
		out += name
	}
	return out
}

// DefaultRegistry returns the fields every rule may reference. Provider-specific
// fields are added through options so this package never imports the checker
// registries (which would invert the dependency and pull HTTP clients into a
// pure evaluation package).
//
// tags.auto is deliberately absent: letting an auto tag depend on another auto
// tag would make a recompute order-dependent and non-idempotent. tags.manual is
// operator input rather than engine output, so it is safe to read.
func DefaultRegistry(options ...RegistryOption) *Registry {
	registry := NewRegistry()
	registerBasicFields(registry)
	registerPerformanceFields(registry)
	registerIPFields(registry)
	registerUnlockFields(registry)
	for _, option := range options {
		option(registry)
	}
	return registry
}

func registerBasicFields(registry *Registry) {
	for _, field := range []Field{
		{Name: FieldNodeName, Label: "节点名称", Group: GroupBasic, Kind: KindString, Source: "nodes.name"},
		{Name: FieldNodeRegion, Label: "地区", Group: GroupBasic, Kind: KindEnum,
			EnumKey: EnumRegion, Source: "nodes.region（空值归一为 other）"},
		{Name: FieldNodeCountry, Label: "国家/地区名", Group: GroupBasic, Kind: KindString, Source: "nodes.country"},
		{Name: FieldNodeProtocol, Label: "协议", Group: GroupBasic, Kind: KindEnum,
			EnumKey: EnumProtocol, Source: "nodes.uri 的协议头"},
		{Name: FieldNodePort, Label: "端口", Group: GroupBasic, Kind: KindInt, Source: "nodes.port"},
		{Name: FieldNodeEnabled, Label: "已启用", Group: GroupBasic, Kind: KindBool, Source: "nodes.enabled"},
		{Name: FieldNodeSource, Label: "节点来源", Group: GroupBasic, Kind: KindEnum,
			EnumKey: EnumNodeSource, Source: "nodes.source"},
		{Name: FieldManualTags, Label: "人工标签", Group: GroupBasic, Kind: KindSet,
			EnumKey: EnumTagName, Source: "node_tags (source=manual)"},
		{Name: FieldSubscriptionIDs, Label: "所属订阅 ID", Group: GroupSources, Kind: KindSet,
			EnumKey: EnumSubscriptionID, Source: "subscription_nodes.subscription_id"},
		{Name: FieldSubscriptionNames, Label: "所属订阅名称", Group: GroupSources, Kind: KindSet,
			EnumKey: EnumSubscriptionName, Source: "subscriptions.name"},
	} {
		registry.Register(field)
	}
}

func registerPerformanceFields(registry *Registry) {
	for _, field := range []Field{
		{Name: FieldLatencyMs, Label: "延迟", Group: GroupPerf, Kind: KindInt, Unit: "ms",
			SupportsMaxAge: true,
			Source:         "node_detection_results.latency_ms 优先，回退 node_stats.last_latency_ms（-1 视为未检测）"},
		{Name: FieldAvailable, Label: "可用", Group: GroupPerf, Kind: KindBool, SupportsMaxAge: true,
			Source: "node_stats.available（仅在 initial_check_done=1 时视为已检测）"},
		{Name: FieldBlacklisted, Label: "已拉黑", Group: GroupPerf, Kind: KindBool,
			Source: "node_stats.blacklisted"},
		{Name: FieldFailureCount, Label: "失败次数", Group: GroupPerf, Kind: KindInt,
			Source: "node_stats.failure_count"},
		{Name: FieldSuccessCount, Label: "成功次数", Group: GroupPerf, Kind: KindInt,
			Source: "node_stats.success_count"},
		{Name: FieldSpeedBps, Label: "平均速度", Group: GroupPerf, Kind: KindInt, Unit: "B/s",
			SupportsMaxAge: true, Source: "node_detection_results.average_bytes_per_second"},
		{Name: FieldSpeedPeakBps, Label: "峰值速度", Group: GroupPerf, Kind: KindInt, Unit: "B/s",
			SupportsMaxAge: true, Source: "node_detection_results.peak_bytes_per_second"},
	} {
		registry.Register(field)
	}
}

func registerIPFields(registry *Registry) {
	for _, field := range []Field{
		{Name: FieldExitCountryCode, Label: "落地国家代码", Group: GroupIP, Kind: KindEnum,
			EnumKey: EnumCountryCode, SupportsMaxAge: true,
			Source: "node_detection_results.exit_country_code（exit_ip_status=success）"},
		{Name: FieldExitIPFamily, Label: "落地 IP 协议族", Group: GroupIP, Kind: KindEnum,
			EnumKey: EnumIPFamily, SupportsMaxAge: true, Source: "node_detection_results.exit_ip_family"},
		{Name: FieldUnlockIPPure, Label: "原生 IP", Group: GroupIP, Kind: KindBool, SupportsMaxAge: true,
			Source: "node_unlock_results.ip_pure"},
		{Name: FieldUnlockIPRiskLevel, Label: "风险等级", Group: GroupIP, Kind: KindEnum,
			EnumKey: EnumRiskLevel, SupportsMaxAge: true, Source: "node_unlock_results.result_json ip.risk_level"},
		{Name: FieldUnlockIPType, Label: "IP 类型", Group: GroupIP, Kind: KindString, SupportsMaxAge: true,
			Source: "node_unlock_results.result_json ip.ip_type"},
		{Name: FieldUnlockIPUsageType, Label: "IP 用途", Group: GroupIP, Kind: KindString, SupportsMaxAge: true,
			Source: "node_unlock_results.result_json ip.usage_type"},
		{Name: FieldUnlockIPFraudScore, Label: "解锁检测风险分", Group: GroupIP, Kind: KindInt, SupportsMaxAge: true,
			Source: "node_unlock_results.result_json ip.fraud_score"},
		{Name: FieldUnlockIPASN, Label: "解锁检测 ASN", Group: GroupIP, Kind: KindString, SupportsMaxAge: true,
			Source: "node_unlock_results.result_json ip.asn"},
		{Name: FieldUnlockIPOrg, Label: "解锁检测组织", Group: GroupIP, Kind: KindString, SupportsMaxAge: true,
			Source: "node_unlock_results.result_json ip.org"},
	} {
		registry.Register(field)
	}
}

func registerUnlockFields(registry *Registry) {
	registry.Register(Field{
		Name: FieldUnlockedCount, Label: "已解锁服务数", Group: GroupUnlock, Kind: KindInt,
		SupportsMaxAge: true, Source: "node_unlock_results.result_json services[] 归约",
	})
	registry.Register(Field{
		Name: FieldUnlockedProviders, Label: "已解锁服务", Group: GroupUnlock, Kind: KindSet,
		EnumKey: EnumUnlockProvider, SupportsMaxAge: true,
		Source: "node_unlock_results.result_json services[] 归约",
	})
}
