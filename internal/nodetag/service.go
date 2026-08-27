package nodetag

import (
	"context"
	"log"
	"sort"
	"time"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/store"
)

// Preview sample bounds. The match count always covers every node; only the
// returned examples are capped.
const (
	DefaultPreviewLimit = 50
	MaxPreviewLimit     = 200
)

// draftTagID stands in for the tag of a rule that has not been saved yet, so a
// preview runs through the same resolver as a recompute. It is deliberately
// larger than any real tag ID: the resolver breaks priority ties on the lower
// ID, so an unsaved rule loses its ties and the preview shows the shadowing an
// operator would actually get rather than hiding it.
const draftTagID = int64(1) << 62

// Store is the slice of the store the service needs: everything the fact loader
// reads, plus the writes a recompute and a template seed perform.
type Store interface {
	nodefacts.Source
	ListTagMutexGroups(ctx context.Context) ([]store.TagMutexGroup, error)
	CreateTagMutexGroup(ctx context.Context, group *store.TagMutexGroup) error
	CreateTag(ctx context.Context, tag *store.Tag) error
	ReplaceAutoNodeTags(ctx context.Context, assignments []store.NodeAutoTagAssignment) error
}

// Service evaluates tag rules against node facts and persists the outcome.
//
// It never writes a manual assignment: every write goes through
// ReplaceAutoNodeTags, which touches only source='auto' rows.
type Service struct {
	store           Store
	loader          *nodefacts.Loader
	registry        *nodefacts.Registry
	limits          nodefacts.Limits
	unlockProviders []nodefacts.ProviderInfo
	now             func() time.Time
	notify          func([]int64)
	logf            func(string, ...any)
}

// Option configures a Service.
type Option func(*Service)

// WithRegistry sets the field registry used to validate rules. The registry
// carries the unlock and IP-quality providers, which is how this package stays
// clear of internal/unlock and internal/ipquality.
func WithRegistry(registry *nodefacts.Registry) Option {
	return func(s *Service) {
		if registry != nil {
			s.registry = registry
		}
	}
}

// WithLimits overrides the rule complexity limits.
func WithLimits(limits nodefacts.Limits) Option {
	return func(s *Service) { s.limits = limits }
}

// WithUnlockProviders sets the providers that get an unlock template.
func WithUnlockProviders(providers []nodefacts.ProviderInfo) Option {
	return func(s *Service) { s.unlockProviders = providers }
}

// WithClock overrides the clock used to age facts. Tests use it to make
// max_age_seconds deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithMembershipNotifier registers a callback receiving the nodes whose visible
// tag set changed. It is how a recompute reaches group membership without this
// package importing the runtime.
func WithMembershipNotifier(notify func([]int64)) Option {
	return func(s *Service) { s.notify = notify }
}

// WithLogf overrides the log sink.
func WithLogf(logf func(string, ...any)) Option {
	return func(s *Service) {
		if logf != nil {
			s.logf = logf
		}
	}
}

// NewService returns a service reading and writing through dataStore.
func NewService(dataStore Store, options ...Option) *Service {
	service := &Service{
		store:    dataStore,
		loader:   nodefacts.NewLoader(dataStore),
		registry: nodefacts.DefaultRegistry(),
		limits:   nodefacts.DefaultLimits(),
		now:      time.Now,
		logf:     log.Printf,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

// Registry returns the field registry, for the schema endpoint.
func (s *Service) Registry() *nodefacts.Registry { return s.registry }

// Limits returns the rule complexity limits.
func (s *Service) Limits() nodefacts.Limits { return s.limits }

// ValidateRule reports whether a rule is storable. An empty rule is valid and
// means "this tag has no rule".
func (s *Service) ValidateRule(condition nodefacts.Condition) error {
	return condition.Validate(s.registry, s.limits)
}

// RecomputeAll re-evaluates every node.
func (s *Service) RecomputeAll(ctx context.Context) ([]int64, error) {
	return s.Recompute(ctx, nil)
}

// Recompute re-evaluates the auto tags of the listed nodes, replaces their auto
// assignments, and returns the nodes whose *visible* tag set changed. A nil
// nodeIDs means every node.
//
// The returned IDs are what group membership reacts to, so they are the nodes
// whose nodes.tags projection changed — not every node that was rewritten. A
// node gaining an auto row for a tag it already carries by hand looks identical
// from the outside and must not cost a group rebuild.
func (s *Service) Recompute(ctx context.Context, nodeIDs []int64) ([]int64, error) {
	if nodeIDs != nil && len(nodeIDs) == 0 {
		return nil, nil
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	rules, meta, skipped := CompileRules(tags)
	for _, tagID := range skipped {
		s.logf("nodetag: 标签 %d 的规则无法解析，本次重算已跳过", tagID)
	}
	factSets, err := s.loader.Load(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	if len(factSets) == 0 {
		return nil, nil
	}
	manual, err := s.assignments(ctx, nodeIDs, store.NodeTagSourceManual)
	if err != nil {
		return nil, err
	}
	previous, err := s.assignments(ctx, nodeIDs, store.NodeTagSourceAuto)
	if err != nil {
		return nil, err
	}

	now := s.now()
	reported := map[int64]bool{}
	assignments := make([]store.NodeAutoTagAssignment, 0, len(factSets))
	var changed []int64
	for _, facts := range factSets {
		decision := Resolve(facts.NodeID, s.match(facts, rules, now, reported),
			manual[facts.NodeID], meta)
		assignments = append(assignments, decision.Assignment())
		if projectionChanged(previous[facts.NodeID], decision.TagIDs,
			manual[facts.NodeID], meta) {
			changed = append(changed, facts.NodeID)
		}
	}
	if err := s.store.ReplaceAutoNodeTags(ctx, assignments); err != nil {
		return nil, err
	}
	if len(changed) > 0 && s.notify != nil {
		s.notify(changed)
	}
	return changed, nil
}

// match returns the rules that matched one node. A rule that fails to evaluate
// is treated as no match and reported once per recompute rather than once per
// node — one broken rule must not fail the other tags or flood the log.
func (s *Service) match(facts nodefacts.NodeFacts, rules []Rule, now time.Time, reported map[int64]bool) []Rule {
	var matched []Rule
	for _, rule := range rules {
		hit, err := nodefacts.Evaluate(rule.Condition, facts, now)
		if err != nil {
			if !reported[rule.TagID] {
				reported[rule.TagID] = true
				s.logf("nodetag: 标签 %d(%s) 的规则求值失败: %v", rule.TagID, rule.Name, err)
			}
			continue
		}
		if hit {
			matched = append(matched, rule)
		}
	}
	return matched
}

// assignments groups one source's assignments by node. nodeIDs is passed through
// to the store so a subset recompute reads only the rows it needs.
func (s *Service) assignments(ctx context.Context, nodeIDs []int64, source string) (map[int64][]int64, error) {
	rows, err := s.store.ListNodeTags(ctx, store.NodeTagFilter{NodeIDs: nodeIDs, Source: source})
	if err != nil {
		return nil, err
	}
	byNode := make(map[int64][]int64)
	for _, row := range rows {
		byNode[row.NodeID] = append(byNode[row.NodeID], row.TagID)
	}
	for _, tagIDs := range byNode {
		sort.Slice(tagIDs, func(first, second int) bool { return tagIDs[first] < tagIDs[second] })
	}
	return byNode, nil
}

// projectionChanged reports whether the node's visible tag names differ before
// and after the recompute. Names, not IDs: nodes.tags is a set of names, and a
// manual assignment can already cover the name an auto rule just produced.
func projectionChanged(previousAuto, nextAuto, manual []int64, meta map[int64]TagMeta) bool {
	before := projection(previousAuto, manual, meta)
	after := projection(nextAuto, manual, meta)
	if len(before) != len(after) {
		return true
	}
	for index := range before {
		if before[index] != after[index] {
			return true
		}
	}
	return false
}

// projection is the sorted, deduplicated name set that nodes.tags will hold.
func projection(auto, manual []int64, meta map[int64]TagMeta) []string {
	seen := make(map[string]struct{}, len(auto)+len(manual))
	names := make([]string, 0, len(auto)+len(manual))
	for _, source := range [][]int64{manual, auto} {
		for _, tagID := range source {
			tag, known := meta[tagID]
			if !known || tag.Name == "" {
				continue
			}
			if _, duplicate := seen[tag.Name]; duplicate {
				continue
			}
			seen[tag.Name] = struct{}{}
			names = append(names, tag.Name)
		}
	}
	sort.Strings(names)
	return names
}

// PreviewRequest is a dry run of one rule. TagID, MutexGroupID and Priority
// describe the tag the rule is being written for: they let the preview report
// the mutex shadowing the operator would actually get. TagID also tells the
// preview which stored rule the draft replaces.
type PreviewRequest struct {
	Condition    nodefacts.Condition
	TagID        int64
	MutexGroupID int64
	Priority     int
	NodeIDs      []int64
	Limit        int
}

// PreviewNode is one example node. It carries no URI, username or password —
// only the identity an operator needs plus the facts the rule actually read.
type PreviewNode struct {
	NodeID   int64             `json:"node_id"`
	Name     string            `json:"name"`
	Region   string            `json:"region"`
	Matched  bool              `json:"matched"`
	Applied  bool              `json:"applied"`
	Shadowed *ShadowNote       `json:"shadowed,omitempty"`
	Facts    map[string]string `json:"facts"`
}

// PreviewResult counts every node but returns only a page of examples.
type PreviewResult struct {
	TotalNodes int `json:"total_nodes"`
	// MatchCount is how many nodes the rule matched; AppliedCount subtracts the
	// ones a mutex group would shadow.
	MatchCount    int `json:"match_count"`
	AppliedCount  int `json:"applied_count"`
	ShadowedCount int `json:"shadowed_count"`
	// UnknownCount is how many non-matching nodes are missing at least one fact
	// the rule reads, which is the usual reason a rule looks broken.
	UnknownCount int           `json:"unknown_count"`
	Fields       []string      `json:"fields"`
	Samples      []PreviewNode `json:"samples"`
}

// Preview evaluates an unsaved rule and writes nothing.
func (s *Service) Preview(ctx context.Context, request PreviewRequest) (*PreviewResult, error) {
	if err := s.ValidateRule(request.Condition); err != nil {
		return nil, err
	}
	limit := request.Limit
	if limit <= 0 {
		limit = DefaultPreviewLimit
	}
	if limit > MaxPreviewLimit {
		limit = MaxPreviewLimit
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	stored, meta, _ := CompileRules(tags)
	draft := Rule{TagMeta: TagMeta{
		TagID:        draftTagID,
		Name:         "（未保存规则）",
		MutexGroupID: request.MutexGroupID,
		Priority:     request.Priority,
	}, Condition: request.Condition}
	if existing, known := meta[request.TagID]; known && request.TagID != 0 {
		draft.Name = existing.Name
		// A manual assignment of the tag under edit must occupy the group the
		// draft declares, not the one the stored row declares.
		promoted := draft.TagMeta
		promoted.TagID = request.TagID
		meta[request.TagID] = promoted
	}
	meta[draftTagID] = draft.TagMeta
	others := make([]Rule, 0, len(stored))
	for _, rule := range stored {
		if request.TagID != 0 && rule.TagID == request.TagID {
			continue // the draft replaces it
		}
		others = append(others, rule)
	}
	manual, err := s.assignments(ctx, request.NodeIDs, store.NodeTagSourceManual)
	if err != nil {
		return nil, err
	}
	factSets, err := s.loader.Load(ctx, request.NodeIDs)
	if err != nil {
		return nil, err
	}

	now := s.now()
	fields := nodefacts.ReferencedFields(request.Condition)
	result := &PreviewResult{TotalNodes: len(factSets), Fields: fields, Samples: []PreviewNode{}}
	reported := map[int64]bool{}
	var fill []PreviewNode
	for _, facts := range factSets {
		matched, err := nodefacts.Evaluate(request.Condition, facts, now)
		if err != nil {
			return nil, err
		}
		applied, note := matched, (*ShadowNote)(nil)
		if matched && request.MutexGroupID != 0 {
			candidates := append(s.match(facts, others, now, reported), draft)
			decision := Resolve(facts.NodeID, candidates, manual[facts.NodeID], meta)
			applied = containsTagID(decision.TagIDs, draftTagID)
			if !applied {
				note = shadowNoteFor(decision.Shadowed, draftTagID)
			}
		}
		switch {
		case matched:
			result.MatchCount++
			if applied {
				result.AppliedCount++
			} else {
				result.ShadowedCount++
			}
		case missingFact(facts, fields):
			result.UnknownCount++
		}
		sample := PreviewNode{
			NodeID: facts.NodeID, Name: facts.Name, Region: facts.Region,
			Matched: matched, Applied: applied, Shadowed: note,
			Facts: make(map[string]string, len(fields)),
		}
		for _, field := range fields {
			sample.Facts[field] = facts.Value(field).Display()
		}
		// Matched nodes fill the page first; the rest pad it out so a rule that
		// matches nothing still shows why.
		if matched && len(result.Samples) < limit {
			result.Samples = append(result.Samples, sample)
		} else if !matched && len(fill) < limit {
			fill = append(fill, sample)
		}
	}
	for _, sample := range fill {
		if len(result.Samples) >= limit {
			break
		}
		result.Samples = append(result.Samples, sample)
	}
	return result, nil
}

func containsTagID(tagIDs []int64, wanted int64) bool {
	for _, tagID := range tagIDs {
		if tagID == wanted {
			return true
		}
	}
	return false
}

func shadowNoteFor(notes []ShadowNote, tagID int64) *ShadowNote {
	for index := range notes {
		if notes[index].TagID == tagID {
			return &notes[index]
		}
	}
	return nil
}

// missingFact reports whether the node has never measured one of the fields the
// rule reads. It looks at the raw facts rather than the aged ones, so a fact that
// exists but is too old for the rule counts as present.
func missingFact(facts nodefacts.NodeFacts, fields []string) bool {
	for _, field := range fields {
		if !facts.Value(field).Known {
			return true
		}
	}
	return false
}

// SeedResult reports what a template seed did, by tag name.
type SeedResult struct {
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
	// Conflicts are templates whose name is already taken by a tag that was not
	// seeded from this template. They are left alone: silently turning an
	// operator's hand-made tag into a rule-driven one would retag every node.
	Conflicts []string `json:"conflicts"`
}

// SeedTemplates creates the builtin tags that do not exist yet. It is idempotent
// through tags.builtin_key, so a renamed builtin is not seeded twice.
//
// Nothing calls this during a migration: creating auto rules on upgrade would
// rewrite every node's tags before anyone asked for it.
func (s *Service) SeedTemplates(ctx context.Context) (*SeedResult, error) {
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	seeded := make(map[string]struct{}, len(tags))
	taken := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if tag.BuiltinKey != "" {
			seeded[tag.BuiltinKey] = struct{}{}
		}
		taken[tag.Name] = struct{}{}
	}
	groups, err := s.store.ListTagMutexGroups(ctx)
	if err != nil {
		return nil, err
	}
	groupIDs := make(map[string]int64, len(groups))
	for _, group := range groups {
		groupIDs[group.Name] = group.ID
	}

	result := &SeedResult{}
	for _, template := range Templates(s.unlockProviders) {
		if _, done := seeded[template.BuiltinKey]; done {
			result.Skipped = append(result.Skipped, template.Name)
			continue
		}
		if _, clash := taken[template.Name]; clash {
			result.Conflicts = append(result.Conflicts, template.Name)
			continue
		}
		groupID := int64(0)
		if template.MutexGroup != "" {
			if groupID, err = s.ensureMutexGroup(ctx, template.MutexGroup, groupIDs); err != nil {
				return nil, err
			}
		}
		ruleJSON, err := nodefacts.MarshalRule(template.Condition)
		if err != nil {
			return nil, err
		}
		tag := &store.Tag{
			Name:         template.Name,
			Color:        template.Color,
			Description:  template.Description,
			MutexGroupID: groupID,
			Priority:     template.Priority,
			AutoEnabled:  true,
			RuleJSON:     string(ruleJSON),
			RuleVersion:  1,
			BuiltinKey:   template.BuiltinKey,
		}
		if err := s.store.CreateTag(ctx, tag); err != nil {
			return nil, err
		}
		taken[tag.Name] = struct{}{}
		result.Created = append(result.Created, tag.Name)
	}
	return result, nil
}

func (s *Service) ensureMutexGroup(ctx context.Context, name string, cache map[string]int64) (int64, error) {
	if groupID, known := cache[name]; known {
		return groupID, nil
	}
	group := &store.TagMutexGroup{Name: name, Description: "由内置标签模板创建"}
	if err := s.store.CreateTagMutexGroup(ctx, group); err != nil {
		return 0, err
	}
	cache[name] = group.ID
	return group.ID, nil
}
