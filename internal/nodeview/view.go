// Package nodeview queries one immutable management snapshot using the same
// membership predicate as groupmember. It performs no network or SQL work.
package nodeview

import (
	"cmp"
	"crypto/sha256"
	"fmt"
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
	v := &View{nodes: append([]Node(nil), nodes...), names: make(map[int64]string, len(names)), facts: map[int64]nodefacts.NodeFacts{}, now: now}
	for id, name := range names {
		v.names[id] = name
	}
	for i := range v.nodes {
		v.nodes[i].Tags = append([]string(nil), v.nodes[i].Tags...)
		v.nodes[i].SubscriptionIDs = append([]int64(nil), v.nodes[i].SubscriptionIDs...)
	}
	for _, fact := range facts {
		copyFact := fact
		copyFact.Values = make(map[string]nodefacts.Value, len(fact.Values))
		for key, value := range fact.Values {
			value.Set = append([]string(nil), value.Set...)
			copyFact.Values[key] = value
		}
		v.facts[fact.NodeID] = copyFact
	}
	sort.Slice(v.nodes, func(i, j int) bool { return v.nodes[i].ID < v.nodes[j].ID })
	encoded, _ := json.MarshalCanonical(struct {
		Nodes []Node
		Names map[int64]string
		Facts []nodefacts.NodeFacts
	}{v.nodes, v.names, facts})
	v.revision = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return v
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
		result = append(result, Candidate{Node: node, Decision: decision})
	}
	return result
}

func (v *View) Query(spec Spec, page Page) (Result, error) {
	if err := page.Normalize(); err != nil {
		return Result{}, err
	}
	filter := spec.Filter(v.names)
	matched := make([]Node, 0)
	for _, node := range v.nodes {
		allowed, err := v.allow(spec, filter, node)
		if err != nil {
			return Result{}, err
		}
		if allowed {
			matched = append(matched, node)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		cmp := 0
		switch page.Sort {
		case "name":
			cmp = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case "speed_bps", "fraud_score", "diagnostic_latency":
			field := map[string]string{"speed_bps": nodefacts.FieldSpeedBps, "fraud_score": nodefacts.FieldIPQMaxFraudScore, "diagnostic_latency": nodefacts.FieldLatencyMs}[page.Sort]
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
		case "id":
			if a.ID < b.ID {
				cmp = -1
			} else if a.ID > b.ID {
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
	result := Result{Items: []Node{}, Total: len(matched), Page: page, SnapshotRevision: v.revision}
	start := min(page.Offset, len(matched))
	end := start + min(page.Limit, len(matched)-start)
	result.Items = append(result.Items, matched[start:end]...)
	return result, nil
}

// Facets counts the entire visible snapshot, not just the current page.
func (v *View) Facets() Facets        { return v.facets(false) }
func (v *View) RuntimeFacets() Facets { return v.facets(true) }
func (v *View) facets(runtimeOnly bool) Facets {
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
		if runtimeOnly && node.RuntimeTag == "" {
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
			if runtimeOnly && node.RuntimeTag == "" {
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

// Allow applies the same constraints as Query to one node from this snapshot.
func (v *View) Allow(spec Spec, node Node) (bool, error) {
	return v.allow(spec, spec.Filter(v.names), node)
}
func (v *View) allow(spec Spec, filter nodeselect.Filter, node Node) (bool, error) {
	if !filter.Allow(nodeselect.Node{ID: node.ID, Region: node.Region, Tags: node.Tags}) {
		return false, nil
	}
	if spec.Text != "" && !strings.Contains(strings.ToLower(node.Name), strings.ToLower(strings.TrimSpace(spec.Text))) {
		return false, nil
	}
	if spec.RuntimeOnly && node.RuntimeTag == "" {
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
	if preset, err := DiagnosticCondition(spec.Diagnostic); err != nil {
		return false, err
	} else if preset != nil {
		ok, err := nodefacts.Evaluate(*preset, v.facts[node.ID], v.now)
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
	filter := spec.Filter(v.names)
	for _, node := range v.nodes {
		allowed, err := v.allow(spec, filter, node)
		if err != nil {
			return nil, err
		}
		if allowed {
			ids = append(ids, node.ID)
		}
	}
	return ids, nil
}
