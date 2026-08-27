package nodetag

import "sort"

// Resolve picks the auto tags of one node out of the rules that matched it.
//
// It is a pure function with a total order over its inputs, so the same facts
// always yield the same assignment — a recompute is idempotent and its output is
// byte-stable, which is what lets the caller diff assignments to decide whether a
// group needs rebuilding.
//
// The mutex-group invariant is "at most one tag per node per group". Precedence:
//
//  1. A manual assignment in the group blocks every rule in that group. The
//     invariant allows only one tag, and an operator's explicit choice outranks
//     a rule — otherwise a hand-placed tag would flicker on every recompute.
//  2. Otherwise the highest priority wins; ties break on the lower tag ID so the
//     winner never depends on map or query ordering.
//
// Tags in no mutex group are unconstrained.
func Resolve(nodeID int64, matched []Rule, manualTagIDs []int64, meta map[int64]TagMeta) Decision {
	ordered := make([]Rule, len(matched))
	copy(ordered, matched)
	sort.Slice(ordered, func(first, second int) bool {
		if ordered[first].Priority != ordered[second].Priority {
			return ordered[first].Priority > ordered[second].Priority
		}
		return ordered[first].TagID < ordered[second].TagID
	})

	// occupants maps a mutex group to the tag already holding it.
	occupants := map[int64]TagMeta{}
	manualGroups := map[int64]struct{}{}
	for _, tagID := range manualTagIDs {
		manual, known := meta[tagID]
		if !known || manual.MutexGroupID == 0 {
			continue
		}
		if _, taken := occupants[manual.MutexGroupID]; taken {
			continue
		}
		occupants[manual.MutexGroupID] = manual
		manualGroups[manual.MutexGroupID] = struct{}{}
	}

	decision := Decision{NodeID: nodeID}
	for _, rule := range ordered {
		if rule.MutexGroupID == 0 {
			decision.TagIDs = append(decision.TagIDs, rule.TagID)
			decision.RuleVersions = append(decision.RuleVersions, rule.RuleVersion)
			continue
		}
		if winner, taken := occupants[rule.MutexGroupID]; taken {
			reason := ReasonLowerPriority
			if _, byHand := manualGroups[rule.MutexGroupID]; byHand {
				reason = ReasonManualOccupiesGroup
			}
			decision.Shadowed = append(decision.Shadowed, ShadowNote{
				TagID:         rule.TagID,
				TagName:       rule.Name,
				MutexGroupID:  rule.MutexGroupID,
				Reason:        reason,
				WinnerTagID:   winner.TagID,
				WinnerTagName: winner.Name,
			})
			continue
		}
		occupants[rule.MutexGroupID] = rule.TagMeta
		decision.TagIDs = append(decision.TagIDs, rule.TagID)
		decision.RuleVersions = append(decision.RuleVersions, rule.RuleVersion)
	}

	sortByTagID(decision.TagIDs, decision.RuleVersions)
	sort.Slice(decision.Shadowed, func(first, second int) bool {
		return decision.Shadowed[first].TagID < decision.Shadowed[second].TagID
	})
	return decision
}

// sortByTagID sorts tag IDs ascending, keeping the parallel rule versions aligned.
func sortByTagID(tagIDs []int64, ruleVersions []int) {
	sort.Sort(&tagAssignment{tagIDs: tagIDs, ruleVersions: ruleVersions})
}

type tagAssignment struct {
	tagIDs       []int64
	ruleVersions []int
}

func (a *tagAssignment) Len() int { return len(a.tagIDs) }

func (a *tagAssignment) Less(first, second int) bool {
	return a.tagIDs[first] < a.tagIDs[second]
}

func (a *tagAssignment) Swap(first, second int) {
	a.tagIDs[first], a.tagIDs[second] = a.tagIDs[second], a.tagIDs[first]
	a.ruleVersions[first], a.ruleVersions[second] = a.ruleVersions[second], a.ruleVersions[first]
}
