package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"easy_proxies/internal/config"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
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
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
		Enabled bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	if len(body.Names) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点列表为空"})
		return
	}

	var errs []string
	successCount := 0
	for _, name := range body.Names {
		if err := s.nodeMgr.SetNodeEnabled(r.Context(), name, body.Enabled); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
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
		"total":   len(body.Names),
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}
	if len(body.Names) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "节点列表为空"})
		return
	}

	var errs []string
	successCount := 0
	for _, name := range body.Names {
		if err := s.nodeMgr.DeleteNode(r.Context(), name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
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
		"total":   len(body.Names),
	}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	writeJSON(w, result)
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
	var duplicate *monitor.NodeDuplicateError
	if errors.As(err, &duplicate) {
		payload["existing_node_id"] = duplicate.ExistingID
		payload["existing_node_name"] = duplicate.ExistingName
	}
	w.WriteHeader(status)
	writeJSON(w, payload)
}
