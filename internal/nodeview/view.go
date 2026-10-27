// Package nodeview queries one immutable management snapshot using the same
// membership predicate as groupmember. It performs no network or SQL work.
package nodeview

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"hash"
	"slices"
	"sort"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/noderegion"
	"easy_proxies/internal/nodeselect"
)

type Spec struct {
	Diagnostic           string               `json:"diagnostic,omitempty"`
	Version              int                  `json:"version"`
	Text                 string               `json:"text,omitempty"`
	Regions              []string             `json:"regions,omitempty"`
	ExcludeRegions       []string             `json:"exclude_regions,omitempty"`
	IncludeUnknownRegion bool                 `json:"include_unknown_region,omitempty"`
	TagWhitelist         []int64              `json:"tag_whitelist,omitempty"`
	TagBlacklist         []int64              `json:"tag_blacklist,omitempty"`
	TagMatch             string               `json:"tag_match,omitempty"`
	Status               []string             `json:"status,omitempty"`
	MaxLatencyMS         *int64               `json:"max_latency_ms,omitempty"`
	ExplicitIDs          []int64              `json:"explicit_ids,omitempty"`
	ExcludedIDs          []int64              `json:"excluded_ids,omitempty"`
	Sources              []string             `json:"sources,omitempty"`
	Protocols            []string             `json:"protocols,omitempty"`
	SubscriptionIDs      []int64              `json:"subscription_ids,omitempty"`
	Unsubscribed         bool                 `json:"unsubscribed,omitempty"`
	RuntimeOnly          bool                 `json:"runtime_only,omitempty"`
	HealthyOnly          bool                 `json:"healthy_only,omitempty"`
	Condition            *nodefacts.Condition `json:"condition,omitempty"`
}

func (s Spec) Validate(registry *nodefacts.Registry) error {
	if _, err := DiagnosticCondition(s.Diagnostic); err != nil {
		return err
	}
	if s.Version != 0 && s.Version != 1 && s.Version != 2 {
		return fmt.Errorf("unsupported filter version %d", s.Version)
	}
	if len(s.Text) > 512 {
		return fmt.Errorf("search text exceeds 512 bytes")
	}
	if s.MaxLatencyMS != nil && *s.MaxLatencyMS < 0 {
		return fmt.Errorf("max_latency_ms must be nonnegative")
	}
	if s.TagMatch != "" && s.TagMatch != "any" && s.TagMatch != "all" {
		return fmt.Errorf("tag_match must be any or all")
	}
	for _, list := range [][]string{s.Regions, s.ExcludeRegions} {
		if len(list) > 300 {
			return fmt.Errorf("too many region filters")
		}
		for _, value := range list {
			if s.Version == 1 {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(value), noderegion.Other) && noderegion.Normalize(value) == "" {
				return fmt.Errorf("invalid region %q", value)
			}
		}
	}
	for _, ids := range [][]int64{s.TagWhitelist, s.TagBlacklist, s.ExplicitIDs, s.ExcludedIDs, s.SubscriptionIDs} {
		if len(ids) > 10000 {
			return fmt.Errorf("too many filter IDs")
		}
		for _, id := range ids {
			if id <= 0 {
				return fmt.Errorf("filter IDs must be positive")
			}
		}
	}
	for _, status := range s.Status {
		if !slices.Contains([]string{"normal", "pending", "unavailable", "blacklisted", "disabled", "unloaded"}, status) {
			return fmt.Errorf("invalid status %q", status)
		}
	}
	if s.Condition != nil {
		return s.Condition.Validate(registry, nodefacts.DefaultLimits())
	}
	return nil
}

func (s Spec) Filter(names map[int64]string) nodeselect.Filter {
	version := s.Version
	if version == 0 {
		version = noderegion.CurrentVersion
	}
	return nodeselect.New(nodeselect.Spec{Enabled: true, Regions: s.Regions, ExcludeRegions: s.ExcludeRegions,
		RegionRuleVersion: version, IncludeUnknownRegion: s.IncludeUnknownRegion, RequireRegion: s.IncludeUnknownRegion, ExplicitIDs: s.ExplicitIDs, ExcludedIDs: s.ExcludedIDs,
		TagWhitelist: s.TagWhitelist, TagBlacklist: s.TagBlacklist, TagMatch: s.TagMatch}, names)
}

type Node struct {
	Port            uint16   `json:"port"`
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Region          string   `json:"region"`
	Country         string   `json:"country"`
	Tags            []string `json:"tags"`
	Source          string   `json:"source"`
	Protocol        string   `json:"protocol"`
	SubscriptionIDs []int64  `json:"subscription_ids"`
	Status          string   `json:"status"`
	LatencyMS       int64    `json:"latency_ms"`
	RuntimeTag      string   `json:"runtime_tag,omitempty"`
	Healthy         bool     `json:"healthy"`
	Enabled         bool     `json:"enabled"`
}

type Page struct {
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	Sort       string `json:"sort"`
	Descending bool   `json:"descending"`
}

func (p *Page) Normalize() error {
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.Sort == "" {
		p.Sort = "id"
	}
	if p.Offset < 0 || p.Limit < 1 || p.Limit > 500 {
		return fmt.Errorf("offset must be nonnegative; limit must be 1..500")
	}
	if !slices.Contains([]string{"id", "name", "region", "status", "latency_ms", "port", "source", "speed_bps", "fraud_score", "diagnostic_latency"}, p.Sort) {
		return fmt.Errorf("invalid sort %q", p.Sort)
	}
	return nil
}

type Result struct {
	Items            []Node `json:"items"`
	Total            int    `json:"total"`
	Page             Page   `json:"page"`
	SnapshotRevision string `json:"snapshot_revision"`
}

type Facet struct {
	AvgLatencyMS int64 `json:"avg_latency_ms"`
	latencySum   int64
	latencyCount int64
	Value        string `json:"value"`
	Label        string `json:"label"`
	Count        int    `json:"count"`
	Healthy      int    `json:"healthy"`
}

type Facets struct {
	Diagnostics      []Facet `json:"diagnostics"`
	Regions          []Facet `json:"regions"`
	Tags             []Facet `json:"tags"`
	Statuses         []Facet `json:"statuses"`
	Sources          []Facet `json:"sources"`
	Protocols        []Facet `json:"protocols"`
	Total            int     `json:"total"`
	SnapshotRevision string  `json:"snapshot_revision"`
}

type View struct {
	nodes    []Node
	names    map[int64]string
	facts    map[int64]nodefacts.NodeFacts
	now      time.Time
	revision string
}

func New(nodes []Node, names map[int64]string, facts []nodefacts.NodeFacts, now time.Time) *View {
	nodes = append([]Node(nil), nodes...)
	copyNames := make(map[int64]string, len(names))
	for id, name := range names {
		copyNames[id] = name
	}
	for i := range nodes {
		nodes[i] = cloneNode(nodes[i])
	}
	copyFacts := make([]nodefacts.NodeFacts, len(facts))
	for i, fact := range facts {
		copyFact := fact
		copyFact.Values = make(map[string]nodefacts.Value, len(fact.Values))
		for key, value := range fact.Values {
			value.Set = append([]string(nil), value.Set...)
			copyFact.Values[key] = value
		}
		copyFacts[i] = copyFact
	}
	return NewOwned(nodes, copyNames, copyFacts, now)
}

// NewOwned takes exclusive ownership of all inputs, including nested slices and
// fact maps. The caller must not reuse or mutate them after this call. Use New
// when inputs are shared. HTTP loaders can transfer freshly allocated data and
// avoid duplicating an entire fact snapshot merely to cross the package boundary.
func NewOwned(nodes []Node, names map[int64]string, facts []nodefacts.NodeFacts, now time.Time) *View {
	v := &View{nodes: nodes, names: names, facts: make(map[int64]nodefacts.NodeFacts, len(facts)), now: now}
	// Preserve the existing wire representation of empty nested slices. The
	// defensive constructor historically normalized them to nil while cloning.
	for i := range v.nodes {
		if len(v.nodes[i].Tags) == 0 {
			v.nodes[i].Tags = nil
		}
		if len(v.nodes[i].SubscriptionIDs) == 0 {
			v.nodes[i].SubscriptionIDs = nil
		}
	}
	for i := range facts {
		if facts[i].Values == nil {
			facts[i].Values = map[string]nodefacts.Value{}
		}
		for key, value := range facts[i].Values {
			if value.Set != nil && len(value.Set) == 0 {
				value.Set = nil
				facts[i].Values[key] = value
			}
		}
		v.facts[facts[i].NodeID] = facts[i]
	}
	slices.SortFunc(v.nodes, func(a, b Node) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortStableFunc(facts, func(a, b nodefacts.NodeFacts) int { return cmp.Compare(a.NodeID, b.NodeID) })
	// Revision is opaque. Hash canonical records separately so JSON growth and
	// escaping never require a second full-snapshot buffer. Newlines are safe
	// record separators because JSON strings escape embedded newlines.
	h := sha256.New()
	_, _ = h.Write([]byte("nodeview:2\nnodes\n"))
	for start := 0; start < len(v.nodes); start += 256 {
		hashRevisionRecord(h, v.nodes[start:min(start+256, len(v.nodes))])
	}
	_, _ = h.Write([]byte("names\n"))
	if names == nil {
		names = map[int64]string{}
	}
	hashRevisionRecord(h, names)
	_, _ = h.Write([]byte("facts\n"))
	for start := 0; start < len(facts); start += 32 {
		hashRevisionRecord(h, facts[start:min(start+32, len(facts))])
	}
	v.revision = fmt.Sprintf("%x", h.Sum(nil))
	return v
}

func hashRevisionRecord(h hash.Hash, value any) {
	encoded, _ := json.MarshalCanonical(value)
	_, _ = h.Write(encoded)
	_, _ = h.Write([]byte{'\n'})
}

func cloneNode(node Node) Node {
	node.Tags = slices.Clone(node.Tags)
	node.SubscriptionIDs = slices.Clone(node.SubscriptionIDs)
	return node
}

func (v *View) Revision() string { return v.revision }

type Candidate struct {
	Node
	Decision nodeselect.Decision `json:"decision"`
}

func (v *View) Candidates(spec nodeselect.Spec) []Candidate {
	filter := nodeselect.New(spec, v.names)
	result := make([]Candidate, 0, len(v.nodes))
	for _, node := range v.nodes {
		decision := filter.Explain(nodeselect.Node{ID: node.ID, Region: node.Region, Tags: node.Tags})
		if !node.Enabled {
			decision = nodeselect.Decision{Reason: "node_disabled"}
		}
		result = append(result, Candidate{Node: cloneNode(node), Decision: decision})
	}
	return result
}

func (v *View) Query(spec Spec, page Page) (Result, error) {
	if err := page.Normalize(); err != nil {
		return Result{}, err
	}
	matcher, err := v.matcher(spec)
	if err != nil {
		return Result{}, err
	}
	result := Result{Items: []Node{}, Page: page, SnapshotRevision: v.revision}
	if page.Sort == "id" {
		// The immutable snapshot is already in ID order. Count all matches but
		// materialize only this page, including for reverse traversal.
		for i := range v.nodes {
			index := i
			if page.Descending {
				index = len(v.nodes) - 1 - i
			}
			node := v.nodes[index]
			allowed, err := matcher.allow(node)
			if err != nil {
				return Result{}, err
			}
			if !allowed {
				continue
			}
			if result.Total >= page.Offset && len(result.Items) < page.Limit {
				result.Items = append(result.Items, cloneNode(node))
			}
			result.Total++
		}
		return result, nil
	}
	matched := make([]int, 0)
	var nameKeys []string
	if page.Sort == "name" {
		nameKeys = make([]string, len(v.nodes))
	}
	for index, node := range v.nodes {
		allowed, err := matcher.allow(node)
		if err != nil {
			return Result{}, err
		}
		if allowed {
			matched = append(matched, index)
			if nameKeys != nil {
				nameKeys[index] = strings.ToLower(node.Name)
			}
		}
	}
	field := ""
	switch page.Sort {
	case "speed_bps":
		field = nodefacts.FieldSpeedBps
	case "fraud_score":
		field = nodefacts.FieldIPQMaxFraudScore
	case "diagnostic_latency":
		field = nodefacts.FieldLatencyMs
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := v.nodes[matched[i]], v.nodes[matched[j]]
		cmp := 0
		switch page.Sort {
		case "name":
			cmp = strings.Compare(nameKeys[matched[i]], nameKeys[matched[j]])
		case "speed_bps", "fraud_score", "diagnostic_latency":
			av, bv := v.facts[a.ID].Value(field), v.facts[b.ID].Value(field)
			if av.Known != bv.Known {
				return av.Known
			}
			if av.Num < bv.Num {
				cmp = -1
			} else if av.Num > bv.Num {
				cmp = 1
			}
		case "source":
			cmp = strings.Compare(a.Source, b.Source)
		case "port":
			cmp = comparePorts(a.Port, b.Port)
		case "region":
			cmp = strings.Compare(a.Region, b.Region)
		case "status":
			cmp = strings.Compare(a.Status, b.Status)
		case "latency_ms":
			if a.LatencyMS < b.LatencyMS {
				cmp = -1
			} else if a.LatencyMS > b.LatencyMS {
				cmp = 1
			}
		}
		if cmp == 0 {
			return a.ID < b.ID
		}
		if page.Descending {
			return cmp > 0
		}
		return cmp < 0
	})
	result.Total = len(matched)
	start := min(page.Offset, len(matched))
	end := start + min(page.Limit, len(matched)-start)
	for _, index := range matched[start:end] {
		result.Items = append(result.Items, cloneNode(v.nodes[index]))
	}
	return result, nil
}

// Facets counts the entire visible snapshot, not just the current page.
func (v *View) Facets() Facets        { return v.facets(Spec{}) }
func (v *View) RuntimeFacets() Facets { return v.facets(Spec{RuntimeOnly: true}) }
func (v *View) HealthyFacets() Facets { return v.facets(Spec{HealthyOnly: true}) }
func (v *View) facets(scope Spec) Facets {
	total := 0
	regions, tags, statuses, sources, protocols := map[string]*Facet{}, map[string]*Facet{}, map[string]*Facet{}, map[string]*Facet{}, map[string]*Facet{}
	add := func(target map[string]*Facet, value, label string, healthy bool) {
		item := target[value]
		if item == nil {
			item = &Facet{Value: value, Label: label}
			target[value] = item
		}
		item.Count++
		if healthy {
			item.Healthy++
		}
	}
	tagIDs := map[string]int64{}
	for id, name := range v.names {
		tagIDs[name] = id
	}
	for _, node := range v.nodes {
		if !scope.inScope(node) {
			continue
		}
		total++
		region := noderegion.Normalize(node.Region)
		key, label := region, noderegion.Name(region)
		if region == "" {
			key, label = "unknown", "未检测"
		}
		add(regions, key, label, node.Healthy)
		if node.LatencyMS > 0 {
			regions[key].latencySum += node.LatencyMS
			regions[key].latencyCount++
		}
		for _, tag := range node.Tags {
			if id := tagIDs[tag]; id > 0 {
				add(tags, fmt.Sprint(id), tag, node.Healthy)
			}
		}
		add(statuses, node.Status, node.Status, node.Healthy)
		add(sources, node.Source, node.Source, node.Healthy)
		add(protocols, node.Protocol, node.Protocol, node.Healthy)
	}
	list := func(m map[string]*Facet) []Facet {
		result := make([]Facet, 0, len(m))
		for _, item := range m {
			if item.latencyCount > 0 {
				item.AvgLatencyMS = (item.latencySum + item.latencyCount/2) / item.latencyCount
			}
			result = append(result, *item)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Value < result[j].Value })
		return result
	}
	diagnostics := []Facet{}
	for _, name := range []string{"untested", "failed", "unlocked", "mixed", "locked", "residential", "broadcast", "proxy", "quality_success", "quality_failed", "speed_fast", "speed_failed", "fraud_high"} {
		condition, _ := DiagnosticCondition(name)
		labels := map[string]string{"untested": "未检测", "failed": "解锁失败", "unlocked": "完整解锁", "mixed": "混合解锁", "locked": "受限 / 部分", "residential": "住宅 IP", "broadcast": "广播 IP", "proxy": "代理 / 托管", "quality_success": "质量已测", "quality_failed": "质量失败", "speed_fast": "速度 ≥5 MB/s", "speed_failed": "测速失败", "fraud_high": "风险分 ≥71"}
		item := Facet{Value: name, Label: labels[name]}
		for _, node := range v.nodes {
			if !scope.inScope(node) {
				continue
			}
			if allowed, _ := nodefacts.Evaluate(*condition, v.facts[node.ID], v.now); allowed {
				item.Count++
			}
		}
		diagnostics = append(diagnostics, item)
	}
	return Facets{Diagnostics: diagnostics, Regions: list(regions), Tags: list(tags), Statuses: list(statuses), Sources: list(sources), Protocols: list(protocols), Total: total, SnapshotRevision: v.revision}
}

// inScope is shared by queries and all facet counters, including diagnostics.
func (s Spec) inScope(node Node) bool {
	if (s.RuntimeOnly || s.HealthyOnly) && node.RuntimeTag == "" {
		return false
	}
	return !s.HealthyOnly || node.Healthy
}

// Allow applies the same constraints as Query to one node from this snapshot.
func (v *View) Allow(spec Spec, node Node) (bool, error) {
	matcher, err := v.matcher(spec)
	if err != nil {
		return false, err
	}
	return matcher.allow(node)
}

type viewMatcher struct {
	view   *View
	spec   Spec
	filter nodeselect.Filter
	text   string
	preset *nodefacts.Condition
}

func (v *View) matcher(spec Spec) (viewMatcher, error) {
	preset, err := DiagnosticCondition(spec.Diagnostic)
	return viewMatcher{view: v, spec: spec, filter: spec.Filter(v.names), text: strings.ToLower(strings.TrimSpace(spec.Text)), preset: preset}, err
}

func (m viewMatcher) allow(node Node) (bool, error) {
	v, spec, filter := m.view, m.spec, m.filter
	if !filter.Allow(nodeselect.Node{ID: node.ID, Region: node.Region, Tags: node.Tags}) {
		return false, nil
	}
	if spec.Text != "" && !strings.Contains(strings.ToLower(node.Name), m.text) {
		return false, nil
	}
	if !spec.inScope(node) {
		return false, nil
	}
	if len(spec.Status) > 0 && !slices.Contains(spec.Status, node.Status) {
		return false, nil
	}
	if len(spec.Sources) > 0 && !slices.Contains(spec.Sources, node.Source) {
		return false, nil
	}
	if len(spec.Protocols) > 0 && !slices.Contains(spec.Protocols, node.Protocol) {
		return false, nil
	}
	if spec.MaxLatencyMS != nil && (node.LatencyMS < 0 || node.LatencyMS > *spec.MaxLatencyMS) {
		return false, nil
	}
	if spec.Unsubscribed && len(node.SubscriptionIDs) > 0 {
		return false, nil
	}
	if len(spec.SubscriptionIDs) > 0 {
		found := false
		for _, id := range spec.SubscriptionIDs {
			if slices.Contains(node.SubscriptionIDs, id) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	if m.preset != nil {
		ok, err := nodefacts.Evaluate(*m.preset, v.facts[node.ID], v.now)
		if err != nil || !ok {
			return false, err
		}
	}
	if spec.Condition != nil {
		ok, err := nodefacts.Evaluate(*spec.Condition, v.facts[node.ID], v.now)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func comparePorts(a, b uint16) int { return cmp.Compare(a, b) }

// IDs selects bulk-operation targets from this single snapshot without paging
// over a changing runtime or sorting/materializing full node responses.
func (v *View) IDs(spec Spec) ([]int64, error) {
	ids := []int64{}
	matcher, err := v.matcher(spec)
	if err != nil {
		return nil, err
	}
	for _, node := range v.nodes {
		allowed, err := matcher.allow(node)
		if err != nil {
			return nil, err
		}
		if allowed {
			ids = append(ids, node.ID)
		}
	}
	return ids, nil
}
