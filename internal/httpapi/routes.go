package httpapi

import "net/http"

func handleSSEHead(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// routes builds the HTTP surface from method-aware Go 1.22+ ServeMux
// patterns. Keeping API and public-subscription routes in child muxes ensures
// an unknown endpoint is a real 404 rather than a React SPA fallback.
//
// Every entry names the one handler that serves it. The method is part of the
// pattern, so ServeMux owns 405 and the Allow header; a handler never has to
// re-dispatch on a second "action" argument, and the table below is the single
// place where a route's meaning is readable. routes_contract_test.go pins the
// full list.
func (s *Server) routes() http.Handler {
	root := http.NewServeMux()
	api := http.NewServeMux()
	publicSubscriptions := http.NewServeMux()
	static := http.NewServeMux()

	public := func(pattern string, handler http.HandlerFunc) {
		api.HandleFunc(pattern, handler)
	}
	authenticated := func(pattern string, handler http.HandlerFunc) {
		api.HandleFunc(pattern, s.withAuth(handler))
	}
	// publicAPI routes are unauthenticated by session but require a stored API
	// key (X-API-Key). They expose exactly two write operations.
	publicAPI := func(pattern string, handler http.HandlerFunc) {
		api.HandleFunc(pattern, s.withAPIKey(handler))
	}

	// —— 认证 ——
	public("GET /api/auth", s.handleAuthStatus)
	public("POST /api/auth", s.handleAuthLogin)

	// —— 设置 ——
	authenticated("GET /api/settings", s.handleSettingsGet)
	authenticated("PUT /api/settings", s.handleSettingsUpdate)
	authenticated("GET /api/operations/probe-settings", s.handleProbeSettingsGet)
	authenticated("PUT /api/operations/probe-settings", s.handleProbeSettingsUpdate)
	authenticated("GET /api/operations/probe-status", s.handleProbeStatus)
	authenticated("GET /api/operations/node-check-settings", s.handleNodeCheckSettingsGet)
	authenticated("PUT /api/operations/node-check-settings", s.handleNodeCheckSettingsUpdate)

	// —— 综合检测任务 ——
	authenticated("GET /api/node-check/results", s.handleNodeCheckResults)
	authenticated("GET /api/node-check/tasks", s.handleNodeCheckTasksList)
	authenticated("POST /api/node-check/tasks", s.handleNodeCheckTasksCreate)
	authenticated("GET /api/node-check/tasks/{taskID}", s.handleNodeCheckTaskGet)
	authenticated("DELETE /api/node-check/tasks/{taskID}", s.handleNodeCheckTaskCancel)
	authenticated("GET /api/node-check/tasks/{taskID}/events", s.handleNodeCheckTaskEvents)
	authenticated("HEAD /api/node-check/tasks/{taskID}/events", handleSSEHead)

	// —— 节点视图与筛选 ——
	authenticated("GET /api/nodes", s.handleNodes)
	authenticated("POST /api/nodes/query", s.handleNodeQuery)
	authenticated("POST /api/nodes/query-ids", s.handleNodeQuery)
	authenticated("GET /api/nodes/facets", s.handleNodeFacets)
	authenticated("POST /api/groups/preview", s.handleGroupPreview)
	authenticated("POST /api/groups/{groupID}/preview", s.handleGroupPreview)
	authenticated("GET /api/node-filters", s.handleSavedFiltersList)
	authenticated("POST /api/node-filters", s.handleSavedFiltersCreate)
	authenticated("PUT /api/node-filters/{filterID}", s.handleSavedFiltersUpdate)
	authenticated("DELETE /api/node-filters/{filterID}", s.handleSavedFiltersDelete)

	// —— 配置节点 CRUD ——
	authenticated("GET /api/nodes/config", s.handleConfigNodesList)
	authenticated("POST /api/nodes/config", s.handleConfigNodesCreate)
	authenticated("POST /api/nodes/config/batch-toggle", s.handleConfigNodesBatchToggle)
	authenticated("POST /api/nodes/config/batch-delete", s.handleConfigNodesBatchDelete)
	authenticated("PUT /api/nodes/config/{name}", s.handleConfigNodeUpdate)
	authenticated("PATCH /api/nodes/config/{name}", s.handleConfigNodeToggle)
	authenticated("DELETE /api/nodes/config/{name}", s.handleConfigNodeDelete)
	authenticated("GET /api/nodes/maintenance", s.handleNodeMaintenance)

	// —— 单节点诊断 ——
	authenticated("POST /api/nodes/probe-all", s.handleProbeAll)
	authenticated("POST /api/nodes/unlock-all", s.handleUnlockAll)
	authenticated("GET /api/nodes/unlock-meta", s.handleUnlockMeta)
	authenticated("GET /api/nodes/unlock-results", s.handleUnlockResults)
	authenticated("GET /api/nodes/traffic/stream", s.handleTrafficStream)
	authenticated("HEAD /api/nodes/traffic/stream", handleSSEHead)
	authenticated("POST /api/nodes/{tag}/probe", s.handleNodeProbe)
	authenticated("POST /api/nodes/{tag}/release", s.handleNodeRelease)
	authenticated("GET /api/nodes/{tag}/speedtest", s.handleNodeSpeedtest)
	authenticated("HEAD /api/nodes/{tag}/speedtest", handleSSEHead)
	authenticated("POST /api/nodes/{tag}/unlock", s.handleNodeUnlock)

	// —— 调试与导入导出 ——
	authenticated("GET /api/debug", s.handleDebug)
	authenticated("GET /api/debug/stream", s.handleDebugStream)
	authenticated("HEAD /api/debug/stream", handleSSEHead)
	authenticated("GET /api/export", s.handleExport)
	authenticated("POST /api/import", s.handleImport)

	// —— 订阅 ——
	authenticated("GET /api/subscription/status", s.handleSubscriptionStatus)
	authenticated("POST /api/subscription/refresh", s.handleSubscriptionRefresh)
	authenticated("GET /api/subscription/refresh-settings", s.handleSubscriptionRefreshSettingsGet)
	authenticated("PUT /api/subscription/refresh-settings", s.handleSubscriptionRefreshSettingsUpdate)
	authenticated("GET /api/subscriptions", s.handleSubscriptionsList)
	authenticated("POST /api/subscriptions", s.handleSubscriptionsCreate)
	authenticated("GET /api/subscriptions/{subscriptionID}", s.handleSubscriptionGet)
	authenticated("PUT /api/subscriptions/{subscriptionID}", s.handleSubscriptionUpdate)
	authenticated("DELETE /api/subscriptions/{subscriptionID}", s.handleSubscriptionDelete)
	authenticated("PATCH /api/subscriptions/{subscriptionID}/enabled", s.handleSubscriptionSetEnabled)
	authenticated("POST /api/subscriptions/{subscriptionID}/activate", s.handleSubscriptionActivate)
	authenticated("POST /api/subscriptions/{subscriptionID}/refresh", s.handleSubscriptionRefreshOne)
	authenticated("GET /api/subscriptions/{subscriptionID}/nodes", s.handleSubscriptionNodes)

	authenticated("POST /api/reload", s.handleReload)

	// —— 免登录 API 凭证（需要会话认证才能管理）——
	authenticated("GET /api/api-keys", s.handleAPIKeysList)
	authenticated("POST /api/api-keys", s.handleAPIKeysCreate)
	authenticated("DELETE /api/api-keys/{apiKeyID}", s.handleAPIKeyDelete)

	// —— 免登录公开 API（X-API-Key 鉴权）——
	// 作用域刻意保持最小：仅允许添加订阅与导入节点。
	publicAPI("POST /api/public/subscriptions", s.handleSubscriptionsCreate)
	publicAPI("POST /api/public/import", s.handleImport)

	// —— 分组 ——
	authenticated("GET /api/groups", s.handleGroupsList)
	authenticated("POST /api/groups", s.handleGroupsCreate)
	authenticated("GET /api/groups/{groupID}/healing-events", s.handleGroupHealEvents)
	authenticated("PUT /api/groups/{groupID}", s.handleGroupUpdate)
	authenticated("DELETE /api/groups/{groupID}", s.handleGroupDelete)
	authenticated("POST /api/groups/{groupID}/members/{nodeID}/activate", s.handleGroupActivate)
	authenticated("POST /api/groups/{groupID}/members/{nodeID}/restore", s.handleGroupRestore)
	authenticated("DELETE /api/groups/{groupID}/members/{nodeID}", s.handleGroupRemoveMember)
	authenticated("DELETE /api/groups/{groupID}/exclusions/{nodeID}", s.handleGroupRemoveExclusion)
	authenticated("POST /api/groups/{groupID}/subscription/reset-token", s.handleGroupResetToken)

	// —— 标签 ——
	// 字面资源（schema/preview/…）与数字 ID 各自独立注册，互不竞争。
	authenticated("GET /api/tags", s.handleTagsList)
	authenticated("POST /api/tags", s.handleTagsCreate)
	authenticated("GET /api/tags/schema", s.handleTagSchemaRoute)
	authenticated("POST /api/tags/preview", s.handleTagPreviewRoute)
	authenticated("POST /api/tags/recompute", s.handleTagRecomputeRoute)
	authenticated("POST /api/tags/templates", s.handleTagTemplatesRoute)
	authenticated("GET /api/tags/assignments", s.handleTagAssignmentsRoute)
	authenticated("POST /api/tags/nodes/batch", s.handleTagNodesBatchRoute)
	authenticated("PUT /api/tags/nodes/{nodeID}", s.handleTagNodeAssignmentsRoute)
	authenticated("GET /api/tags/mutex-groups", s.handleTagMutexGroupsList)
	authenticated("POST /api/tags/mutex-groups", s.handleTagMutexGroupsCreate)
	authenticated("GET /api/tags/mutex-groups/{mutexGroupID}", s.handleTagMutexGroupGet)
	authenticated("PUT /api/tags/mutex-groups/{mutexGroupID}", s.handleTagMutexGroupUpdate)
	authenticated("DELETE /api/tags/mutex-groups/{mutexGroupID}", s.handleTagMutexGroupDelete)
	authenticated("GET /api/tags/{tagID}", s.handleTagGet)
	authenticated("PUT /api/tags/{tagID}", s.handleTagUpdate)
	authenticated("DELETE /api/tags/{tagID}", s.handleTagDelete)
	authenticated("PATCH /api/tags/{tagID}/auto", s.handleTagAutoRoute)

	// —— GeoIP ——
	authenticated("GET /api/geoip/status", s.handleGeoipStatus)
	authenticated("POST /api/geoip/download", s.handleGeoipDownload)
	authenticated("POST /api/geoip/update", s.handleGeoipUpdate)

	// —— 公开分组订阅（分组订阅 Token 鉴权，无会话）——
	publicSubscriptions.HandleFunc("GET /sub/{groupID}", s.handleGroupSubscription)
	publicSubscriptions.HandleFunc("GET /sub/{groupID}/entry", s.handleGroupSubscriptionEntry)

	static.HandleFunc("GET /", s.handleIndex)
	root.Handle("/api/", api)
	root.Handle("/sub/", publicSubscriptions)
	root.Handle("/", static)
	return root
}
