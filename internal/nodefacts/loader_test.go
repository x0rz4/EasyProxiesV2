package nodefacts

import (
	"context"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

var loadNow = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// countingSource records how many times each batch query ran, so a test can prove
// the loader never degrades into one query per node.
type countingSource struct {
	nodes         []store.Node
	stats         map[int64]*store.NodeStats
	detections    map[int64]*store.NodeDetectionResult
	qualities     map[int64][]store.NodeIPQualityResult
	unlocks       map[int64]*store.UnlockResult
	subscriptions map[int64][]int64
	nodeTags      []store.NodeTag
	tags          []store.Tag
	subs          []store.Subscription
	calls         map[string]int
}

func newCountingSource(nodes ...store.Node) *countingSource {
	return &countingSource{
		nodes:         nodes,
		stats:         map[int64]*store.NodeStats{},
		detections:    map[int64]*store.NodeDetectionResult{},
		qualities:     map[int64][]store.NodeIPQualityResult{},
		unlocks:       map[int64]*store.UnlockResult{},
		subscriptions: map[int64][]int64{},
		calls:         map[string]int{},
	}
}

func (s *countingSource) record(name string) { s.calls[name]++ }

func (s *countingSource) total() int {
	sum := 0
	for _, count := range s.calls {
		sum += count
	}
	return sum
}

func (s *countingSource) ListNodes(_ context.Context, filter store.NodeFilter) ([]store.Node, error) {
	s.record("ListNodes")
	if len(filter.NodeIDs) == 0 {
		return append([]store.Node(nil), s.nodes...), nil
	}
	wanted := map[int64]struct{}{}
	for _, id := range filter.NodeIDs {
		wanted[id] = struct{}{}
	}
	out := make([]store.Node, 0, len(filter.NodeIDs))
	for _, node := range s.nodes {
		if _, ok := wanted[node.ID]; ok {
			out = append(out, node)
		}
	}
	return out, nil
}

func (s *countingSource) ListNodeStats(_ context.Context, _ []int64) (map[int64]*store.NodeStats, error) {
	s.record("ListNodeStats")
	return s.stats, nil
}

func (s *countingSource) ListNodeDetectionResultsByIDs(_ context.Context, _ []int64) (map[int64]*store.NodeDetectionResult, error) {
	s.record("ListNodeDetectionResultsByIDs")
	return s.detections, nil
}

func (s *countingSource) ListNodeIPQualityResultsByIDs(_ context.Context, _ []int64) (map[int64][]store.NodeIPQualityResult, error) {
	s.record("ListNodeIPQualityResultsByIDs")
	return s.qualities, nil
}

func (s *countingSource) ListUnlockResultsByIDs(_ context.Context, _ []int64) (map[int64]*store.UnlockResult, error) {
	s.record("ListUnlockResultsByIDs")
	return s.unlocks, nil
}

func (s *countingSource) ListNodeSubscriptionIDs(_ context.Context, _ []int64) (map[int64][]int64, error) {
	s.record("ListNodeSubscriptionIDs")
	return s.subscriptions, nil
}

func (s *countingSource) ListNodeTags(_ context.Context, filter store.NodeTagFilter) ([]store.NodeTag, error) {
	s.record("ListNodeTags")
	if filter.Source == "" {
		return append([]store.NodeTag(nil), s.nodeTags...), nil
	}
	out := make([]store.NodeTag, 0, len(s.nodeTags))
	for _, assignment := range s.nodeTags {
		if assignment.Source == filter.Source {
			out = append(out, assignment)
		}
	}
	return out, nil
}

func (s *countingSource) ListTags(_ context.Context) ([]store.Tag, error) {
	s.record("ListTags")
	return append([]store.Tag(nil), s.tags...), nil
}

func (s *countingSource) ListSubscriptions(_ context.Context) ([]store.Subscription, error) {
	s.record("ListSubscriptions")
	return append([]store.Subscription(nil), s.subs...), nil
}

func mustLoad(t *testing.T, source *countingSource, nodeIDs []int64) []NodeFacts {
	t.Helper()
	loaded, err := NewLoader(source).Load(context.Background(), nodeIDs)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded
}

func mustLoadOne(t *testing.T, source *countingSource) NodeFacts {
	t.Helper()
	loaded := mustLoad(t, source, nil)
	if len(loaded) != 1 {
		t.Fatalf("Load returned %d fact sets, want 1", len(loaded))
	}
	return loaded[0]
}

func assertUnknown(t *testing.T, facts NodeFacts, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if facts.Value(field).Known {
			t.Fatalf("field %q must be unknown, got %s", field, facts.Value(field).Display())
		}
	}
}

func assertInt(t *testing.T, facts NodeFacts, field string, want int64) {
	t.Helper()
	value := facts.Value(field)
	if !value.Known {
		t.Fatalf("field %q is unknown, want %d", field, want)
	}
	if value.Num != want {
		t.Fatalf("field %q = %d, want %d", field, value.Num, want)
	}
}

func assertBool(t *testing.T, facts NodeFacts, field string, want bool) {
	t.Helper()
	value := facts.Value(field)
	if !value.Known {
		t.Fatalf("field %q is unknown, want %v", field, want)
	}
	if value.Bool != want {
		t.Fatalf("field %q = %v, want %v", field, value.Bool, want)
	}
}

func assertText(t *testing.T, facts NodeFacts, field, want string) {
	t.Helper()
	value := facts.Value(field)
	if !value.Known {
		t.Fatalf("field %q is unknown, want %q", field, want)
	}
	if value.Str != want {
		t.Fatalf("field %q = %q, want %q", field, value.Str, want)
	}
}

func assertSet(t *testing.T, facts NodeFacts, field string, want ...string) {
	t.Helper()
	value := facts.Value(field)
	if !value.Known {
		t.Fatalf("field %q is unknown, want %v", field, want)
	}
	if len(value.Set) != len(want) {
		t.Fatalf("field %q = %v, want %v", field, value.Set, want)
	}
	for index := range want {
		if value.Set[index] != want[index] {
			t.Fatalf("field %q = %v, want %v", field, value.Set, want)
		}
	}
}

// TestLoadQueryCountIsIndependentOfNodeCount is the N+1 guard: a recompute over
// 1000 nodes must cost the same number of round trips as one over a single node.
func TestLoadQueryCountIsIndependentOfNodeCount(t *testing.T) {
	totals := map[int]int{}
	for _, size := range []int{1, 10, 1000} {
		source := newCountingSource()
		source.tags = []store.Tag{{ID: 7, Name: "game"}}
		source.subs = []store.Subscription{{ID: 3, Name: "airport"}}
		for index := 1; index <= size; index++ {
			id := int64(index)
			source.nodes = append(source.nodes, store.Node{ID: id, Name: "node", Region: "hk", Enabled: true})
			source.stats[id] = &store.NodeStats{NodeID: id, InitialCheckDone: true, LastLatencyMs: 20}
			source.nodeTags = append(source.nodeTags,
				store.NodeTag{NodeID: id, TagID: 7, Source: store.NodeTagSourceManual})
			source.subscriptions[id] = []int64{3}
		}
		loaded := mustLoad(t, source, nil)
		if len(loaded) != size {
			t.Fatalf("Load returned %d fact sets, want %d", len(loaded), size)
		}
		for method, count := range source.calls {
			if count != 1 {
				t.Fatalf("%d nodes: %s ran %d times, want exactly one batch", size, method, count)
			}
		}
		totals[size] = source.total()
	}
	for size, total := range totals {
		if total != totals[1] {
			t.Fatalf("%d nodes cost %d queries, one node cost %d", size, total, totals[1])
		}
	}
}

// TestLoadUnmeasuredNodeReadsUnknownNotZero is the loader half of the unknown
// semantics: a node that was never checked must not look like a node measured at 0.
func TestLoadUnmeasuredNodeReadsUnknownNotZero(t *testing.T) {
	source := newCountingSource(store.Node{
		ID: 5, Name: "JP-01", Region: "JP", Enabled: true, Port: 443,
		URI: "vless://host:443?x=1",
	})
	facts := mustLoadOne(t, source)

	assertUnknown(t, facts,
		FieldLatencyMs, FieldSpeedBps, FieldSpeedPeakBps,
		FieldAvailable, FieldBlacklisted, FieldFailureCount, FieldSuccessCount,
		FieldExitCountryCode, FieldExitIPFamily,
		FieldUnlockIPPure, FieldUnlockIPRiskLevel, FieldUnlockIPFraudScore,
		FieldUnlockedCount, FieldUnlockedProviders,
		FieldIPQMaxFraudScore,
		ReducedIPQualityField(ReduceAny, "proxy"), ReducedIPQualityField(ReduceAll, "proxy"),
		FieldNodeCountry,
	)
	// Configuration facts are always known, and they carry no timestamp so no rule
	// can age them out.
	assertText(t, facts, FieldNodeName, "JP-01")
	assertText(t, facts, FieldNodeRegion, "jp")
	assertText(t, facts, FieldNodeProtocol, "vless")
	assertInt(t, facts, FieldNodePort, 443)
	assertBool(t, facts, FieldNodeEnabled, true)
	if checkedAt := facts.Value(FieldNodeName).CheckedAt; !checkedAt.IsZero() {
		t.Fatalf("configuration facts must be timeless, got %v", checkedAt)
	}
	// Membership is known-empty rather than unknown: "belongs to nothing" is an answer.
	assertSet(t, facts, FieldSubscriptionIDs)
	assertSet(t, facts, FieldSubscriptionNames)
	assertSet(t, facts, FieldManualTags)
	if facts.NodeID != 5 || facts.Name != "JP-01" || facts.Region != "jp" {
		t.Fatalf("fact set header = %d/%q/%q", facts.NodeID, facts.Name, facts.Region)
	}
}

func TestLoadEmptyAndSubsetSelections(t *testing.T) {
	source := newCountingSource(
		store.Node{ID: 3, Name: "c"}, store.Node{ID: 1, Name: "a"}, store.Node{ID: 2, Name: "b"},
	)
	loaded := mustLoad(t, source, nil)
	if len(loaded) != 3 || loaded[0].NodeID != 1 || loaded[1].NodeID != 2 || loaded[2].NodeID != 3 {
		t.Fatalf("Load must sort by node ID, got %+v", loaded)
	}
	// An empty region normalizes to the bucket the group builder uses, so a rule
	// can address unclassified nodes.
	assertText(t, loaded[0], FieldNodeRegion, "other")

	subset := mustLoad(t, source, []int64{2})
	if len(subset) != 1 || subset[0].NodeID != 2 {
		t.Fatalf("Load(nodeIDs) returned %+v", subset)
	}
	if empty := mustLoad(t, newCountingSource(), nil); empty != nil {
		t.Fatalf("Load over no nodes must return nil, got %+v", empty)
	}
}

func TestLoadLatencyPrefersDetectionAndHonoursSentinel(t *testing.T) {
	latency := int64(37)
	statsAt := loadNow.Add(-10 * time.Minute)
	detectionAt := loadNow.Add(-time.Minute)

	// The dedicated check wins because it carries its own timestamp.
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.stats[1] = &store.NodeStats{NodeID: 1, LastLatencyMs: 500, UpdatedAt: statsAt, InitialCheckDone: true}
	source.detections[1] = &store.NodeDetectionResult{
		NodeID: 1, LatencyStatus: "success", LatencyMs: &latency, LatencyCheckedAt: detectionAt,
	}
	facts := mustLoadOne(t, source)
	assertInt(t, facts, FieldLatencyMs, 37)
	if got := facts.Value(FieldLatencyMs).CheckedAt; !got.Equal(detectionAt) {
		t.Fatalf("latency timestamp = %v, want the detection check time %v", got, detectionAt)
	}

	// Without a successful dedicated check the health probe's last sample is used.
	source = newCountingSource(store.Node{ID: 1, Name: "n"})
	source.stats[1] = &store.NodeStats{NodeID: 1, LastLatencyMs: 500, UpdatedAt: statsAt}
	source.detections[1] = &store.NodeDetectionResult{NodeID: 1, LatencyStatus: "failed"}
	facts = mustLoadOne(t, source)
	assertInt(t, facts, FieldLatencyMs, 500)
	if got := facts.Value(FieldLatencyMs).CheckedAt; !got.Equal(statsAt) {
		t.Fatalf("fallback latency timestamp = %v, want %v", got, statsAt)
	}

	// -1 is the never-tested sentinel, not a measurement.
	source = newCountingSource(store.Node{ID: 1, Name: "n"})
	source.stats[1] = &store.NodeStats{NodeID: 1, LastLatencyMs: -1, UpdatedAt: statsAt}
	assertUnknown(t, mustLoadOne(t, source), FieldLatencyMs)
}

func TestLoadAvailableIsUnknownUntilTheFirstCheck(t *testing.T) {
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.stats[1] = &store.NodeStats{NodeID: 1, Available: false, InitialCheckDone: false,
		Blacklisted: true, FailureCount: 2, SuccessCount: 9}
	facts := mustLoadOne(t, source)
	assertUnknown(t, facts, FieldAvailable)
	// The counters and the blacklist flag are always known once a stats row exists.
	assertBool(t, facts, FieldBlacklisted, true)
	assertInt(t, facts, FieldFailureCount, 2)
	assertInt(t, facts, FieldSuccessCount, 9)

	source.stats[1].InitialCheckDone = true
	source.stats[1].UpdatedAt = loadNow.Add(-time.Minute)
	assertBool(t, mustLoadOne(t, source), FieldAvailable, false)
}

func TestLoadSkipsUnsuccessfulDetectionStages(t *testing.T) {
	speed, peak := int64(1024), int64(4096)
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.detections[1] = &store.NodeDetectionResult{
		NodeID: 1,
		// A failed stage leaves stale columns behind, so it must contribute nothing.
		SpeedStatus: "failed", AverageBytesPerSecond: &speed, PeakBytesPerSecond: &peak,
		ExitIPStatus: "failed", ExitCountryCode: "JP", ExitIPFamily: "ipv4",
	}
	assertUnknown(t, mustLoadOne(t, source),
		FieldSpeedBps, FieldSpeedPeakBps, FieldExitCountryCode, FieldExitIPFamily)

	source.detections[1].SpeedStatus = "success"
	source.detections[1].SpeedCheckedAt = loadNow.Add(-time.Hour)
	source.detections[1].ExitIPStatus = "success"
	facts := mustLoadOne(t, source)
	assertInt(t, facts, FieldSpeedBps, 1024)
	assertInt(t, facts, FieldSpeedPeakBps, 4096)
	assertText(t, facts, FieldExitCountryCode, "JP")
	assertText(t, facts, FieldExitIPFamily, "ipv4")
}

func TestLoadUnlockFacts(t *testing.T) {
	checkedAt := loadNow.Add(-30 * time.Minute)
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.unlocks[1] = &store.UnlockResult{
		NodeID: 1, CheckedAt: checkedAt,
		IP: store.UnlockIPInfo{IP: "1.2.3.4", Pure: true},
		Services: []store.UnlockServiceResult{
			{Name: "Netflix", Status: "unlocked", Region: "HK", Detail: "原生解锁"},
			{Name: "youtube", Status: "failed", Detail: "timeout"},
			{Name: "  ", Status: "unlocked"},
		},
		ResultJSON: `{"ip":{"ip":"1.2.3.4","asn":"AS4515","org":"HKT","ip_type":"住宅",` +
			`"usage_type":"ISP","risk_level":"Low","fraud_score":12}}`,
	}
	facts := mustLoadOne(t, source)

	assertBool(t, facts, FieldUnlockIPPure, true)
	assertText(t, facts, FieldUnlockIPRiskLevel, "Low")
	assertText(t, facts, FieldUnlockIPType, "住宅")
	assertText(t, facts, FieldUnlockIPUsageType, "ISP")
	assertText(t, facts, FieldUnlockIPASN, "AS4515")
	assertText(t, facts, FieldUnlockIPOrg, "HKT")
	assertInt(t, facts, FieldUnlockIPFraudScore, 12)
	if got := facts.Value(FieldUnlockIPRiskLevel).CheckedAt; !got.Equal(checkedAt) {
		t.Fatalf("unlock fact timestamp = %v, want %v", got, checkedAt)
	}
	// Provider names are lowercased so a rule is written once, and "failed" is a
	// measured answer: it is neither unlocked nor unknown.
	assertText(t, facts, UnlockField("netflix", "status"), "unlocked")
	assertText(t, facts, UnlockField("netflix", "region"), "HK")
	assertText(t, facts, UnlockField("netflix", "detail"), "原生解锁")
	assertText(t, facts, UnlockField("youtube", "status"), "failed")
	assertUnknown(t, facts, UnlockField("youtube", "region"), UnlockField("hbo", "status"))
	assertInt(t, facts, FieldUnlockedCount, 1)
	assertSet(t, facts, FieldUnlockedProviders, "netflix")
}

func TestLoadUnlockToleratesMissingAndCorruptPayloads(t *testing.T) {
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.unlocks[1] = &store.UnlockResult{
		NodeID: 1, CheckedAt: loadNow,
		// No IP means the native-IP verdict was never established; a corrupt payload
		// must leave the extended facts unknown instead of failing the recompute.
		ResultJSON: `{"ip":{"risk_level":`,
		Services:   []store.UnlockServiceResult{{Name: "netflix", Status: "unlocked"}},
	}
	facts := mustLoadOne(t, source)
	assertUnknown(t, facts, FieldUnlockIPPure, FieldUnlockIPRiskLevel, FieldUnlockIPFraudScore)
	assertText(t, facts, UnlockField("netflix", "status"), "unlocked")
	assertInt(t, facts, FieldUnlockedCount, 1)
}

func boolPtr(value bool) *bool { return &value }
func intPtr(value int) *int    { return &value }

// TestLoadIPQualityReductions pins the cross-provider semantics: the reduction is
// only as fresh as its stalest contributor, and a provider that failed contributes
// nothing at all.
func TestLoadIPQualityReductions(t *testing.T) {
	older, newer := loadNow.Add(-2*time.Hour), loadNow.Add(-time.Minute)
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.qualities[1] = []store.NodeIPQualityResult{
		{
			NodeID: 1, Provider: "IPPure", Status: "success", CheckedAt: older,
			FraudScore: intPtr(10), Proxy: boolPtr(false), IsResidential: boolPtr(true),
			ASN: "AS4515", Org: "HKT", ISP: "HKT", CountryCode: "HK",
		},
		{
			NodeID: 1, Provider: "ip-api", Status: "success", CheckedAt: newer,
			FraudScore: intPtr(80), Proxy: boolPtr(true), IsResidential: boolPtr(true),
			Hosting: boolPtr(false), Mobile: boolPtr(true),
		},
		// A failed row's columns hold whatever the failed attempt left behind.
		{NodeID: 1, Provider: "broken", Status: "failed", CheckedAt: newer, FraudScore: intPtr(99)},
	}
	facts := mustLoadOne(t, source)

	assertInt(t, facts, IPQualityField("ippure", "fraud_score"), 10)
	assertText(t, facts, IPQualityField("ippure", "asn"), "AS4515")
	assertText(t, facts, IPQualityField("ippure", "country_code"), "HK")
	assertUnknown(t, facts,
		IPQualityField("ippure", "hosting"), IPQualityField("ippure", "mobile"),
		IPQualityField("ip-api", "asn"),
		IPQualityField("broken", "fraud_score"), IPQualityField("bogus", "proxy"))

	assertInt(t, facts, FieldIPQMaxFraudScore, 80)
	if got := facts.Value(FieldIPQMaxFraudScore).CheckedAt; !got.Equal(older) {
		t.Fatalf("max fraud score timestamp = %v, want the oldest contributor %v", got, older)
	}
	assertBool(t, facts, ReducedIPQualityField(ReduceAny, "proxy"), true)
	assertBool(t, facts, ReducedIPQualityField(ReduceAll, "proxy"), false)
	assertBool(t, facts, ReducedIPQualityField(ReduceAny, "is_residential"), true)
	assertBool(t, facts, ReducedIPQualityField(ReduceAll, "is_residential"), true)
	// Only one provider reported hosting, and it said no.
	assertBool(t, facts, ReducedIPQualityField(ReduceAny, "hosting"), false)
	assertBool(t, facts, ReducedIPQualityField(ReduceAll, "hosting"), false)
	if got := facts.Value(ReducedIPQualityField(ReduceAny, "proxy")).CheckedAt; !got.Equal(older) {
		t.Fatalf("reduction timestamp = %v, want the oldest contributor %v", got, older)
	}
}

func TestLoadIPQualitySingleProviderDegradesToItsOwnValue(t *testing.T) {
	source := newCountingSource(store.Node{ID: 1, Name: "n"})
	source.qualities[1] = []store.NodeIPQualityResult{{
		NodeID: 1, Provider: "ippure", Status: "success", CheckedAt: loadNow,
		FraudScore: intPtr(42), Proxy: boolPtr(true),
	}}
	facts := mustLoadOne(t, source)
	assertInt(t, facts, FieldIPQMaxFraudScore, 42)
	assertBool(t, facts, ReducedIPQualityField(ReduceAny, "proxy"), true)
	assertBool(t, facts, ReducedIPQualityField(ReduceAll, "proxy"), true)
	// Nothing reported these, so the reductions stay unmeasured rather than false.
	assertUnknown(t, facts,
		ReducedIPQualityField(ReduceAny, "hosting"), ReducedIPQualityField(ReduceAll, "hosting"))
}

func TestLoadManualTagsAndSubscriptions(t *testing.T) {
	source := newCountingSource(store.Node{ID: 1, Name: "n"}, store.Node{ID: 2, Name: "m"})
	source.tags = []store.Tag{{ID: 7, Name: "vip"}, {ID: 8, Name: "game"}}
	source.nodeTags = []store.NodeTag{
		{NodeID: 1, TagID: 8, Source: store.NodeTagSourceManual},
		{NodeID: 1, TagID: 7, Source: store.NodeTagSourceManual},
		// Auto rows must never reach tags.manual, otherwise a rule could read the
		// engine's own output and recomputes would stop being idempotent.
		{NodeID: 1, TagID: 99, Source: store.NodeTagSourceAuto},
	}
	source.subs = []store.Subscription{{ID: 3, Name: "airport"}, {ID: 4, Name: ""}}
	source.subscriptions[1] = []int64{4, 3}
	loaded := mustLoad(t, source, nil)

	assertSet(t, loaded[0], FieldManualTags, "game", "vip")
	assertSet(t, loaded[0], FieldSubscriptionIDs, "3", "4")
	// A subscription with no name contributes no name, only an ID.
	assertSet(t, loaded[0], FieldSubscriptionNames, "airport")
	assertSet(t, loaded[1], FieldManualTags)
	assertSet(t, loaded[1], FieldSubscriptionIDs)
}

func TestLoadedFactsSatisfyRules(t *testing.T) {
	source := newCountingSource(store.Node{ID: 1, Name: "HK-01", Region: "hk", Enabled: true})
	source.stats[1] = &store.NodeStats{NodeID: 1, LastLatencyMs: 88,
		InitialCheckDone: true, Available: true, UpdatedAt: loadNow.Add(-time.Minute)}
	source.unlocks[1] = &store.UnlockResult{NodeID: 1, CheckedAt: loadNow.Add(-time.Minute),
		IP:       store.UnlockIPInfo{IP: "1.2.3.4", Pure: true},
		Services: []store.UnlockServiceResult{{Name: "netflix", Status: "unlocked"}}}
	facts := mustLoadOne(t, source)

	rule := Condition{Match: MatchAll, Children: []Condition{
		{FieldName: FieldLatencyMs, Op: OpLte, Value: 100, MaxAgeSeconds: 3600},
		{FieldName: FieldUnlockIPPure, Op: OpEq, Value: true},
		{FieldName: UnlockField("netflix", "status"), Op: OpEq, Value: "unlocked"},
		{FieldName: FieldNodeRegion, Op: OpIn, Values: []any{"hk", "tw"}},
		// The node has no speed sample, so "not slow" must not hold.
		{FieldName: FieldSpeedBps, Op: OpIsUnknown},
	}}
	matched, err := Evaluate(rule, facts, loadNow)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched {
		t.Fatalf("a loaded fact set failed a rule it satisfies: %+v", facts.Values)
	}
	stale := Condition{FieldName: FieldLatencyMs, Op: OpLte, Value: 100, MaxAgeSeconds: 10}
	if matched, err = Evaluate(stale, facts, loadNow.Add(time.Hour)); err != nil || matched {
		t.Fatalf("a stale loaded measurement still matched (err=%v)", err)
	}
}
