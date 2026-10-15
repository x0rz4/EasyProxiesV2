package monitor

import (
	"net/http"
	"sort"
	"strconv"

	"easy_proxies/internal/group"
	"easy_proxies/internal/nodeselect"
	"easy_proxies/internal/nodeview"
	"easy_proxies/internal/store"
)

func groupSelectionSpec(g store.GroupPool) nodeselect.Spec {
	return nodeselect.Spec{Enabled: g.Enabled, Regions: g.Regions, ExcludeRegions: g.ExcludeRegions, RegionRuleVersion: g.RegionRuleVersion,
		IncludeUnknownRegion: g.IncludeUnknownRegion, RequireRegion: true, ExplicitIDs: g.ExplicitNodeIDs, ExcludedIDs: g.ExcludedNodeIDs,
		TagWhitelist: g.TagWhitelist, TagBlacklist: g.TagBlacklist, TagMatch: g.TagFilterMatch}
}

func candidateIDs(candidates []nodeview.Candidate) []int64 {
	result := []int64{}
	for _, candidate := range candidates {
		if candidate.Decision.Allowed {
			result = append(result, candidate.ID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func differenceIDs(left, right []int64) []int64 {
	seen := map[int64]bool{}
	for _, id := range right {
		seen[id] = true
	}
	result := []int64{}
	for _, id := range left {
		if !seen[id] {
			result = append(result, id)
		}
	}
	return result
}

func (s *Server) handleGroupPreview(w http.ResponseWriter, r *http.Request) {
	limitTagRequestBody(w, r)
	if s.store == nil {
		writeAPIError(w, 503, "数据存储不可用")
		return
	}
	draft := store.GroupPool{Enabled: true, RegionRuleVersion: 2}
	if value := r.PathValue("groupID"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(w, 400, "无效分组 ID")
			return
		}
		existing, err := s.store.GetGroupPool(r.Context(), id)
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		if existing == nil {
			writeAPIError(w, 404, "分组不存在")
			return
		}
		draft = *existing
	}
	previous := draft
	var input groupPoolInput
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	if input.Enabled != nil {
		draft.Enabled = *input.Enabled
	}
	if input.Regions != nil {
		draft.Regions = input.Regions
	}
	if input.ExcludeRegions != nil {
		draft.ExcludeRegions = *input.ExcludeRegions
	}
	if input.RegionRuleVersion != nil {
		draft.RegionRuleVersion = *input.RegionRuleVersion
	}
	if input.IncludeUnknownRegion != nil {
		draft.IncludeUnknownRegion = *input.IncludeUnknownRegion
	}
	if input.ExplicitNodeIDs != nil {
		draft.ExplicitNodeIDs = input.ExplicitNodeIDs
	}
	if input.ExcludedNodeIDs != nil {
		draft.ExcludedNodeIDs = *input.ExcludedNodeIDs
	}
	if input.TagWhitelist != nil {
		draft.TagWhitelist = *input.TagWhitelist
	}
	if input.TagBlacklist != nil {
		draft.TagBlacklist = *input.TagBlacklist
	}
	if input.TagFilterMatch != "" {
		draft.TagFilterMatch = input.TagFilterMatch
	}
	spec := nodeview.Spec{Version: draft.RegionRuleVersion, Regions: draft.Regions, ExcludeRegions: draft.ExcludeRegions,
		IncludeUnknownRegion: draft.IncludeUnknownRegion, ExplicitIDs: draft.ExplicitNodeIDs, ExcludedIDs: draft.ExcludedNodeIDs,
		TagWhitelist: draft.TagWhitelist, TagBlacklist: draft.TagBlacklist, TagMatch: draft.TagFilterMatch}
	if err := spec.Validate(s.nodeViewRegistry()); err != nil {
		writeAPIError(w, 400, err.Error())
		return
	}
	view, err := s.loadNodeView(r.Context(), false)
	if err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	candidates := view.Candidates(groupSelectionSpec(draft))
	matched := candidateIDs(candidates)
	previousIDs := candidateIDs(view.Candidates(groupSelectionSpec(previous)))
	current := []int64{}
	for _, member := range group.GroupRuntimeSnapshots()[draft.ID].Members {
		current = append(current, member.NodeID)
	}
	sort.Slice(current, func(i, j int) bool { return current[i] < current[j] })
	writeJSON(w, map[string]any{"candidates": candidates, "matched_ids": matched, "current_ids": current,
		"added_ids": differenceIDs(matched, current), "removed_ids": differenceIDs(current, matched),
		"rule_added_ids": differenceIDs(matched, previousIDs), "rule_removed_ids": differenceIDs(previousIDs, matched),
		"region_rule_version": draft.RegionRuleVersion, "snapshot_revision": view.Revision(), "group_updated_at": previous.UpdatedAt})
}
