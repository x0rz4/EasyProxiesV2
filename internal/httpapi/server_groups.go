package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/noderegion"
	"easy_proxies/internal/store"
)

type groupPoolInput struct {
	MinHealthy           *int      `json:"min_healthy"`
	TargetSize           *int      `json:"target_size"`
	HealingEnabled       *bool     `json:"healing_enabled"`
	Refill               *[]string `json:"refill"`
	Name                 string    `json:"name"`
	BindAddress          string    `json:"bind_address"`
	BindPort             uint16    `json:"bind_port"`
	Protocol             string    `json:"protocol"`
	Username             string    `json:"username"`
	Password             string    `json:"password"`
	DispatchMode         string    `json:"dispatch_mode"`
	Regions              []string  `json:"regions"`
	RegionRuleVersion    *int      `json:"region_rule_version"`
	IncludeUnknownRegion *bool     `json:"include_unknown_region"`
	ExcludeRegions       *[]string `json:"exclude_regions"`
	ExplicitNodeIDs      []int64   `json:"explicit_node_ids"`
	ExcludedNodeIDs      *[]int64  `json:"excluded_node_ids"`
	// TagWhitelist and TagBlacklist are pointers so that omitting them from a
	// partial update leaves the stored filter alone, matching ExcludedNodeIDs.
	TagWhitelist         *[]int64 `json:"tag_whitelist"`
	TagBlacklist         *[]int64 `json:"tag_blacklist"`
	TagFilterMatch       string   `json:"tag_filter_match"`
	FailureWindowSeconds int      `json:"failure_window_seconds"`
	FailureThreshold     int      `json:"failure_threshold"`
	HealthCheckSeconds   int      `json:"health_check_seconds"`
	// Automatic recovery of evicted members. Pointers keep a partial update
	// from silently resetting stored values, matching Enabled.
	RecoveryEnabled          *bool  `json:"recovery_enabled"`
	RecoverySuccessThreshold int    `json:"recovery_success_threshold"`
	RecoveryCooldownSeconds  *int   `json:"recovery_cooldown_seconds"`
	Enabled                  *bool  `json:"enabled"`
	SubscriptionEnabled      *bool  `json:"subscription_enabled"`
	SubscriptionMode         string `json:"subscription_mode"`
	ExternalHost             string `json:"external_host"`
}

type groupMemberResponse struct {
	Standby      bool      `json:"standby"`
	NodeID       int64     `json:"node_id"`
	Tag          string    `json:"tag"`
	Name         string    `json:"name"`
	Region       string    `json:"region,omitempty"`
	Country      string    `json:"country,omitempty"`
	Status       string    `json:"status"`
	FailureCount int       `json:"failure_count"`
	LastError    string    `json:"last_error,omitempty"`
	EvictedAt    time.Time `json:"evicted_at,omitempty"`
	// Recovery progress of an evicted member that has automatic recovery on.
	RecoverySuccesses   int       `json:"recovery_successes,omitempty"`
	RecoveryThreshold   int       `json:"recovery_threshold,omitempty"`
	RecoveryCooldownEnd time.Time `json:"recovery_cooldown_end,omitempty"`
	LatencyMs           int64     `json:"latency_ms"`
	Available           bool      `json:"available"`
	Provisional         bool      `json:"provisional"`
	HealthSource        string    `json:"health_source"`
	IsActive            bool      `json:"is_active"`
}

type groupNodeOptionResponse struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	URI              string `json:"uri"`
	Region           string `json:"region,omitempty"`
	Country          string `json:"country,omitempty"`
	Enabled          bool   `json:"enabled"`
	Tag              string `json:"tag,omitempty"`
	Status           string `json:"status"`
	LatencyMs        int64  `json:"latency_ms"`
	Available        bool   `json:"available"`
	InitialCheckDone bool   `json:"initial_check_done"`
	Selectable       bool   `json:"selectable"`
	Provisional      bool   `json:"provisional"`
	HealthSource     string `json:"health_source"`
}

type groupPoolResponse struct {
	store.GroupPool
	StandbyCount     int                   `json:"standby_count"`
	RuntimeStatus    string                `json:"runtime_status"`
	RuntimeError     string                `json:"runtime_error,omitempty"`
	CurrentActiveTag string                `json:"current_active_tag,omitempty"`
	Members          []groupMemberResponse `json:"members"`
	MemberCount      int                   `json:"member_count"`
	AliveCount       int                   `json:"alive_count"`
	EvictedCount     int                   `json:"evicted_count"`
}

// groupRouteID resolves the {groupID} path segment, answering 400 itself. Every
// /api/groups/{groupID} handler starts here.
func (s *Server) groupRouteID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	return pathID(w, r, "groupID", "无效的分组 ID")
}

// lockGroupMutation serialises the store-then-apply sequence for one group.
// Every handler that mutates a group's persisted row must hold this lock while
// it reads, writes and applies the runtime change, so two concurrent edits
// cannot interleave their read/modify/write.
func (s *Server) lockGroupMutation(groupID int64) func() {
	operationLock := s.groupOperationLock(groupID)
	operationLock.Lock()
	return operationLock.Unlock
}

// requireGroupStore applies the store guard shared by every group route.
func (s *Server) requireGroupStore(w http.ResponseWriter) bool {
	return s.requireStore(w, storeUnavailableGeneric)
}

func (s *Server) handleGroupsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	s.writeGroupList(w, r)
}

func (s *Server) handleGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	var input groupPoolInput
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "无效的请求数据")
		return
	}
	s.groupMutationMu.Lock()
	defer s.groupMutationMu.Unlock()
	group, removedNodeIDs, err := s.groupFromInput(r.Context(), input, nil)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.CreateGroupPool(r.Context(), group); err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	reloadError := s.applyGroupRuntimeMutation(r.Context(), nil, group)
	if reloadError != "" {
		rolledBack := s.store.DeleteGroupPool(r.Context(), group.ID) == nil
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"group": group, "reloaded": true, "reload_error": "",
		"removed_unavailable_node_ids": removedNodeIDs})
}

func (s *Server) handleGroupActivate(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	nodeID, ok := pathID(w, r, "nodeID", "无效的节点 ID")
	if !ok {
		return
	}
	groupPool, lookupErr := s.store.GetGroupPool(r.Context(), groupID)
	if lookupErr != nil || groupPool == nil {
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	if !groupPool.Enabled {
		writeAPIError(w, http.StatusConflict, "分组未启用")
		return
	}
	activateErr := error(nil)
	if runtimeManager, ok := s.nodeMgr.(controlplane.GroupRuntimeManager); ok {
		activateErr = runtimeManager.ActivateGroupMember(r.Context(), groupID, nodeID)
	} else {
		activateErr = group.ActivateMember(groupID, nodeID)
	}
	if activateErr != nil {
		writeAPIError(w, http.StatusConflict, activateErr.Error())
		return
	}
	writeJSON(w, map[string]any{"message": "当前出口已切换", "group_id": groupID, "node_id": nodeID})
}

func (s *Server) handleGroupRestore(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	nodeID, ok := pathID(w, r, "nodeID", "无效的节点 ID")
	if !ok {
		return
	}
	if err := s.store.ClearGroupNodeState(r.Context(), groupID, nodeID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := group.RestoreGroupMember(groupID, nodeID); err != nil {
		// A disabled/invalid node may not currently have a runtime; the cleared
		// persisted state still guarantees restoration on the next reload.
		s.logger.Printf("restore group runtime: %v", err)
	}
	writeJSON(w, map[string]any{"message": "节点已恢复", "group_id": groupID, "node_id": nodeID})
}

func (s *Server) handleGroupRemoveMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	unlock := s.lockGroupMutation(groupID)
	defer unlock()
	nodeID, ok := pathID(w, r, "nodeID", "无效的节点 ID")
	if !ok {
		return
	}
	runtimeState, ok := group.GroupRuntimeSnapshots()[groupID]
	if !ok {
		writeAPIError(w, http.StatusConflict, "分组当前未运行")
		return
	}
	isMember := false
	for _, member := range runtimeState.Members {
		if member.NodeID == nodeID {
			isMember = true
			break
		}
	}
	if !isMember {
		writeAPIError(w, http.StatusNotFound, "节点不是当前运行成员")
		return
	}
	s.groupMutationMu.Lock()
	groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil || groupPool == nil {
		s.groupMutationMu.Unlock()
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	originalGroup := cloneGroupPool(groupPool)
	groupPool.ExplicitNodeIDs = removeInt64(groupPool.ExplicitNodeIDs, nodeID)
	if !containsInt64(groupPool.ExcludedNodeIDs, nodeID) {
		groupPool.ExcludedNodeIDs = append(groupPool.ExcludedNodeIDs, nodeID)
	}
	if groupPool.CurrentActiveNodeID == nodeID || runtimeState.CurrentNodeID == nodeID {
		groupPool.CurrentActiveNodeID = 0
	}
	err = s.store.UpdateGroupPool(r.Context(), groupPool)
	s.groupMutationMu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	reloadError := s.applyGroupRuntimeMutation(r.Context(), originalGroup, groupPool)
	if reloadError != "" {
		rolledBack := s.store.UpdateGroupPool(r.Context(), originalGroup) == nil
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
		return
	}
	writeJSON(w, map[string]any{"message": "节点已从当前分组移除", "group_id": groupID, "node_id": nodeID,
		"reloaded": true, "reload_error": ""})
}

func (s *Server) handleGroupRemoveExclusion(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	unlock := s.lockGroupMutation(groupID)
	defer unlock()
	nodeID, ok := pathID(w, r, "nodeID", "无效的节点 ID")
	if !ok {
		return
	}
	s.groupMutationMu.Lock()
	groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil || groupPool == nil {
		s.groupMutationMu.Unlock()
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	if !containsInt64(groupPool.ExcludedNodeIDs, nodeID) {
		s.groupMutationMu.Unlock()
		writeJSON(w, map[string]any{"message": "节点未处于排除名单", "group_id": groupID, "node_id": nodeID,
			"reloaded": true, "reload_error": ""})
		return
	}
	originalGroup := cloneGroupPool(groupPool)
	groupPool.ExcludedNodeIDs = removeInt64(groupPool.ExcludedNodeIDs, nodeID)
	err = s.store.UpdateGroupPool(r.Context(), groupPool)
	s.groupMutationMu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	reloadError := s.applyGroupRuntimeMutation(r.Context(), originalGroup, groupPool)
	if reloadError != "" {
		rolledBack := s.store.UpdateGroupPool(r.Context(), originalGroup) == nil
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
		return
	}
	writeJSON(w, map[string]any{"message": "节点已取消排除", "group_id": groupID, "node_id": nodeID,
		"reloaded": true, "reload_error": ""})
}

func (s *Server) handleGroupResetToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	groupPool, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil || groupPool == nil {
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	token, err := controlplane.GenerateToken()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "生成订阅 Token 失败")
		return
	}
	groupPool.SubscriptionToken = token
	if err := s.store.UpdateGroupPool(r.Context(), groupPool); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"message": "订阅 Token 已重置", "token": token})
}

// groupForUpdate resolves the group a mutation applies to, answering 500/404
// itself. The read happens under the caller's per-group operation lock.
func (s *Server) groupForUpdate(w http.ResponseWriter, r *http.Request, groupID int64) (*store.GroupPool, bool) {
	existing, err := s.store.GetGroupPool(r.Context(), groupID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if existing == nil {
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return nil, false
	}
	return existing, true
}

func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	unlock := s.lockGroupMutation(groupID)
	defer unlock()
	existing, ok := s.groupForUpdate(w, r, groupID)
	if !ok {
		return
	}
	var input groupPoolInput
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "无效的请求数据")
		return
	}
	updated, removedNodeIDs, err := s.groupFromInput(r.Context(), input, existing)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateGroupPool(r.Context(), updated); err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	reloadError := s.applyGroupRuntimeMutation(r.Context(), existing, updated)
	if reloadError != "" {
		rolledBack := s.store.UpdateGroupPool(r.Context(), existing) == nil
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": rolledBack})
		return
	}
	writeJSON(w, map[string]any{"group": updated, "reloaded": reloadError == "", "reload_error": reloadError,
		"removed_unavailable_node_ids": removedNodeIDs})
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireGroupStore(w) {
		return
	}
	groupID, ok := s.groupRouteID(w, r)
	if !ok {
		return
	}
	unlock := s.lockGroupMutation(groupID)
	defer unlock()
	existing, ok := s.groupForUpdate(w, r, groupID)
	if !ok {
		return
	}
	if reloadError := s.applyGroupRuntimeMutation(r.Context(), existing, nil); reloadError != "" {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": reloadError, "reloaded": false, "reload_error": reloadError, "rolled_back": true})
		return
	}
	if err := s.store.DeleteGroupPool(r.Context(), groupID); err != nil {
		_ = s.applyGroupRuntimeMutation(r.Context(), nil, existing)
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"message": "分组已删除", "reloaded": true, "reload_error": ""})
}

func (s *Server) writeGroupList(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListGroupPools(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	nodes, _ := s.store.ListNodes(r.Context(), store.NodeFilter{})
	nodeByID := make(map[int64]store.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	monitorByTag := make(map[string]monitor.Snapshot)
	monitorByNodeID := make(map[int64]monitor.Snapshot)
	if s.mgr != nil {
		for _, snapshot := range s.mgr.Snapshot() {
			monitorByTag[snapshot.Tag] = snapshot
			if snapshot.NodeID != 0 {
				monitorByNodeID[snapshot.NodeID] = snapshot
			}
		}
	}
	runtimes := group.GroupRuntimeSnapshots()
	nodeOptions, err := s.groupNodeOptions(r.Context(), groups, monitorByTag)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	responses := make([]groupPoolResponse, 0, len(groups))
	for _, group := range groups {
		response := groupPoolResponse{GroupPool: group, Members: []groupMemberResponse{}}
		if runtimeManager, ok := s.nodeMgr.(controlplane.GroupRuntimeManager); ok {
			runtimeStatus := runtimeManager.GroupRuntimeStatus(group.ID)
			response.RuntimeStatus, response.RuntimeError = runtimeStatus.Status, runtimeStatus.Error
		} else if !group.Enabled {
			response.RuntimeStatus = "stopped"
		} else {
			response.RuntimeStatus = "starting"
		}
		if runtimeState, ok := runtimes[group.ID]; ok {
			if response.RuntimeStatus == "starting" {
				response.RuntimeStatus = "ready"
			}
			response.CurrentActiveTag = runtimeState.CurrentTag
			for _, member := range runtimeState.Members {
				node := nodeByID[member.NodeID]
				mon := monitorByNodeID[member.NodeID]
				if mon.NodeID == 0 {
					mon = monitorByTag[member.Tag]
				}
				status := member.Status
				if status == "ALIVE" && mon.InitialCheckDone && !mon.Available {
					status = "SUSPECT"
				}
				item := groupMemberResponse{NodeID: member.NodeID, Tag: member.Tag, Name: node.Name,
					Standby: member.Standby,
					Region:  mon.Region, Country: mon.Country, Status: status, FailureCount: member.FailureCount,
					LastError: member.LastError, EvictedAt: member.EvictedAt, LatencyMs: mon.LastLatencyMs,
					Available: mon.RoutingEligible, Provisional: mon.Provisional, HealthSource: mon.HealthSource,
					IsActive:          member.NodeID == runtimeState.CurrentNodeID,
					RecoverySuccesses: member.RecoverySuccesses, RecoveryThreshold: member.RecoveryThreshold,
					RecoveryCooldownEnd: member.RecoveryCooldownEnd}
				if item.Region == "" {
					item.Region = node.Region
				}
				if item.Country == "" {
					item.Country = node.Country
				}
				response.Members = append(response.Members, item)
				if member.Standby {
					response.StandbyCount++
				}
				if status == "EVICTED" {
					response.EvictedCount++
				} else if status == "ALIVE" && !member.Standby {
					response.AliveCount++
				}
			}
		}
		response.MemberCount = len(response.Members)
		responses = append(responses, response)
	}
	writeJSON(w, map[string]any{"groups": responses, "nodes": nodeOptions, "port_range": map[string]int{"start": 10000, "end": 19999}})
}

func (s *Server) groupNodeOptions(ctx context.Context, groups []store.GroupPool, monitorByTag map[string]monitor.Snapshot) ([]groupNodeOptionResponse, error) {
	managedNodes, err := s.store.ListManagedNodes(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("读取节点管理列表: %w", err)
	}
	referenced := make(map[int64]struct{})
	for _, groupPool := range groups {
		for _, nodeID := range groupPool.ExplicitNodeIDs {
			referenced[nodeID] = struct{}{}
		}
		for _, nodeID := range groupPool.ExcludedNodeIDs {
			referenced[nodeID] = struct{}{}
		}
	}
	monitorByURI := make(map[string]monitor.Snapshot, len(monitorByTag))
	monitorByIdentity := make(map[string]monitor.Snapshot, len(monitorByTag))
	monitorByName := make(map[string]monitor.Snapshot, len(monitorByTag))
	for _, snapshot := range monitorByTag {
		if snapshot.URI != "" {
			monitorByURI[snapshot.URI] = snapshot
			if identity, identityErr := nodecodec.ParseURI(snapshot.URI); identityErr == nil {
				monitorByIdentity[identity.Hash] = snapshot
			}
		}
		if snapshot.Name != "" {
			monitorByName[snapshot.Name] = snapshot
		}
	}
	options := make([]groupNodeOptionResponse, 0, len(managedNodes))
	for _, managed := range managedNodes {
		snapshot, found := monitorByURI[managed.URI]
		if !found && managed.IdentityHash != "" {
			snapshot, found = monitorByIdentity[managed.IdentityHash]
		}
		if !found {
			snapshot, found = monitorByName[managed.Name]
		}
		status := "pending"
		if !managed.Enabled {
			status = "disabled"
		} else if found && snapshot.Blacklisted {
			status = "blacklisted"
		} else if found && snapshot.InitialCheckDone && snapshot.Available {
			status = "normal"
		} else if found && snapshot.InitialCheckDone {
			status = "unavailable"
		}
		// Configuration membership survives temporary health failures. Runtime
		// availability controls routing, not whether a managed node can be chosen.
		selectable := managed.Enabled
		if _, isReferenced := referenced[managed.ID]; !selectable && !isReferenced {
			continue
		}
		latencyMs := int64(-1)
		if found {
			latencyMs = snapshot.LastLatencyMs
		}
		region, country := managed.Region, managed.Country
		if found && snapshot.Region != "" {
			region = snapshot.Region
		}
		if found && snapshot.Country != "" {
			country = snapshot.Country
		}
		options = append(options, groupNodeOptionResponse{ID: managed.ID, Name: managed.Name, URI: managed.URI,
			Region: region, Country: country, Enabled: managed.Enabled, Tag: snapshot.Tag, Status: status,
			LatencyMs: latencyMs, Available: found && snapshot.RoutingEligible,
			InitialCheckDone: found && snapshot.InitialCheckDone, Selectable: selectable,
			Provisional: found && snapshot.Provisional, HealthSource: snapshot.HealthSource})
	}
	return options, nil
}

func (s *Server) selectableGroupNodeIDs(ctx context.Context) (map[int64]struct{}, error) {
	monitorByTag := make(map[string]monitor.Snapshot)
	if s.mgr != nil {
		for _, snapshot := range s.mgr.Snapshot() {
			monitorByTag[snapshot.Tag] = snapshot
		}
	}
	options, err := s.groupNodeOptions(ctx, nil, monitorByTag)
	if err != nil {
		return nil, err
	}
	selectable := make(map[int64]struct{}, len(options))
	for _, option := range options {
		if option.Selectable {
			selectable[option.ID] = struct{}{}
		}
	}
	return selectable, nil
}

func (s *Server) groupFromInput(ctx context.Context, input groupPoolInput, existing *store.GroupPool) (*store.GroupPool, []int64, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return nil, nil, errors.New("分组名称不能为空")
	}
	if input.BindAddress == "" {
		input.BindAddress = "0.0.0.0"
	}
	if net.ParseIP(input.BindAddress) == nil && input.BindAddress != "localhost" {
		return nil, nil, errors.New("监听地址无效")
	}
	if input.Protocol == "" {
		input.Protocol = config.InboundProtocolMixed
	}
	protocol, err := config.NormalizeInboundProtocol(input.Protocol)
	if err != nil {
		return nil, nil, err
	}
	switch strings.ToLower(strings.TrimSpace(input.DispatchMode)) {
	case "random":
		input.DispatchMode = "random"
	case "lowest_latency":
		input.DispatchMode = "lowest_latency"
	default:
		input.DispatchMode = "fixed"
	}
	if input.FailureWindowSeconds <= 0 {
		input.FailureWindowSeconds = 300
	}
	if input.FailureThreshold <= 0 {
		input.FailureThreshold = 3
	}
	if input.HealthCheckSeconds <= 0 {
		input.HealthCheckSeconds = 60
	}
	recoveryEnabled := true
	if input.RecoveryEnabled != nil {
		recoveryEnabled = *input.RecoveryEnabled
	} else if existing != nil {
		recoveryEnabled = existing.RecoveryEnabled
	}
	recoveryThreshold := input.RecoverySuccessThreshold
	if recoveryThreshold <= 0 && existing != nil {
		recoveryThreshold = existing.RecoverySuccessThreshold
	}
	if recoveryThreshold <= 0 {
		recoveryThreshold = 3
	}
	recoveryCooldown := 300
	if input.RecoveryCooldownSeconds != nil {
		recoveryCooldown = *input.RecoveryCooldownSeconds
	} else if existing != nil {
		recoveryCooldown = existing.RecoveryCooldownSeconds
	}
	if recoveryCooldown < 0 {
		recoveryCooldown = 0
	}
	autoPort := input.BindPort == 0
	if autoPort {
		input.BindPort, err = s.allocateGroupPort(ctx)
		if err != nil {
			return nil, nil, err
		}
	} else if err := s.validateGroupPort(ctx, input.BindAddress, input.BindPort, existing); err != nil {
		return nil, nil, err
	}
	managedNodes, err := s.store.ListManagedNodes(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	knownNodeIDs := make(map[int64]struct{}, len(managedNodes))
	for _, node := range managedNodes {
		knownNodeIDs[node.ID] = struct{}{}
	}
	keptNodeIDs := make([]int64, 0, len(input.ExplicitNodeIDs))
	removedNodeIDs := make([]int64, 0)
	seenNodeIDs := make(map[int64]struct{}, len(input.ExplicitNodeIDs))
	for _, nodeID := range input.ExplicitNodeIDs {
		if nodeID <= 0 {
			continue
		}
		if _, duplicate := seenNodeIDs[nodeID]; duplicate {
			continue
		}
		seenNodeIDs[nodeID] = struct{}{}
		if _, ok := knownNodeIDs[nodeID]; !ok {
			removedNodeIDs = append(removedNodeIDs, nodeID)
			continue
		}
		keptNodeIDs = append(keptNodeIDs, nodeID)
	}
	excludedInput := []int64(nil)
	if input.ExcludedNodeIDs != nil {
		excludedInput = *input.ExcludedNodeIDs
	} else if existing != nil {
		excludedInput = existing.ExcludedNodeIDs
	}
	excludedNodeIDs := make([]int64, 0, len(excludedInput))
	seenExcluded := make(map[int64]struct{}, len(excludedInput))
	for _, nodeID := range excludedInput {
		if nodeID <= 0 {
			continue
		}
		if _, duplicate := seenExcluded[nodeID]; duplicate {
			continue
		}
		seenExcluded[nodeID] = struct{}{}
		excludedNodeIDs = append(excludedNodeIDs, nodeID)
	}
	regionVersion := noderegion.CurrentVersion
	if existing != nil {
		regionVersion = existing.RegionRuleVersion
		if regionVersion == 0 {
			regionVersion = 1
		}
	}
	if input.RegionRuleVersion != nil {
		regionVersion = *input.RegionRuleVersion
	}
	if regionVersion != 1 && regionVersion != noderegion.CurrentVersion {
		return nil, nil, errors.New("不支持的地区规则版本")
	}
	includeUnknown := false
	if existing != nil {
		includeUnknown = existing.IncludeUnknownRegion
	}
	if input.IncludeUnknownRegion != nil {
		includeUnknown = *input.IncludeUnknownRegion
	}
	excludeRegions := []string(nil)
	if existing != nil {
		excludeRegions = append(excludeRegions, existing.ExcludeRegions...)
	}
	if input.ExcludeRegions != nil {
		excludeRegions = append([]string(nil), (*input.ExcludeRegions)...)
	}
	for i, value := range excludeRegions {
		if strings.EqualFold(strings.TrimSpace(value), noderegion.Other) {
			excludeRegions[i] = noderegion.Other
			continue
		}
		excludeRegions[i] = noderegion.Normalize(value)
		if excludeRegions[i] == "" {
			return nil, nil, fmt.Errorf("无效的排除地区: %s", value)
		}
	}
	regions := make([]string, 0, len(input.Regions))
	seenRegions := make(map[string]struct{})
	for _, value := range input.Regions {
		region := strings.ToLower(strings.TrimSpace(value))
		if region == "" {
			continue
		}
		if regionVersion >= noderegion.CurrentVersion && region != noderegion.Other {
			region = noderegion.Normalize(region)
			if region == "" {
				return nil, nil, fmt.Errorf("无效的地区: %s", value)
			}
		}
		if _, ok := seenRegions[region]; !ok {
			seenRegions[region] = struct{}{}
			regions = append(regions, region)
		}
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	} else if existing != nil {
		enabled = existing.Enabled
	}
	tagWhitelist, tagBlacklist, tagFilterMatch, err := s.groupTagFilterFromInput(ctx, input, existing)
	if err != nil {
		return nil, nil, err
	}
	subscriptionEnabled := true
	if input.SubscriptionEnabled != nil {
		subscriptionEnabled = *input.SubscriptionEnabled
	} else if existing != nil {
		subscriptionEnabled = existing.SubscriptionEnabled
	}
	if input.SubscriptionMode != "members" {
		input.SubscriptionMode = "entry"
	}
	subscriptionToken := ""
	if existing != nil {
		subscriptionToken = existing.SubscriptionToken
	}
	if subscriptionToken == "" {
		subscriptionToken, err = controlplane.GenerateToken()
		if err != nil {
			return nil, nil, fmt.Errorf("生成订阅 Token: %w", err)
		}
	}
	healPolicy := group.HealPolicy{Refill: []string{group.PromoteStandby, group.ProbeQuarantined, group.RefreshSubscription}}
	if existing != nil {
		healPolicy = existing.HealPolicy.Clone()
	}
	if input.MinHealthy != nil {
		healPolicy.MinHealthy = *input.MinHealthy
	}
	if input.TargetSize != nil {
		healPolicy.TargetSize = *input.TargetSize
	}
	if input.HealingEnabled != nil {
		healPolicy.Execute = *input.HealingEnabled
	}
	if input.Refill != nil {
		healPolicy.Refill = append([]string(nil), (*input.Refill)...)
	}
	if err := healPolicy.Validate(); err != nil {
		return nil, nil, err
	}
	group := &store.GroupPool{Name: input.Name, BindAddress: input.BindAddress, BindPort: input.BindPort,
		HealPolicy: healPolicy,
		Protocol:   protocol, Username: input.Username, Password: input.Password, DispatchMode: input.DispatchMode,
		Regions: regions, ExplicitNodeIDs: keptNodeIDs, ExcludedNodeIDs: excludedNodeIDs, FailureWindowSeconds: input.FailureWindowSeconds,
		RegionRuleVersion: regionVersion, IncludeUnknownRegion: includeUnknown, ExcludeRegions: excludeRegions,
		TagWhitelist: tagWhitelist, TagBlacklist: tagBlacklist, TagFilterMatch: tagFilterMatch,
		FailureThreshold: input.FailureThreshold, HealthCheckSeconds: input.HealthCheckSeconds, Enabled: enabled,
		RecoveryEnabled: recoveryEnabled, RecoverySuccessThreshold: recoveryThreshold, RecoveryCooldownSeconds: recoveryCooldown,
		SubscriptionEnabled: subscriptionEnabled, SubscriptionToken: subscriptionToken,
		SubscriptionMode: input.SubscriptionMode, ExternalHost: strings.TrimSpace(input.ExternalHost)}
	if existing != nil {
		group.ID, group.CurrentActiveNodeID, group.CreatedAt, group.NodeStates = existing.ID, existing.CurrentActiveNodeID, existing.CreatedAt, existing.NodeStates
	}
	return group, removedNodeIDs, nil
}

// groupTagFilterFromInput normalizes the group's tag filter. Tag IDs that no
// longer exist are rejected rather than dropped: silently discarding a
// whitelist entry would widen the group, which is the opposite of what the
// operator asked for.
func (s *Server) groupTagFilterFromInput(ctx context.Context, input groupPoolInput,
	existing *store.GroupPool) ([]int64, []int64, string, error) {
	whitelistInput, blacklistInput := []int64(nil), []int64(nil)
	switch {
	case input.TagWhitelist != nil:
		whitelistInput = *input.TagWhitelist
	case existing != nil:
		whitelistInput = existing.TagWhitelist
	}
	switch {
	case input.TagBlacklist != nil:
		blacklistInput = *input.TagBlacklist
	case existing != nil:
		blacklistInput = existing.TagBlacklist
	}
	match := strings.ToLower(strings.TrimSpace(input.TagFilterMatch))
	if match == "" && existing != nil {
		match = existing.TagFilterMatch
	}
	switch match {
	case "":
		match = store.TagFilterMatchAny
	case store.TagFilterMatchAny, store.TagFilterMatchAll:
	default:
		return nil, nil, "", errors.New("标签匹配方式只能是 any 或 all")
	}

	whitelist := normalizeTagIDs(whitelistInput)
	blacklist := normalizeTagIDs(blacklistInput)
	inWhitelist := make(map[int64]struct{}, len(whitelist))
	for _, tagID := range whitelist {
		inWhitelist[tagID] = struct{}{}
	}
	for _, tagID := range blacklist {
		if _, conflict := inWhitelist[tagID]; conflict {
			return nil, nil, "", errors.New("标签白名单与黑名单不能包含同一标签")
		}
	}
	if len(whitelist) == 0 && len(blacklist) == 0 {
		return whitelist, blacklist, match, nil
	}
	if s.store == nil {
		return nil, nil, "", errors.New("标签筛选不可用: 数据存储未初始化")
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("读取标签列表: %w", err)
	}
	known := make(map[int64]struct{}, len(tags))
	for _, tag := range tags {
		known[tag.ID] = struct{}{}
	}
	for _, tagID := range append(append([]int64(nil), whitelist...), blacklist...) {
		if _, ok := known[tagID]; !ok {
			return nil, nil, "", fmt.Errorf("标签不存在: %d", tagID)
		}
	}
	return whitelist, blacklist, match, nil
}

// normalizeTagIDs drops non-positive IDs and duplicates while keeping the order
// the operator sent, which is the order the UI shows them back in.
func normalizeTagIDs(tagIDs []int64) []int64 {
	normalized := make([]int64, 0, len(tagIDs))
	seen := make(map[int64]struct{}, len(tagIDs))
	for _, tagID := range tagIDs {
		if tagID <= 0 {
			continue
		}
		if _, duplicate := seen[tagID]; duplicate {
			continue
		}
		seen[tagID] = struct{}{}
		normalized = append(normalized, tagID)
	}
	return normalized
}

func (s *Server) allocateGroupPort(ctx context.Context) (uint16, error) {
	groups, err := s.store.ListGroupPools(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[uint16]struct{}, len(groups)+2)
	for _, group := range groups {
		used[group.BindPort] = struct{}{}
	}
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg != nil {
		cfg.RLock()
		if cfg.Listener.Enabled {
			used[cfg.Listener.Port] = struct{}{}
		}
		if cfg.MultiPort.Enabled {
			for _, node := range cfg.Nodes {
				used[node.Port] = struct{}{}
			}
		}
		cfg.RUnlock()
	}
	for port := 10000; port <= 19999; port++ {
		candidate := uint16(port)
		if _, ok := used[candidate]; ok {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
		if err == nil {
			listener.Close()
			return candidate, nil
		}
	}
	return 0, errors.New("10000–19999 端口段没有可用端口")
}

func (s *Server) validateGroupPort(ctx context.Context, address string, port uint16, existing *store.GroupPool) error {
	groups, err := s.store.ListGroupPools(ctx)
	if err != nil {
		return err
	}
	for _, group := range groups {
		if existing != nil && group.ID == existing.ID {
			continue
		}
		if group.BindPort == port {
			return fmt.Errorf("端口 %d 已被分组“%s”使用", port, group.Name)
		}
	}
	s.cfgMu.RLock()
	cfg := s.cfgSrc
	s.cfgMu.RUnlock()
	if cfg != nil {
		cfg.RLock()
		if cfg.Listener.Enabled && cfg.Listener.Port == port {
			cfg.RUnlock()
			return fmt.Errorf("端口 %d 与全局代理入口冲突", port)
		}
		if cfg.MultiPort.Enabled {
			for _, node := range cfg.Nodes {
				if node.Port == port {
					cfg.RUnlock()
					return fmt.Errorf("端口 %d 与节点“%s”冲突", port, node.Name)
				}
			}
		}
		cfg.RUnlock()
	}
	if existing == nil || existing.BindPort != port || existing.BindAddress != address {
		listener, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(int(port))))
		if err != nil {
			return fmt.Errorf("端口 %d 当前不可用: %w", port, err)
		}
		listener.Close()
	}
	return nil
}

func (s *Server) reloadAfterGroupMutation(ctx context.Context) string {
	if s.nodeMgr == nil {
		return "重载管理器不可用"
	}
	if err := s.nodeMgr.TriggerReload(ctx); err != nil {
		return err.Error()
	}
	return ""
}

func (s *Server) applyGroupRuntimeMutation(ctx context.Context, before, after *store.GroupPool) string {
	if runtimeManager, ok := s.nodeMgr.(controlplane.GroupRuntimeManager); ok {
		if err := runtimeManager.ApplyGroupRuntime(ctx, before, after); err != nil {
			return err.Error()
		}
		return ""
	}
	return s.reloadAfterGroupMutation(ctx)
}

func cloneGroupPool(groupPool *store.GroupPool) *store.GroupPool {
	if groupPool == nil {
		return nil
	}
	cloned := *groupPool
	cloned.HealPolicy = groupPool.HealPolicy.Clone()
	cloned.Regions = append([]string(nil), groupPool.Regions...)
	cloned.ExcludeRegions = append([]string(nil), groupPool.ExcludeRegions...)
	cloned.ExplicitNodeIDs = append([]int64(nil), groupPool.ExplicitNodeIDs...)
	cloned.ExcludedNodeIDs = append([]int64(nil), groupPool.ExcludedNodeIDs...)
	cloned.TagWhitelist = append([]int64(nil), groupPool.TagWhitelist...)
	cloned.TagBlacklist = append([]int64(nil), groupPool.TagBlacklist...)
	cloned.NodeStates = append([]store.GroupNodeState(nil), groupPool.NodeStates...)
	return &cloned
}

func (s *Server) groupOperationLock(groupID int64) *sync.Mutex {
	lock, _ := s.groupOperationLocks.LoadOrStore(groupID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func removeInt64(values []int64, target int64) []int64 {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}
