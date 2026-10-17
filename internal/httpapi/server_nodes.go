package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
)

// nodePayload is the JSON request body for node CRUD operations.
type nodePayload struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Port     uint16 `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p nodePayload) toConfig() config.NodeConfig {
	return config.NodeConfig{
		Name:     p.Name,
		URI:      p.URI,
		Port:     p.Port,
		Username: p.Username,
		Password: p.Password,
	}
}

// configNodeName resolves the {name} path segment, answering 400 when it is
// missing. Every /api/nodes/config/{name} handler starts here.
func (s *Server) configNodeName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点名称无效"})
		return "", false
	}
	return name, true
}

// handleConfigNodesList lists config nodes, optionally filtered by subscription.
func (s *Server) handleConfigNodesList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	var subscriptionID *int64
	if raw := r.URL.Query().Get("subscription_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "subscription_id 必须是正整数"})
			return
		}
		subscriptionID = &id
	}
	nodes, err := s.nodeMgr.ListConfigNodes(r.Context(), subscriptionID)
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"nodes": nodes})
}

func (s *Server) handleConfigNodesCreate(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	var payload nodePayload
	if err := decodeJSON(r, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	node, err := s.nodeMgr.CreateNode(r.Context(), payload.toConfig())
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	reloadError := s.reloadAfterGroupMutation(r.Context())
	writeJSON(w, map[string]any{"node": node, "message": "节点已添加", "reloaded": reloadError == "", "reload_error": reloadError})
}

func (s *Server) handleConfigNodeUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	nodeName, ok := s.configNodeName(w, r)
	if !ok {
		return
	}
	var payload nodePayload
	if err := decodeJSON(r, &payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	node, err := s.nodeMgr.UpdateNode(r.Context(), nodeName, payload.toConfig())
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	// An edit can change the URI, which drops the cached detection facts an
	// auto tag was derived from.
	s.enqueueRetagAll()
	reloadError := s.reloadAfterGroupMutation(r.Context())
	writeJSON(w, map[string]any{"node": node, "message": "节点已更新", "reloaded": reloadError == "", "reload_error": reloadError})
}

func (s *Server) handleConfigNodeToggle(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	nodeName, ok := s.configNodeName(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	if body.Enabled == nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "缺少 enabled 字段"})
		return
	}
	if err := s.nodeMgr.SetNodeEnabled(r.Context(), nodeName, *body.Enabled); err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	state := "已启用"
	if !*body.Enabled {
		state = "已禁用"
	}
	// Auto-reload after toggle
	reloadMsg := ""
	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.logger.Printf("auto-reload after toggle failed: %v", err)
		reloadMsg = "（自动重载失败，请手动重载）"
	} else {
		reloadMsg = "（已自动重载）"
	}
	writeJSON(w, map[string]any{"message": fmt.Sprintf("节点 %s %s%s", nodeName, state, reloadMsg)})
}

func (s *Server) handleConfigNodeDelete(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	nodeName, ok := s.configNodeName(w, r)
	if !ok {
		return
	}
	if err := s.nodeMgr.DeleteNode(r.Context(), nodeName); err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	reloadError := s.reloadAfterGroupMutation(r.Context())
	writeJSON(w, map[string]any{"message": "节点已删除", "reloaded": reloadError == "", "reload_error": reloadError})
}

// handleConfigNodesBatchToggle handles batch enable/disable for multiple nodes.
func (s *Server) handleConfigNodesBatchToggle(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	var body struct {
		Names   []string `json:"names"`
		IDs     []int64  `json:"ids"`
		Enabled bool     `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	targets, ok := nodeTargets(w, body.Names, body.IDs)
	if !ok {
		return
	}

	var errs []string
	successCount := 0
	for _, target := range targets {
		if err := s.setNodeEnabledTarget(r.Context(), target, body.Enabled); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.label(), err))
		} else {
			successCount++
		}
	}

	action := "启用"
	if !body.Enabled {
		action = "禁用"
	}
	if successCount > 0 {
		s.enqueueRetagAll()
	}
	// Auto-reload after batch toggle
	reloadMsg := ""
	if successCount > 0 {
		if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
			s.logger.Printf("auto-reload after batch toggle failed: %v", err)
			reloadMsg = "（自动重载失败，请手动重载）"
		} else {
			reloadMsg = "（已自动重载）"
		}
	}

	result := map[string]any{
		"message": fmt.Sprintf("成功%s %d 个节点%s", action, successCount, reloadMsg),
		"success": successCount,
		"total":   len(targets),
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
}

// handleConfigNodesBatchDelete handles batch deletion for multiple nodes.
func (s *Server) handleConfigNodesBatchDelete(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	var body struct {
		Names []string `json:"names"`
		IDs   []int64  `json:"ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	targets, ok := nodeTargets(w, body.Names, body.IDs)
	if !ok {
		return
	}

	var errs []string
	successCount := 0
	for _, target := range targets {
		if err := s.deleteNodeTarget(r.Context(), target); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target.label(), err))
		} else {
			successCount++
		}
	}

	// Auto-reload after batch delete
	reloadMsg := ""
	if successCount > 0 {
		s.enqueueRetagAll()
		if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
			s.logger.Printf("auto-reload after batch delete failed: %v", err)
			reloadMsg = "（自动重载失败，请手动重载）"
		} else {
			reloadMsg = "（已自动重载）"
		}
	}

	result := map[string]any{
		"message": fmt.Sprintf("成功删除 %d 个节点%s", successCount, reloadMsg),
		"success": successCount,
		"total":   len(targets),
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
}

// nodeTarget is one addressable node in a batch request. Callers that have an
// ID use it: names are not unique in the store, so a name-addressed operation
// can only ever mean the oldest holder.
type nodeTarget struct {
	id   int64
	name string
}

func (t nodeTarget) label() string {
	if t.id > 0 {
		return fmt.Sprintf("#%d", t.id)
	}
	return t.name
}

// nodeTargets resolves a batch body's names and ids, writing a 400 when neither
// is supplied. IDs win: when both are present the caller's selection is the ID
// list.
func nodeTargets(w http.ResponseWriter, names []string, ids []int64) ([]nodeTarget, bool) {
	if len(ids) > 0 {
		targets := make([]nodeTarget, 0, len(ids))
		for _, id := range ids {
			if id <= 0 {
				writeAPIError(w, http.StatusBadRequest, "节点 ID 必须是正整数")
				return nil, false
			}
			targets = append(targets, nodeTarget{id: id})
		}
		return targets, true
	}
	if len(names) == 0 {
		writeAPIError(w, http.StatusBadRequest, "节点列表为空")
		return nil, false
	}
	targets := make([]nodeTarget, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			targets = append(targets, nodeTarget{name: name})
		}
	}
	if len(targets) == 0 {
		writeAPIError(w, http.StatusBadRequest, "节点列表为空")
		return nil, false
	}
	return targets, true
}

func (s *Server) updateNodeTarget(ctx context.Context, target nodeTarget, node config.NodeConfig) (config.NodeConfig, error) {
	if target.id > 0 {
		if byID, ok := s.nodeMgr.(controlplane.IDNodeManager); ok {
			return byID.UpdateNodeByID(ctx, target.id, node)
		}
	}
	if target.id > 0 && target.name == "" {
		return config.NodeConfig{}, controlplane.ErrNodeNotFound
	}
	return s.nodeMgr.UpdateNode(ctx, target.name, node)
}

func (s *Server) setNodeEnabledTarget(ctx context.Context, target nodeTarget, enabled bool) error {
	if target.id > 0 {
		if byID, ok := s.nodeMgr.(controlplane.IDNodeManager); ok {
			return byID.SetNodeEnabledByID(ctx, target.id, enabled)
		}
	}
	if target.id > 0 && target.name == "" {
		return controlplane.ErrNodeNotFound
	}
	return s.nodeMgr.SetNodeEnabled(ctx, target.name, enabled)
}

func (s *Server) deleteNodeTarget(ctx context.Context, target nodeTarget) error {
	if target.id > 0 {
		if byID, ok := s.nodeMgr.(controlplane.IDNodeManager); ok {
			return byID.DeleteNodeByID(ctx, target.id)
		}
	}
	if target.id > 0 && target.name == "" {
		return controlplane.ErrNodeNotFound
	}
	return s.nodeMgr.DeleteNode(ctx, target.name)
}

func configNodeID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return pathID(w, r, "id", "无效的节点 ID")
}

// handleConfigNodeUpdateByID is the ID-addressed twin of handleConfigNodeUpdate.
func (s *Server) handleConfigNodeUpdateByID(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	id, ok := configNodeID(w, r)
	if !ok {
		return
	}
	var payload nodePayload
	if err := decodeJSON(r, &payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	node, err := s.updateNodeTarget(r.Context(), nodeTarget{id: id}, payload.toConfig())
	if err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	reloadError := s.reloadAfterGroupMutation(r.Context())
	writeJSON(w, map[string]any{"node": node, "message": "节点已更新", "reloaded": reloadError == "", "reload_error": reloadError})
}

// handleConfigNodeToggleByID is the ID-addressed twin of handleConfigNodeToggle.
func (s *Server) handleConfigNodeToggleByID(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	id, ok := configNodeID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.Enabled == nil {
		writeAPIError(w, http.StatusBadRequest, "缺少 enabled 字段")
		return
	}
	if err := s.setNodeEnabledTarget(r.Context(), nodeTarget{id: id}, *body.Enabled); err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	state := "已启用"
	if !*body.Enabled {
		state = "已禁用"
	}
	reloadMsg := "（已自动重载）"
	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.logger.Printf("auto-reload after toggle failed: %v", err)
		reloadMsg = "（自动重载失败，请手动重载）"
	}
	writeJSON(w, map[string]any{"message": fmt.Sprintf("节点 #%d %s%s", id, state, reloadMsg)})
}

// handleConfigNodeDeleteByID is the ID-addressed twin of handleConfigNodeDelete.
func (s *Server) handleConfigNodeDeleteByID(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	id, ok := configNodeID(w, r)
	if !ok {
		return
	}
	if err := s.deleteNodeTarget(r.Context(), nodeTarget{id: id}); err != nil {
		s.respondNodeError(w, err)
		return
	}
	s.enqueueRetagAll()
	reloadError := s.reloadAfterGroupMutation(r.Context())
	writeJSON(w, map[string]any{"message": "节点已删除", "reloaded": reloadError == "", "reload_error": reloadError})
}

// batchCreateRequest is the body of POST /api/nodes/config/batch.
type batchCreateRequest struct {
	Nodes  []nodePayload `json:"nodes"`
	DryRun bool          `json:"dry_run"`
}

// handleConfigNodesBatchCreate creates several manual nodes in one call. It is
// the JSON sibling of the import endpoint: same batch path, same build dry run,
// same per-row reporting.
func (s *Server) handleConfigNodesBatchCreate(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}
	var req batchCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(req.Nodes) == 0 {
		writeAPIError(w, http.StatusBadRequest, "节点列表为空")
		return
	}
	nodes := make([]config.NodeConfig, 0, len(req.Nodes))
	for _, payload := range req.Nodes {
		nodes = append(nodes, payload.toConfig())
	}

	if batch, ok := s.nodeMgr.(controlplane.BatchNodeCreator); ok {
		result, err := batch.CreateNodes(r.Context(), nodes, req.DryRun)
		if err != nil {
			s.respondNodeError(w, err)
			return
		}
		if len(result.Created) > 0 && !req.DryRun {
			s.enqueueRetagAll()
		}
		// A dry run neither writes nor reloads, so it must not claim to have.
		response := map[string]any{
			"message":   batchCreateMessage(result, req.DryRun),
			"planned":   result.Planned,
			"failed":    result.Failed,
			"created":   result.Created,
			"dry_run":   req.DryRun,
			"reloaded":  false,
			"imported":  len(result.Created),
			"requested": len(nodes),
		}
		if req.DryRun {
			response["would_create"] = len(result.Planned)
		}
		if len(result.Created) > 0 && !req.DryRun {
			reloadError := s.reloadAfterGroupMutation(r.Context())
			response["reloaded"] = reloadError == ""
			if reloadError != "" {
				response["reload_error"] = reloadError
			}
		}
		writeJSON(w, response)
		return
	}

	// Fallback for runtimes without the batch capability: per-row creates, with
	// the same response shape. There is no build dry run here, so a dry run
	// reports the requested rows as planned and writes nothing.
	if req.DryRun {
		writeJSON(w, map[string]any{
			"message":   fmt.Sprintf("预览：将创建 %d 个节点", len(nodes)),
			"planned":   nodes,
			"failed":    []controlplane.NodeCreateFailure{},
			"created":   []config.NodeConfig{},
			"dry_run":   true,
			"reloaded":  false,
			"imported":  0,
			"requested": len(nodes),
		})
		return
	}
	var created []config.NodeConfig
	var failures []controlplane.NodeCreateFailure
	for index, node := range nodes {
		createdNode, err := s.nodeMgr.CreateNode(r.Context(), node)
		if err != nil {
			failures = append(failures, controlplane.NodeCreateFailure{Index: index, Name: node.Name, URI: node.URI, Reason: err.Error(), Err: err})
			continue
		}
		created = append(created, createdNode)
	}
	response := map[string]any{
		"message":   fmt.Sprintf("已创建 %d 个节点，失败 %d 个", len(created), len(failures)),
		"failed":    failures,
		"created":   created,
		"dry_run":   false,
		"reloaded":  true,
		"imported":  len(created),
		"requested": len(nodes),
	}
	if len(created) > 0 {
		s.enqueueRetagAll()
		reloadError := s.reloadAfterGroupMutation(r.Context())
		response["reloaded"] = reloadError == ""
		if reloadError != "" {
			response["reload_error"] = reloadError
		}
	}
	writeJSON(w, response)
}

func batchCreateMessage(result controlplane.NodeBatchResult, dryRun bool) string {
	if dryRun {
		return fmt.Sprintf("预览：将创建 %d 个节点，失败 %d 个", len(result.Planned), len(result.Failed))
	}
	return fmt.Sprintf("已创建 %d 个节点，失败 %d 个", len(result.Created), len(result.Failed))
}

// handleReload triggers a configuration reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if !s.ensureNodeManager(w) {
		return
	}

	if err := s.nodeMgr.TriggerReload(r.Context()); err != nil {
		s.respondNodeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"message": "重载成功，现有连接已被中断",
	})
}

func (s *Server) ensureNodeManager(w http.ResponseWriter) bool {
	if s.nodeMgr == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "节点管理未启用"})
		return false
	}
	return true
}

func (s *Server) respondNodeError(w http.ResponseWriter, err error) {
	// The status policy lives in nodeErrorStatus; this wrapper only adds the
	// fields a duplicate-node conflict has to carry back to the client.
	status := nodeErrorStatus(err)
	payload := map[string]any{"error": err.Error()}
	var duplicate *controlplane.NodeDuplicateError
	if errors.As(err, &duplicate) {
		payload["existing_node_id"] = duplicate.ExistingID
		payload["existing_node_name"] = duplicate.ExistingName
	}
	w.WriteHeader(status)
	writeJSON(w, payload)
}
