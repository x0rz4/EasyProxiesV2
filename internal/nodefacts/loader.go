package nodefacts

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/geoip"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/store"
)

// Stage statuses are persisted as bare strings by the detection pipeline, and
// the unlock status vocabulary lives in internal/unlock. Both are mirrored here
// as constants instead of imported: pulling internal/unlock in would drag every
// HTTP checker into a package whose whole point is to be pure.
const (
	detectionStatusSuccess = "success"
	unlockStatusUnlocked   = "unlocked"
)

// Source is the slice of the store the loader needs. It is an interface so a
// test can count queries and so this package cannot reach for anything else.
type Source interface {
	ListNodes(ctx context.Context, filter store.NodeFilter) ([]store.Node, error)
	ListNodeStats(ctx context.Context, nodeIDs []int64) (map[int64]*store.NodeStats, error)
	ListNodeDetectionResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64]*store.NodeDetectionResult, error)
	ListNodeIPQualityResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64][]store.NodeIPQualityResult, error)
	ListUnlockResultsByIDs(ctx context.Context, nodeIDs []int64) (map[int64]*store.UnlockResult, error)
	ListNodeSubscriptionIDs(ctx context.Context, nodeIDs []int64) (map[int64][]int64, error)
	ListNodeTags(ctx context.Context, filter store.NodeTagFilter) ([]store.NodeTag, error)
	ListTags(ctx context.Context) ([]store.Tag, error)
	ListSubscriptions(ctx context.Context) ([]store.Subscription, error)
}

// Loader turns stored rows into fact sets.
type Loader struct {
	source Source
}

// NewLoader returns a loader reading from source.
func NewLoader(source Source) *Loader {
	return &Loader{source: source}
}

// Load returns the fact set of every requested node, sorted by node ID. A nil
// nodeIDs means "every node".
//
// The query count is fixed — one batch per fact family, never one per node — so
// a recompute over 5000 nodes costs the same number of round trips as one over
// five. Each batch chunks its IN list inside the store.
func (l *Loader) Load(ctx context.Context, nodeIDs []int64) ([]NodeFacts, error) {
	nodes, err := l.source.ListNodes(ctx, store.NodeFilter{NodeIDs: nodeIDs})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	stats, err := l.source.ListNodeStats(ctx, ids)
	if err != nil {
		return nil, err
	}
	detections, err := l.source.ListNodeDetectionResultsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	qualities, err := l.source.ListNodeIPQualityResultsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	unlocks, err := l.source.ListUnlockResultsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	subscriptionIDs, err := l.source.ListNodeSubscriptionIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	manualTags, err := l.manualTagNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	subscriptionNames, err := l.subscriptionNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeFacts, 0, len(nodes))
	for _, node := range nodes {
		facts := NewNodeFacts(node.ID, node.Name, NormalizeRegion(node.Region))
		applyNodeFields(facts, node)
		applySourceFields(facts, subscriptionIDs[node.ID], subscriptionNames, manualTags[node.ID])
		applyStatsFields(facts, stats[node.ID])
		applyDetectionFields(facts, detections[node.ID], stats[node.ID])
		applyUnlockFields(facts, unlocks[node.ID])
		applyIPQualityFields(facts, qualities[node.ID])
		out = append(out, facts)
	}
	sort.Slice(out, func(first, second int) bool { return out[first].NodeID < out[second].NodeID })
	return out, nil
}

// NormalizeRegion lowercases a region code and maps the empty one to
// geoip.RegionOther, matching how the group builder buckets unclassified nodes.
func NormalizeRegion(region string) string {
	trimmed := strings.ToLower(strings.TrimSpace(region))
	if trimmed == "" {
		return geoip.RegionOther
	}
	return trimmed
}

func (l *Loader) manualTagNames(ctx context.Context, ids []int64) (map[int64][]string, error) {
	assignments, err := l.source.ListNodeTags(ctx, store.NodeTagFilter{
		NodeIDs: ids, Source: store.NodeTagSourceManual,
	})
	if err != nil {
		return nil, err
	}
	if len(assignments) == 0 {
		return map[int64][]string{}, nil
	}
	tags, err := l.source.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(tags))
	for _, tag := range tags {
		names[tag.ID] = tag.Name
	}
	out := make(map[int64][]string, len(assignments))
	for _, assignment := range assignments {
		name := names[assignment.TagID]
		if name == "" {
			continue
		}
		out[assignment.NodeID] = append(out[assignment.NodeID], name)
	}
	for nodeID := range out {
		sort.Strings(out[nodeID])
	}
	return out, nil
}

func (l *Loader) subscriptionNames(ctx context.Context) (map[int64]string, error) {
	subscriptions, err := l.source.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(subscriptions))
	for _, subscription := range subscriptions {
		out[subscription.ID] = subscription.Name
	}
	return out, nil
}

// knownText treats an empty column as an unmeasured fact rather than as the
// empty string, so is_unknown answers "was this ever detected?" truthfully.
func knownText(value string) Fact[string] {
	if strings.TrimSpace(value) == "" {
		return Unknown[string]()
	}
	return Known(value)
}

func knownTextAt(value string, checkedAt time.Time) Fact[string] {
	if strings.TrimSpace(value) == "" {
		return Unknown[string]()
	}
	return KnownAt(value, checkedAt)
}

// applyNodeFields loads configuration facts. They carry no CheckedAt, so
// max_age_seconds can never expire them — they are not measurements.
func applyNodeFields(facts NodeFacts, node store.Node) {
	facts.Set(FieldNodeName, StringValue(Known(node.Name)))
	facts.Set(FieldNodeRegion, EnumValue(Known(NormalizeRegion(node.Region))))
	facts.Set(FieldNodeCountry, StringValue(knownText(node.Country)))
	facts.Set(FieldNodeProtocol, EnumValue(knownText(uriProtocol(node.URI))))
	facts.Set(FieldNodeSource, EnumValue(knownText(node.Source)))
	facts.Set(FieldNodeEnabled, BoolValue(Known(node.Enabled)))
	if node.Port > 0 {
		facts.Set(FieldNodePort, IntValue(Known(int64(node.Port))))
	} else {
		facts.Set(FieldNodePort, IntValue(Unknown[int64]()))
	}
}

// uriProtocol reads the scheme off a node URI. Parsing the full identity would
// be more precise but costs a base64/JSON decode per node per recompute, and the
// scheme is exactly what an operator means by "协议".
func uriProtocol(uri string) string {
	scheme, _, found := strings.Cut(uri, "://")
	if !found {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(scheme))
}

// applySourceFields sets the membership facts. An empty set is known-empty, not
// unknown: "this node belongs to no subscription" is a measured answer.
func applySourceFields(facts NodeFacts, subscriptionIDs []int64, subscriptionNames map[int64]string, manualTags []string) {
	ids := make([]string, 0, len(subscriptionIDs))
	names := make([]string, 0, len(subscriptionIDs))
	for _, id := range subscriptionIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
		if name := subscriptionNames[id]; name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(ids)
	sort.Strings(names)
	facts.Set(FieldSubscriptionIDs, SetValue(Known(ids)))
	facts.Set(FieldSubscriptionNames, SetValue(Known(names)))
	facts.Set(FieldManualTags, SetValue(Known(append([]string(nil), manualTags...))))
}

// applyStatsFields loads the health-probe counters. available is only known once
// the first check has run, otherwise a fresh node would look explicitly down.
func applyStatsFields(facts NodeFacts, stats *store.NodeStats) {
	if stats == nil {
		facts.Set(FieldAvailable, BoolValue(Unknown[bool]()))
		facts.Set(FieldBlacklisted, BoolValue(Unknown[bool]()))
		facts.Set(FieldFailureCount, IntValue(Unknown[int64]()))
		facts.Set(FieldSuccessCount, IntValue(Unknown[int64]()))
		return
	}
	if stats.InitialCheckDone {
		facts.Set(FieldAvailable, BoolValue(KnownAt(stats.Available, stats.UpdatedAt)))
	} else {
		facts.Set(FieldAvailable, BoolValue(Unknown[bool]()))
	}
	facts.Set(FieldBlacklisted, BoolValue(Known(stats.Blacklisted)))
	facts.Set(FieldFailureCount, IntValue(Known(int64(stats.FailureCount))))
	facts.Set(FieldSuccessCount, IntValue(Known(int64(stats.SuccessCount))))
}

// applyDetectionFields loads the manual-diagnostics facts. Latency prefers the
// dedicated check (it carries its own timestamp, so max_age works) and falls back
// to the health probe's last sample, where -1 encodes "never measured".
func applyDetectionFields(facts NodeFacts, detection *store.NodeDetectionResult, stats *store.NodeStats) {
	latency := Unknown[int64]()
	switch {
	case detection != nil && detection.LatencyStatus == detectionStatusSuccess && detection.LatencyMs != nil:
		latency = KnownAt(*detection.LatencyMs, detection.LatencyCheckedAt)
	case stats != nil && stats.LastLatencyMs >= 0:
		latency = KnownAt(stats.LastLatencyMs, stats.UpdatedAt)
	}
	facts.Set(FieldLatencyMs, IntValue(latency))

	speed, peak := Unknown[int64](), Unknown[int64]()
	exitCountry, exitFamily := Unknown[string](), Unknown[string]()
	if detection != nil {
		if detection.SpeedStatus == detectionStatusSuccess {
			if detection.AverageBytesPerSecond != nil {
				speed = KnownAt(*detection.AverageBytesPerSecond, detection.SpeedCheckedAt)
			}
			if detection.PeakBytesPerSecond != nil {
				peak = KnownAt(*detection.PeakBytesPerSecond, detection.SpeedCheckedAt)
			}
		}
		if detection.ExitIPStatus == detectionStatusSuccess {
			exitCountry = knownTextAt(detection.ExitCountryCode, detection.ExitIPCheckedAt)
			exitFamily = knownTextAt(detection.ExitIPFamily, detection.ExitIPCheckedAt)
		}
	}
	facts.Set(FieldSpeedBps, IntValue(speed))
	facts.Set(FieldSpeedPeakBps, IntValue(peak))
	facts.Set(FieldExitCountryCode, EnumValue(exitCountry))
	facts.Set(FieldExitIPFamily, EnumValue(exitFamily))
}

// unlockPayload reads the extended IP classification out of the stored
// unlock.Result payload. The store only scans ip/country/iso_code/region/pure
// into columns, so risk_level and friends are only available from result_json.
type unlockPayload struct {
	IP struct {
		IP        string `json:"ip"`
		ASN       string `json:"asn"`
		Org       string `json:"org"`
		IPType    string `json:"ip_type"`
		UsageType string `json:"usage_type"`
		RiskLevel string `json:"risk_level"`
		// FraudScore is a pointer to separate "absent" from zero. The writer
		// omits a zero score, so a genuine 0 still reads as unmeasured — the
		// honest answer given what is stored.
		FraudScore *int `json:"fraud_score"`
	} `json:"ip"`
}

// applyUnlockFields loads the unlock report: the native-IP verdict, the extended
// IP classification, and one status/region/detail triple per service.
func applyUnlockFields(facts NodeFacts, result *store.UnlockResult) {
	if result == nil {
		return
	}
	checkedAt := result.CheckedAt
	if result.IP.IP != "" {
		facts.Set(FieldUnlockIPPure, BoolValue(KnownAt(result.IP.Pure, checkedAt)))
	}
	var payload unlockPayload
	if result.ResultJSON != "" {
		if err := json.Unmarshal([]byte(result.ResultJSON), &payload); err != nil {
			// A corrupt payload leaves the extended facts unknown instead of
			// failing the whole recompute for every other node.
			payload = unlockPayload{}
		}
	}
	facts.Set(FieldUnlockIPRiskLevel, EnumValue(knownTextAt(payload.IP.RiskLevel, checkedAt)))
	facts.Set(FieldUnlockIPType, StringValue(knownTextAt(payload.IP.IPType, checkedAt)))
	facts.Set(FieldUnlockIPUsageType, StringValue(knownTextAt(payload.IP.UsageType, checkedAt)))
	facts.Set(FieldUnlockIPASN, StringValue(knownTextAt(payload.IP.ASN, checkedAt)))
	facts.Set(FieldUnlockIPOrg, StringValue(knownTextAt(payload.IP.Org, checkedAt)))
	if payload.IP.FraudScore != nil {
		facts.Set(FieldUnlockIPFraudScore, IntValue(KnownAt(int64(*payload.IP.FraudScore), checkedAt)))
	}
	if len(result.Services) == 0 {
		return
	}
	unlocked := make([]string, 0, len(result.Services))
	for _, service := range result.Services {
		provider := strings.ToLower(strings.TrimSpace(service.Name))
		if provider == "" {
			continue
		}
		facts.Set(UnlockField(provider, "status"), EnumValue(knownTextAt(service.Status, checkedAt)))
		facts.Set(UnlockField(provider, "region"), StringValue(knownTextAt(service.Region, checkedAt)))
		facts.Set(UnlockField(provider, "detail"), StringValue(knownTextAt(service.Detail, checkedAt)))
		if strings.EqualFold(service.Status, unlockStatusUnlocked) {
			unlocked = append(unlocked, provider)
		}
	}
	sort.Strings(unlocked)
	facts.Set(FieldUnlockedCount, IntValue(KnownAt(int64(len(unlocked)), checkedAt)))
	facts.Set(FieldUnlockedProviders, SetValue(KnownAt(unlocked, checkedAt)))
}

// nullableBool is the shape every tri-state IP quality flag arrives in: a nil
// pointer is a provider that did not report the attribute.
func nullableBool(value *bool, checkedAt time.Time) Fact[bool] {
	if value == nil {
		return Unknown[bool]()
	}
	return KnownAt(*value, checkedAt)
}

// flagReduction accumulates one boolean attribute across providers. It records
// the oldest contributing timestamp so max_age_seconds expires the reduction as
// soon as any provider's measurement goes stale.
type flagReduction struct {
	known     bool
	anyTrue   bool
	allTrue   bool
	checkedAt time.Time
}

func (r *flagReduction) observe(fact Fact[bool]) {
	if !fact.Known {
		return
	}
	if !r.known {
		r.known = true
		r.allTrue = true
	}
	if fact.Value {
		r.anyTrue = true
	} else {
		r.allTrue = false
	}
	if !fact.CheckedAt.IsZero() && (r.checkedAt.IsZero() || fact.CheckedAt.Before(r.checkedAt)) {
		r.checkedAt = fact.CheckedAt
	}
}

func (r flagReduction) fact(value bool) Fact[bool] {
	if !r.known {
		return Unknown[bool]()
	}
	return KnownAt(value, r.checkedAt)
}

// applyIPQualityFields loads the per-provider IP quality facts plus the explicit
// cross-provider reductions. Rows whose check did not succeed contribute nothing:
// their columns hold whatever the failed attempt left behind.
func applyIPQualityFields(facts NodeFacts, rows []store.NodeIPQualityResult) {
	reductions := map[string]*flagReduction{
		"is_residential": {}, "proxy": {}, "hosting": {},
	}
	// The max reduction takes the highest score but the oldest timestamp: the
	// combined fact is only as fresh as its stalest contributor.
	maxFraudKnown, maxFraudValue := false, int64(0)
	var maxFraudCheckedAt time.Time
	for _, row := range rows {
		if row.Status != detectionStatusSuccess {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(row.Provider))
		if provider == "" {
			continue
		}
		checkedAt := row.CheckedAt
		if row.FraudScore != nil {
			score := int64(*row.FraudScore)
			facts.Set(IPQualityField(provider, "fraud_score"), IntValue(KnownAt(score, checkedAt)))
			if !maxFraudKnown || score > maxFraudValue {
				maxFraudValue = score
			}
			maxFraudKnown = true
			if !checkedAt.IsZero() && (maxFraudCheckedAt.IsZero() || checkedAt.Before(maxFraudCheckedAt)) {
				maxFraudCheckedAt = checkedAt
			}
		}
		for attribute, value := range map[string]*bool{
			"is_residential": row.IsResidential,
			"proxy":          row.Proxy,
			"hosting":        row.Hosting,
			"mobile":         row.Mobile,
			"is_broadcast":   row.IsBroadcast,
		} {
			fact := nullableBool(value, checkedAt)
			facts.Set(IPQualityField(provider, attribute), BoolValue(fact))
			if reduction, tracked := reductions[attribute]; tracked {
				reduction.observe(fact)
			}
		}
		for attribute, value := range map[string]string{
			"asn": row.ASN, "org": row.Org, "isp": row.ISP,
		} {
			facts.Set(IPQualityField(provider, attribute), StringValue(knownTextAt(value, checkedAt)))
		}
		facts.Set(IPQualityField(provider, "country_code"),
			EnumValue(knownTextAt(row.CountryCode, checkedAt)))
	}
	maxFraud := Unknown[int64]()
	if maxFraudKnown {
		maxFraud = KnownAt(maxFraudValue, maxFraudCheckedAt)
	}
	facts.Set(FieldIPQMaxFraudScore, IntValue(maxFraud))
	for attribute, reduction := range reductions {
		facts.Set(ReducedIPQualityField(ReduceAny, attribute), BoolValue(reduction.fact(reduction.anyTrue)))
		facts.Set(ReducedIPQualityField(ReduceAll, attribute), BoolValue(reduction.fact(reduction.allTrue)))
	}
}
