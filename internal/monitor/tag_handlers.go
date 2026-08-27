package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"easy_proxies/internal/nodefacts"
	"easy_proxies/internal/nodetag"
	"easy_proxies/internal/store"
)

const maxTagRequestBytes = 64 << 10

type tagInput struct {
	Name         *string              `json:"name"`
	Color        *string              `json:"color"`
	Description  *string              `json:"description"`
	MutexGroupID *int64               `json:"mutex_group_id"`
	Priority     *int                 `json:"priority"`
	AutoEnabled  *bool                `json:"auto_enabled"`
	Rule         *nodefacts.Condition `json:"rule"`
}

type tagView struct {
	store.Tag
	Rule         *nodefacts.Condition `json:"rule,omitempty"`
	NodeCount    int                  `json:"node_count"`
	ManualCount  int                  `json:"manual_count"`
	AutoCount    int                  `json:"auto_count"`
	UsedByGroups []int64              `json:"used_by_groups"`
}

type nodeTagAssignmentView struct {
	NodeID       int64   `json:"node_id"`
	ManualTagIDs []int64 `json:"manual_tag_ids"`
	AutoTagIDs   []int64 `json:"auto_tag_ids"`
}

type mutexGroupInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	limitTagRequestBody(w, r)
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		tags, groups, err := s.tagViews(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"tags": tags, "mutex_groups": groups})
	case http.MethodPost:
		var input tagInput
		if err := decodeTagJSON(w, r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		tag, changed, err := s.tagFromInput(r.Context(), input, nil)
		if err != nil {
			writeTagMutationError(w, err)
			return
		}
		if err := s.store.CreateTag(r.Context(), tag); err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		if changed {
			s.enqueueRetagAll()
		}
		view, err := s.tagViewByID(r.Context(), tag.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"tag": view})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
	}
}

// handleTagItem dispatches literal tag subresources before trying a numeric
// tag ID. In particular, schema and nodes must never be parsed as tag IDs.
func (s *Server) handleTagItem(w http.ResponseWriter, r *http.Request) {
	limitTagRequestBody(w, r)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tags/"), "/")
	if path == "" {
		writeAPIError(w, http.StatusNotFound, "接口不存在")
		return
	}
	parts := strings.Split(path, "/")
	switch parts[0] {
	case "schema":
		if len(parts) == 1 {
			s.handleTagSchema(w, r)
			return
		}
	case "preview":
		if len(parts) == 1 {
			s.handleTagPreview(w, r)
			return
		}
	case "recompute":
		if len(parts) == 1 {
			s.handleTagRecompute(w, r)
			return
		}
	case "templates":
		if len(parts) == 1 {
			s.handleTagTemplates(w, r)
			return
		}
	case "assignments":
		if len(parts) == 1 {
			s.handleTagAssignments(w, r)
			return
		}
	case "mutex-groups":
		if len(parts) <= 2 {
			s.handleTagMutexGroups(w, r, parts[1:])
			return
		}
	case "nodes":
		if len(parts) == 2 {
			s.handleTagNodes(w, r, parts[1])
			return
		}
	}

	tagID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || tagID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效的标签 ID")
		return
	}
	if len(parts) == 2 && parts[1] == "auto" {
		s.handleTagAuto(w, r, tagID)
		return
	}
	if len(parts) != 1 {
		writeAPIError(w, http.StatusNotFound, "接口不存在")
		return
	}
	s.handleTagByID(w, r, tagID)
}

func (s *Server) handleTagByID(w http.ResponseWriter, r *http.Request, tagID int64) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	existing, err := s.store.GetTag(r.Context(), tagID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "标签不存在")
		return
	}
	switch r.Method {
	case http.MethodGet:
		view, err := s.tagViewByID(r.Context(), tagID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"tag": view})
	case http.MethodPut:
		var input tagInput
		if err := decodeTagJSON(w, r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.updateTag(w, r, existing, input)
	case http.MethodDelete:
		s.deleteTag(w, r, existing)
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
	}
}

func (s *Server) handleTagAuto(w http.ResponseWriter, r *http.Request, tagID int64) {
	if r.Method != http.MethodPatch {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	var input struct {
		AutoEnabled *bool `json:"auto_enabled"`
	}
	if err := decodeTagJSON(w, r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.AutoEnabled == nil {
		writeAPIError(w, http.StatusBadRequest, "缺少 auto_enabled")
		return
	}
	existing, err := s.store.GetTag(r.Context(), tagID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "标签不存在")
		return
	}
	s.updateTag(w, r, existing, tagInput{AutoEnabled: input.AutoEnabled})
}

func (s *Server) updateTag(w http.ResponseWriter, r *http.Request, existing *store.Tag, input tagInput) {
	affected, err := s.store.ListNodeTags(r.Context(), store.NodeTagFilter{TagIDs: []int64{existing.ID}})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	affectedNodeIDs := nodeIDsFromAssignments(affected)
	updated, semanticChanged, err := s.tagFromInput(r.Context(), input, existing)
	if err != nil {
		writeTagMutationError(w, err)
		return
	}
	rename := updated.Name != existing.Name
	if rename && len(affectedNodeIDs) > 0 && s.tagSvc == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "标签服务不可用，无法刷新标签投影")
		return
	}
	if err := s.store.UpdateTag(r.Context(), updated); err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	if rename && len(affectedNodeIDs) > 0 {
		if _, err := s.tagSvc.Recompute(r.Context(), affectedNodeIDs); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.refreshGroupMembership(affectedNodeIDs); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if semanticChanged {
		s.enqueueRetagAll()
	}
	view, err := s.tagViewByID(r.Context(), updated.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"tag": view})
}

func (s *Server) deleteTag(w http.ResponseWriter, r *http.Request, tag *store.Tag) {
	groups, err := s.store.ListGroupPools(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var referenced []*store.GroupPool
	var usedBy []int64
	for index := range groups {
		group := &groups[index]
		if containsInt64(group.TagWhitelist, tag.ID) || containsInt64(group.TagBlacklist, tag.ID) {
			referenced = append(referenced, cloneGroupPool(group))
			usedBy = append(usedBy, group.ID)
		}
	}
	force := r.URL.Query().Get("force") == "1" || strings.EqualFold(r.URL.Query().Get("force"), "true")
	if len(usedBy) > 0 && !force {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "标签正被分组引用", "used_by_groups": usedBy})
		return
	}
	assignments, err := s.store.ListNodeTags(r.Context(), store.NodeTagFilter{TagIDs: []int64{tag.ID}})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	affectedNodeIDs := nodeIDsFromAssignments(assignments)
	if err := s.store.DeleteTag(r.Context(), tag.ID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var runtimeErrors []string
	for _, before := range referenced {
		after, lookupErr := s.store.GetGroupPool(r.Context(), before.ID)
		if lookupErr != nil {
			runtimeErrors = append(runtimeErrors, lookupErr.Error())
			continue
		}
		if reloadError := s.applyGroupRuntimeMutation(r.Context(), before, after); reloadError != "" {
			runtimeErrors = append(runtimeErrors, fmt.Sprintf("分组 %d: %s", before.ID, reloadError))
		}
	}
	if err := s.refreshGroupMembership(affectedNodeIDs); err != nil {
		runtimeErrors = append(runtimeErrors, err.Error())
	}
	s.enqueueRetag(affectedNodeIDs...)
	response := map[string]any{
		"ok": true, "deleted_id": tag.ID, "affected_node_ids": affectedNodeIDs,
		"updated_group_ids": usedBy,
	}
	if len(runtimeErrors) > 0 {
		response["runtime_errors"] = runtimeErrors
	}
	writeJSON(w, response)
}

func (s *Server) handleTagPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if s.tagSvc == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "标签服务不可用")
		return
	}
	var input struct {
		Rule         nodefacts.Condition `json:"rule"`
		TagID        int64               `json:"tag_id"`
		MutexGroupID int64               `json:"mutex_group_id"`
		Priority     int                 `json:"priority"`
		NodeIDs      []int64             `json:"node_ids"`
		Limit        int                 `json:"limit"`
	}
	if err := decodeTagJSON(w, r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Priority < 0 || input.Priority > 1000 {
		writeAPIError(w, http.StatusBadRequest, "priority 必须在 0..1000 之间")
		return
	}
	if err := validatePositiveIDList("node_ids", input.NodeIDs); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateMutexGroupID(r.Context(), input.MutexGroupID); err != nil {
		writeTagMutationError(w, err)
		return
	}
	result, err := s.tagSvc.Preview(r.Context(), nodetag.PreviewRequest{
		Condition: input.Rule, TagID: input.TagID, MutexGroupID: input.MutexGroupID,
		Priority: input.Priority, NodeIDs: normalizePositiveIDs(input.NodeIDs), Limit: input.Limit,
	})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleTagRecompute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if s.tagSvc == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "标签服务不可用")
		return
	}
	var input struct {
		NodeIDs *[]int64 `json:"node_ids"`
	}
	if err := decodeTagJSON(w, r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	var nodeIDs []int64
	if input.NodeIDs != nil {
		if err := validatePositiveIDList("node_ids", *input.NodeIDs); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		nodeIDs = normalizePositiveIDs(*input.NodeIDs)
	}
	changed, err := s.tagSvc.Recompute(r.Context(), nodeIDs)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"changed_node_ids": changed})
}

func (s *Server) handleTagTemplates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if s.tagSvc == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "标签服务不可用")
		return
	}
	result, err := s.tagSvc.SeedTemplates(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, result)
}

func (s *Server) handleTagAssignments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	nodeIDs, err := parseTagIDQuery(r.URL.Query().Get("node_ids"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := s.store.ListNodeTags(r.Context(), store.NodeTagFilter{NodeIDs: nodeIDs})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byNode := make(map[int64]*nodeTagAssignmentView)
	for _, nodeID := range nodeIDs {
		byNode[nodeID] = &nodeTagAssignmentView{NodeID: nodeID, ManualTagIDs: []int64{}, AutoTagIDs: []int64{}}
	}
	for _, row := range rows {
		entry := byNode[row.NodeID]
		if entry == nil {
			entry = &nodeTagAssignmentView{NodeID: row.NodeID, ManualTagIDs: []int64{}, AutoTagIDs: []int64{}}
			byNode[row.NodeID] = entry
		}
		if row.Source == store.NodeTagSourceManual {
			entry.ManualTagIDs = append(entry.ManualTagIDs, row.TagID)
		} else if row.Source == store.NodeTagSourceAuto {
			entry.AutoTagIDs = append(entry.AutoTagIDs, row.TagID)
		}
	}
	assignments := make([]nodeTagAssignmentView, 0, len(byNode))
	for _, entry := range byNode {
		assignments = append(assignments, *entry)
	}
	sort.Slice(assignments, func(i, j int) bool { return assignments[i].NodeID < assignments[j].NodeID })
	writeJSON(w, map[string]any{"assignments": assignments})
}

func (s *Server) handleTagNodes(w http.ResponseWriter, r *http.Request, target string) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	if target == "batch" {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
			return
		}
		var input struct {
			NodeIDs      []int64 `json:"node_ids"`
			AddTagIDs    []int64 `json:"add_tag_ids"`
			RemoveTagIDs []int64 `json:"remove_tag_ids"`
		}
		if err := decodeTagJSON(w, r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validatePositiveIDList("node_ids", input.NodeIDs); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validatePositiveIDList("add_tag_ids", input.AddTagIDs); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validatePositiveIDList("remove_tag_ids", input.RemoveTagIDs); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		nodeIDs := normalizePositiveIDs(input.NodeIDs)
		adds, removes := normalizePositiveIDs(input.AddTagIDs), normalizePositiveIDs(input.RemoveTagIDs)
		if len(nodeIDs) == 0 || (len(adds) == 0 && len(removes) == 0) {
			writeAPIError(w, http.StatusBadRequest, "node_ids 不能为空，且至少要添加或移除一个标签")
			return
		}
		if err := s.validateNodeAndTagIDs(r.Context(), nodeIDs, append(append([]int64(nil), adds...), removes...)); err != nil {
			writeTagMutationError(w, err)
			return
		}
		if err := s.store.BatchUpdateManualNodeTags(r.Context(), nodeIDs, adds, removes); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		reloadError := ""
		if err := s.refreshGroupMembership(nodeIDs); err != nil {
			reloadError = err.Error()
		}
		s.enqueueRetag(nodeIDs...)
		writeJSON(w, map[string]any{"ok": true, "node_ids": nodeIDs, "reload_error": reloadError})
		return
	}

	if r.Method != http.MethodPut {
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	nodeID, err := strconv.ParseInt(target, 10, 64)
	if err != nil || nodeID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效的节点 ID")
		return
	}
	var input struct {
		TagIDs []int64 `json:"tag_ids"`
	}
	if err := decodeTagJSON(w, r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePositiveIDList("tag_ids", input.TagIDs); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	tagIDs := normalizePositiveIDs(input.TagIDs)
	if err := s.validateNodeAndTagIDs(r.Context(), []int64{nodeID}, tagIDs); err != nil {
		writeTagMutationError(w, err)
		return
	}
	if err := s.store.SetManualNodeTags(r.Context(), nodeID, tagIDs); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	reloadError := ""
	if err := s.refreshGroupMembership([]int64{nodeID}); err != nil {
		reloadError = err.Error()
	}
	s.enqueueRetag(nodeID)
	assignment, err := s.nodeTagAssignment(r.Context(), nodeID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"assignment": assignment, "reload_error": reloadError})
}

func (s *Server) handleTagMutexGroups(w http.ResponseWriter, r *http.Request, tail []string) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	groups, err := s.store.ListTagMutexGroups(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"mutex_groups": groups})
		case http.MethodPost:
			var input mutexGroupInput
			if err := decodeTagJSON(w, r, &input); err != nil {
				writeAPIError(w, http.StatusBadRequest, err.Error())
				return
			}
			group, err := mutexGroupFromInput(input, nil, groups)
			if err != nil {
				writeTagMutationError(w, err)
				return
			}
			if err := s.store.CreateTagMutexGroup(r.Context(), group); err != nil {
				writeAPIError(w, http.StatusConflict, err.Error())
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"mutex_group": group})
		default:
			writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		}
		return
	}
	groupID, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil || groupID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效的互斥组 ID")
		return
	}
	var existing *store.TagMutexGroup
	for index := range groups {
		if groups[index].ID == groupID {
			existing = &groups[index]
			break
		}
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "互斥组不存在")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"mutex_group": existing})
	case http.MethodPut:
		var input mutexGroupInput
		if err := decodeTagJSON(w, r, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		updated, err := mutexGroupFromInput(input, existing, groups)
		if err != nil {
			writeTagMutationError(w, err)
			return
		}
		if err := s.store.UpdateTagMutexGroup(r.Context(), updated); err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]any{"mutex_group": updated})
	case http.MethodDelete:
		if err := s.store.DeleteTagMutexGroup(r.Context(), groupID); err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.enqueueRetagAll()
		writeJSON(w, map[string]any{"ok": true, "deleted_id": groupID})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "请求方法不允许")
	}
}

func (s *Server) tagFromInput(ctx context.Context, input tagInput, existing *store.Tag) (*store.Tag, bool, error) {
	var tag store.Tag
	if existing != nil {
		tag = *existing
	}
	if input.Name != nil {
		name, err := validateTagName(*input.Name)
		if err != nil {
			return nil, false, err
		}
		duplicate, err := s.store.GetTagByName(ctx, name)
		if err != nil {
			return nil, false, err
		}
		if duplicate != nil && (existing == nil || duplicate.ID != existing.ID) {
			return nil, false, tagConflictError{name: name}
		}
		tag.Name = name
	} else if existing == nil {
		return nil, false, errors.New("标签名称不能为空")
	}
	if input.Color != nil {
		tag.Color = strings.TrimSpace(*input.Color)
	}
	if input.Description != nil {
		tag.Description = strings.TrimSpace(*input.Description)
	}
	semanticChanged := false
	if input.MutexGroupID != nil {
		if err := s.validateMutexGroupID(ctx, *input.MutexGroupID); err != nil {
			return nil, false, err
		}
		if tag.MutexGroupID != *input.MutexGroupID {
			semanticChanged = true
		}
		tag.MutexGroupID = *input.MutexGroupID
	}
	if input.Priority != nil {
		if *input.Priority < 0 || *input.Priority > 1000 {
			return nil, false, errors.New("priority 必须在 0..1000 之间")
		}
		if tag.Priority != *input.Priority {
			semanticChanged = true
		}
		tag.Priority = *input.Priority
	}
	if input.Rule != nil {
		if s.tagSvc == nil {
			return nil, false, errors.New("标签服务不可用")
		}
		if err := s.tagSvc.ValidateRule(*input.Rule); err != nil {
			return nil, false, err
		}
		ruleJSON, err := nodefacts.MarshalRule(*input.Rule)
		if err != nil {
			return nil, false, err
		}
		if tag.RuleJSON != string(ruleJSON) {
			semanticChanged = true
			tag.RuleJSON = string(ruleJSON)
			if existing == nil {
				tag.RuleVersion = 1
			} else {
				tag.RuleVersion++
			}
		}
	}
	if input.AutoEnabled != nil {
		if tag.AutoEnabled != *input.AutoEnabled {
			semanticChanged = true
		}
		tag.AutoEnabled = *input.AutoEnabled
	}
	if tag.AutoEnabled && strings.TrimSpace(tag.RuleJSON) == "" {
		return nil, false, errors.New("启用自动打标前必须配置规则")
	}
	if tag.RuleVersion < 1 {
		tag.RuleVersion = 1
	}
	return &tag, semanticChanged, nil
}

func (s *Server) tagViews(ctx context.Context) ([]tagView, []store.TagMutexGroup, error) {
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.store.ListTagMutexGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	counts, err := s.store.CountNodesByTag(ctx)
	if err != nil {
		return nil, nil, err
	}
	assignments, err := s.store.ListNodeTags(ctx, store.NodeTagFilter{})
	if err != nil {
		return nil, nil, err
	}
	manual, automatic := map[int64]int{}, map[int64]int{}
	for _, assignment := range assignments {
		if assignment.Source == store.NodeTagSourceManual {
			manual[assignment.TagID]++
		} else if assignment.Source == store.NodeTagSourceAuto {
			automatic[assignment.TagID]++
		}
	}
	pools, err := s.store.ListGroupPools(ctx)
	if err != nil {
		return nil, nil, err
	}
	used := make(map[int64][]int64)
	for _, pool := range pools {
		for _, tagID := range append(append([]int64(nil), pool.TagWhitelist...), pool.TagBlacklist...) {
			if !containsInt64(used[tagID], pool.ID) {
				used[tagID] = append(used[tagID], pool.ID)
			}
		}
	}
	views := make([]tagView, 0, len(tags))
	for _, tag := range tags {
		views = append(views, makeTagView(tag, counts, manual, automatic, used[tag.ID]...))
	}
	return views, groups, nil
}

func (s *Server) tagViewByID(ctx context.Context, tagID int64) (*tagView, error) {
	views, _, err := s.tagViews(ctx)
	if err != nil {
		return nil, err
	}
	for index := range views {
		if views[index].ID == tagID {
			return &views[index], nil
		}
	}
	return nil, nil
}

func makeTagView(tag store.Tag, counts, manual, automatic map[int64]int, used ...int64) tagView {
	view := tagView{Tag: tag, UsedByGroups: append([]int64(nil), used...)}
	if view.UsedByGroups == nil {
		view.UsedByGroups = []int64{}
	}
	if counts != nil {
		view.NodeCount = counts[tag.ID]
	}
	if manual != nil {
		view.ManualCount = manual[tag.ID]
	}
	if automatic != nil {
		view.AutoCount = automatic[tag.ID]
	}
	if condition, err := nodefacts.ParseRule([]byte(tag.RuleJSON)); err == nil && !condition.IsEmpty() {
		view.Rule = &condition
	}
	return view
}

func (s *Server) validateMutexGroupID(ctx context.Context, groupID int64) error {
	if groupID == 0 {
		return nil
	}
	if groupID < 0 {
		return errors.New("mutex_group_id 不能为负数")
	}
	if s.store == nil {
		return errors.New("数据存储不可用")
	}
	groups, err := s.store.ListTagMutexGroups(ctx)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.ID == groupID {
			return nil
		}
	}
	return fmt.Errorf("互斥组不存在: %d", groupID)
}

func (s *Server) validateNodeAndTagIDs(ctx context.Context, nodeIDs, tagIDs []int64) error {
	nodes, err := s.store.ListNodes(ctx, store.NodeFilter{NodeIDs: nodeIDs})
	if err != nil {
		return err
	}
	knownNodes := make(map[int64]struct{}, len(nodes))
	for _, node := range nodes {
		knownNodes[node.ID] = struct{}{}
	}
	for _, nodeID := range nodeIDs {
		if _, ok := knownNodes[nodeID]; !ok {
			return fmt.Errorf("节点不存在: %d", nodeID)
		}
	}
	if len(tagIDs) == 0 {
		return nil
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return err
	}
	knownTags := make(map[int64]struct{}, len(tags))
	for _, tag := range tags {
		knownTags[tag.ID] = struct{}{}
	}
	for _, tagID := range tagIDs {
		if _, ok := knownTags[tagID]; !ok {
			return fmt.Errorf("标签不存在: %d", tagID)
		}
	}
	return nil
}

func (s *Server) nodeTagAssignment(ctx context.Context, nodeID int64) (nodeTagAssignmentView, error) {
	view := nodeTagAssignmentView{NodeID: nodeID, ManualTagIDs: []int64{}, AutoTagIDs: []int64{}}
	rows, err := s.store.ListNodeTags(ctx, store.NodeTagFilter{NodeIDs: []int64{nodeID}})
	if err != nil {
		return view, err
	}
	for _, row := range rows {
		if row.Source == store.NodeTagSourceManual {
			view.ManualTagIDs = append(view.ManualTagIDs, row.TagID)
		} else if row.Source == store.NodeTagSourceAuto {
			view.AutoTagIDs = append(view.AutoTagIDs, row.TagID)
		}
	}
	return view, nil
}

type tagConflictError struct{ name string }

func (e tagConflictError) Error() string { return fmt.Sprintf("标签名称已存在: %s", e.name) }

func writeTagMutationError(w http.ResponseWriter, err error) {
	var conflict tagConflictError
	if errors.As(err, &conflict) {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "不可用") && strings.Contains(message, "服务") {
		writeAPIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeAPIError(w, http.StatusBadRequest, err.Error())
}

func validateTagName(value string) (string, error) {
	value = strings.TrimSpace(value)
	length := utf8.RuneCountInString(value)
	if length < 1 || length > 64 {
		return "", errors.New("标签名称长度必须为 1..64 个字符")
	}
	return value, nil
}

func mutexGroupFromInput(input mutexGroupInput, existing *store.TagMutexGroup, groups []store.TagMutexGroup) (*store.TagMutexGroup, error) {
	var group store.TagMutexGroup
	if existing != nil {
		group = *existing
	}
	if input.Name != nil {
		name, err := validateTagName(*input.Name)
		if err != nil {
			return nil, err
		}
		for _, candidate := range groups {
			if candidate.Name == name && (existing == nil || candidate.ID != existing.ID) {
				return nil, tagConflictError{name: name}
			}
		}
		group.Name = name
	} else if existing == nil {
		return nil, errors.New("互斥组名称不能为空")
	}
	if input.Description != nil {
		group.Description = strings.TrimSpace(*input.Description)
	}
	return &group, nil
}

func decodeTagJSON(_ http.ResponseWriter, r *http.Request, dst any) error {
	return decodeJSON(r, dst)
}

func limitTagRequestBody(w http.ResponseWriter, r *http.Request) {
	if r != nil && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxTagRequestBytes)
	}
}

func parseTagIDQuery(value string) ([]int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("node_ids 必须是逗号分隔的正整数")
		}
		ids = append(ids, id)
	}
	return normalizePositiveIDs(ids), nil
}

func normalizePositiveIDs(ids []int64) []int64 {
	result := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func validatePositiveIDList(field string, ids []int64) error {
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("%s 只能包含正整数", field)
		}
	}
	return nil
}

func nodeIDsFromAssignments(assignments []store.NodeTag) []int64 {
	ids := make([]int64, 0, len(assignments))
	seen := make(map[int64]struct{}, len(assignments))
	for _, assignment := range assignments {
		if _, exists := seen[assignment.NodeID]; exists {
			continue
		}
		seen[assignment.NodeID] = struct{}{}
		ids = append(ids, assignment.NodeID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
