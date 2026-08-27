// Package nodetag turns tag rules into node tag assignments. It owns the tag
// semantics — mutex groups, manual-over-auto precedence, recompute scheduling —
// and delegates every fact and every comparison to internal/nodefacts.
package nodetag

import (
	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/store"
)

// Reasons a matched tag was not applied. They are returned by the resolver and
// surfaced by the preview endpoint so an operator can see why a rule that
// clearly matches produced no tag.
const (
	// ReasonLowerPriority means another tag in the same mutex group won.
	ReasonLowerPriority = "lower_priority"
	// ReasonManualOccupiesGroup means an operator already placed this node in
	// the mutex group by hand, and that decision outranks every rule.
	ReasonManualOccupiesGroup = "manual_occupies_group"
)

// TagMeta is what the resolver needs to know about a tag, whether or not the tag
// has a rule. Manual assignments are resolved through it, which is how a manual
// tag can occupy a mutex group.
type TagMeta struct {
	TagID        int64
	Name         string
	MutexGroupID int64
	Priority     int
}

// Rule is one auto-enabled tag plus its parsed condition.
type Rule struct {
	TagMeta
	RuleVersion int
	Condition   nodefacts.Condition
}

// ShadowNote records a tag whose rule matched but which was not applied.
type ShadowNote struct {
	TagID        int64  `json:"tag_id"`
	TagName      string `json:"tag_name"`
	MutexGroupID int64  `json:"mutex_group_id"`
	Reason       string `json:"reason"`
	// WinnerTagID is the tag holding the mutex group instead, whether it won on
	// priority or was assigned by hand.
	WinnerTagID   int64  `json:"winner_tag_id,omitempty"`
	WinnerTagName string `json:"winner_tag_name,omitempty"`
}

// Decision is the resolved auto-tag state of one node. TagIDs is sorted so a
// recompute over unchanged facts produces byte-identical assignments.
type Decision struct {
	NodeID       int64
	TagIDs       []int64
	RuleVersions []int
	Shadowed     []ShadowNote
}

// Assignment converts the decision into the store's write shape.
func (d Decision) Assignment() store.NodeAutoTagAssignment {
	return store.NodeAutoTagAssignment{
		NodeID:       d.NodeID,
		TagIDs:       d.TagIDs,
		RuleVersions: d.RuleVersions,
	}
}

// CompileRules parses the rules of every auto-enabled tag and returns them
// alongside the metadata of *all* tags. A tag whose stored rule cannot be parsed
// is skipped rather than failing the whole recompute: rules are validated at save
// time, so an unparseable one means the row was edited outside the application,
// and one bad row must not stop every other node from being tagged.
func CompileRules(tags []store.Tag) (rules []Rule, meta map[int64]TagMeta, skipped []int64) {
	meta = make(map[int64]TagMeta, len(tags))
	for _, tag := range tags {
		meta[tag.ID] = TagMeta{
			TagID:        tag.ID,
			Name:         tag.Name,
			MutexGroupID: tag.MutexGroupID,
			Priority:     tag.Priority,
		}
		if !tag.AutoEnabled || tag.RuleJSON == "" {
			continue
		}
		condition, err := nodefacts.ParseRule([]byte(tag.RuleJSON))
		if err != nil {
			skipped = append(skipped, tag.ID)
			continue
		}
		// An empty rule matches nothing, so an auto tag with no condition is not
		// a rule at all and must not be evaluated.
		if condition.IsEmpty() {
			continue
		}
		rules = append(rules, Rule{
			TagMeta:     meta[tag.ID],
			RuleVersion: tag.RuleVersion,
			Condition:   condition,
		})
	}
	return rules, meta, skipped
}
