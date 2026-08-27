package nodetag

import (
	"strings"

	"easy_proxies/internal/nodefacts"
)

// Builtin template keys. They identify a seeded tag independently of its display
// name, which an operator may rename, so seeding stays idempotent.
const (
	BuiltinNativeIP     = "native_ip"
	BuiltinRiskHigh     = "risk_high"
	BuiltinRiskLow      = "risk_low"
	BuiltinLatencyFast  = "latency_fast"
	BuiltinLatencyOK    = "latency_ok"
	BuiltinLatencySlow  = "latency_slow"
	BuiltinUnlockPrefix = "unlock_"
)

// Builtin mutex group names. A group is created on demand when a template that
// needs it is seeded.
const (
	MutexGroupRisk    = "风险等级"
	MutexGroupLatency = "延迟档"
)

// Template is a seedable tag: a name, a rule, and an optional mutex group named
// by string so the seeder can create the group on demand.
type Template struct {
	BuiltinKey  string
	Name        string
	Color       string
	Description string
	MutexGroup  string
	Priority    int
	Condition   nodefacts.Condition
}

// Templates returns the builtin templates. One unlock template is produced per
// provider, so registering a new checker adds a tag without a code change here.
//
// The latency thresholds are deliberately generous and overlapping: the mutex
// group turns "≤100 / ≤300 / >300" into three exclusive buckets by priority, and
// an operator retunes them by editing the rule rather than by editing this table.
func Templates(unlockProviders []nodefacts.ProviderInfo) []Template {
	templates := []Template{
		{
			BuiltinKey:  BuiltinNativeIP,
			Name:        "原生IP",
			Color:       "#22c55e",
			Description: "解锁检测判定为原生 IP",
			Condition:   leafValue(nodefacts.FieldUnlockIPPure, nodefacts.OpEq, true),
		},
		{
			BuiltinKey:  BuiltinRiskHigh,
			Name:        "高风险",
			Color:       "#ef4444",
			Description: "IP 风险等级为 High 或 Medium",
			MutexGroup:  MutexGroupRisk,
			Priority:    20,
			Condition: leafList(nodefacts.FieldUnlockIPRiskLevel, nodefacts.OpIn,
				"High", "Medium"),
		},
		{
			BuiltinKey:  BuiltinRiskLow,
			Name:        "低风险",
			Color:       "#3b82f6",
			Description: "IP 风险等级为 Low",
			MutexGroup:  MutexGroupRisk,
			Priority:    10,
			Condition:   leafList(nodefacts.FieldUnlockIPRiskLevel, nodefacts.OpIn, "Low"),
		},
		{
			BuiltinKey:  BuiltinLatencyFast,
			Name:        "⚡极速",
			Color:       "#22c55e",
			Description: "延迟不超过 100ms",
			MutexGroup:  MutexGroupLatency,
			Priority:    30,
			Condition:   leafValue(nodefacts.FieldLatencyMs, nodefacts.OpLte, 100),
		},
		{
			BuiltinKey:  BuiltinLatencyOK,
			Name:        "✅正常",
			Color:       "#0ea5e9",
			Description: "延迟不超过 300ms",
			MutexGroup:  MutexGroupLatency,
			Priority:    20,
			Condition:   leafValue(nodefacts.FieldLatencyMs, nodefacts.OpLte, 300),
		},
		{
			BuiltinKey:  BuiltinLatencySlow,
			Name:        "🐌较慢",
			Color:       "#f97316",
			Description: "延迟超过 300ms",
			MutexGroup:  MutexGroupLatency,
			Priority:    10,
			Condition:   leafValue(nodefacts.FieldLatencyMs, nodefacts.OpGt, 300),
		},
	}
	for _, provider := range unlockProviders {
		name := strings.ToLower(strings.TrimSpace(provider.Name))
		if name == "" {
			continue
		}
		label := strings.TrimSpace(provider.Label)
		if label == "" {
			label = name
		}
		templates = append(templates, Template{
			BuiltinKey:  UnlockTemplateKey(name),
			Name:        label + "解锁",
			Color:       "#a855f7",
			Description: label + " 解锁检测通过",
			Condition: leafValue(nodefacts.UnlockField(name, "status"),
				nodefacts.OpEq, unlockStatusUnlocked),
		})
	}
	return templates
}

// UnlockTemplateKey is the builtin key of a provider's unlock template.
func UnlockTemplateKey(provider string) string {
	return BuiltinUnlockPrefix + strings.ToLower(strings.TrimSpace(provider))
}

// unlockStatusUnlocked mirrors internal/unlock's vocabulary. It is duplicated
// rather than imported so this package does not pull every unlock checker (and
// its HTTP clients) into the tagging engine.
const unlockStatusUnlocked = "unlocked"

func leafValue(field string, op nodefacts.Operator, value any) nodefacts.Condition {
	return nodefacts.Condition{FieldName: field, Op: op, Value: value}
}

func leafList(field string, op nodefacts.Operator, values ...any) nodefacts.Condition {
	return nodefacts.Condition{FieldName: field, Op: op, Values: values}
}
