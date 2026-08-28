package monitor

import "net/http"

func bindRouteAction(action string, handler func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { handler(w, r, action) }
}

func handleSSEHead(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// routes builds the HTTP surface from method-aware Go 1.22+ ServeMux
// patterns. Keeping API and public-subscription routes in child muxes ensures
// an unknown endpoint is a real 404 rather than a React SPA fallback.
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

	public("GET /api/auth", bindRouteAction("status", s.handleAuth))
	public("POST /api/auth", bindRouteAction("login", s.handleAuth))
	authenticated("GET /api/settings", bindRouteAction("get", s.handleSettings))
	authenticated("PUT /api/settings", bindRouteAction("update", s.handleSettings))
	authenticated("GET /api/operations/probe-settings", bindRouteAction("get", s.handleProbeSettings))
	authenticated("PUT /api/operations/probe-settings", bindRouteAction("update", s.handleProbeSettings))
	authenticated("GET /api/operations/probe-status", s.handleProbeStatus)
	authenticated("GET /api/operations/node-check-settings", bindRouteAction("get", s.handleNodeCheckSettings))
	authenticated("PUT /api/operations/node-check-settings", bindRouteAction("update", s.handleNodeCheckSettings))

	authenticated("GET /api/node-check/results", s.handleNodeCheckResults)
	authenticated("GET /api/node-check/tasks", bindRouteAction("list", s.handleNodeCheckTasks))
	authenticated("POST /api/node-check/tasks", bindRouteAction("create", s.handleNodeCheckTasks))
	authenticated("GET /api/node-check/tasks/{taskID}", bindRouteAction("get", s.handleNodeCheckTaskItem))
	authenticated("DELETE /api/node-check/tasks/{taskID}", bindRouteAction("cancel", s.handleNodeCheckTaskItem))
	authenticated("GET /api/node-check/tasks/{taskID}/events", bindRouteAction("events", s.handleNodeCheckTaskItem))
	authenticated("HEAD /api/node-check/tasks/{taskID}/events", handleSSEHead)

	authenticated("GET /api/nodes", s.handleNodes)
	authenticated("GET /api/nodes/config", bindRouteAction("list", s.handleConfigNodes))
	authenticated("POST /api/nodes/config", bindRouteAction("create", s.handleConfigNodes))
	authenticated("POST /api/nodes/config/batch-toggle", s.handleConfigNodesBatchToggle)
	authenticated("POST /api/nodes/config/batch-delete", s.handleConfigNodesBatchDelete)
	authenticated("PUT /api/nodes/config/{name}", bindRouteAction("update", s.handleConfigNodeItem))
	authenticated("PATCH /api/nodes/config/{name}", bindRouteAction("toggle", s.handleConfigNodeItem))
	authenticated("DELETE /api/nodes/config/{name}", bindRouteAction("delete", s.handleConfigNodeItem))
	authenticated("POST /api/nodes/probe-all", s.handleProbeAll)
	authenticated("POST /api/nodes/unlock-all", s.handleUnlockAll)
	authenticated("GET /api/nodes/unlock-meta", s.handleUnlockMeta)
	authenticated("GET /api/nodes/unlock-results", s.handleUnlockResults)
	authenticated("GET /api/nodes/traffic/stream", s.handleTrafficStream)
	authenticated("HEAD /api/nodes/traffic/stream", handleSSEHead)
	authenticated("POST /api/nodes/{tag}/probe", bindRouteAction("probe", s.handleNodeAction))
	authenticated("POST /api/nodes/{tag}/release", bindRouteAction("release", s.handleNodeAction))
	authenticated("GET /api/nodes/{tag}/speedtest", bindRouteAction("speedtest", s.handleNodeAction))
	authenticated("HEAD /api/nodes/{tag}/speedtest", handleSSEHead)
	authenticated("POST /api/nodes/{tag}/unlock", bindRouteAction("unlock", s.handleNodeAction))

	authenticated("GET /api/debug", s.handleDebug)
	authenticated("GET /api/debug/stream", s.handleDebugStream)
	authenticated("HEAD /api/debug/stream", handleSSEHead)
	authenticated("GET /api/export", s.handleExport)
	authenticated("POST /api/import", s.handleImport)
	authenticated("GET /api/subscription/status", s.handleSubscriptionStatus)
	authenticated("POST /api/subscription/refresh", s.handleSubscriptionRefresh)
	authenticated("GET /api/subscriptions", bindRouteAction("list", s.handleSubscriptions))
	authenticated("POST /api/subscriptions", bindRouteAction("create", s.handleSubscriptions))
	authenticated("GET /api/subscriptions/{subscriptionID}", bindRouteAction("get", s.handleSubscriptionItem))
	authenticated("PUT /api/subscriptions/{subscriptionID}", bindRouteAction("update", s.handleSubscriptionItem))
	authenticated("DELETE /api/subscriptions/{subscriptionID}", bindRouteAction("delete", s.handleSubscriptionItem))
	authenticated("PATCH /api/subscriptions/{subscriptionID}/enabled", bindRouteAction("enabled", s.handleSubscriptionItem))
	authenticated("POST /api/subscriptions/{subscriptionID}/activate", bindRouteAction("activate", s.handleSubscriptionItem))
	authenticated("POST /api/subscriptions/{subscriptionID}/refresh", bindRouteAction("refresh", s.handleSubscriptionItem))
	authenticated("GET /api/subscriptions/{subscriptionID}/nodes", bindRouteAction("nodes", s.handleSubscriptionItem))

	authenticated("POST /api/reload", s.handleReload)
	authenticated("GET /api/groups", bindRouteAction("list", s.handleGroups))
	authenticated("POST /api/groups", bindRouteAction("create", s.handleGroups))
	authenticated("PUT /api/groups/{groupID}", bindRouteAction("update", s.handleGroupItem))
	authenticated("DELETE /api/groups/{groupID}", bindRouteAction("delete", s.handleGroupItem))
	authenticated("POST /api/groups/{groupID}/members/{nodeID}/activate", bindRouteAction("activate", s.handleGroupItem))
	authenticated("POST /api/groups/{groupID}/members/{nodeID}/restore", bindRouteAction("restore", s.handleGroupItem))
	authenticated("DELETE /api/groups/{groupID}/members/{nodeID}", bindRouteAction("remove-member", s.handleGroupItem))
	authenticated("DELETE /api/groups/{groupID}/exclusions/{nodeID}", bindRouteAction("remove-exclusion", s.handleGroupItem))
	authenticated("POST /api/groups/{groupID}/subscription/reset-token", bindRouteAction("reset-token", s.handleGroupItem))

	authenticated("GET /api/tags", bindRouteAction("list", s.handleTags))
	authenticated("POST /api/tags", bindRouteAction("create", s.handleTags))
	authenticated("GET /api/tags/schema", bindRouteAction("schema", s.handleTagItem))
	authenticated("POST /api/tags/preview", bindRouteAction("preview", s.handleTagItem))
	authenticated("POST /api/tags/recompute", bindRouteAction("recompute", s.handleTagItem))
	authenticated("POST /api/tags/templates", bindRouteAction("templates", s.handleTagItem))
	authenticated("GET /api/tags/assignments", bindRouteAction("assignments", s.handleTagItem))
	authenticated("POST /api/tags/nodes/batch", bindRouteAction("nodes-batch", s.handleTagItem))
	authenticated("PUT /api/tags/nodes/{nodeID}", bindRouteAction("node", s.handleTagItem))
	authenticated("GET /api/tags/mutex-groups", bindRouteAction("list-mutex-groups", s.handleTagItem))
	authenticated("POST /api/tags/mutex-groups", bindRouteAction("create-mutex-group", s.handleTagItem))
	authenticated("GET /api/tags/mutex-groups/{mutexGroupID}", bindRouteAction("get-mutex-group", s.handleTagItem))
	authenticated("PUT /api/tags/mutex-groups/{mutexGroupID}", bindRouteAction("update-mutex-group", s.handleTagItem))
	authenticated("DELETE /api/tags/mutex-groups/{mutexGroupID}", bindRouteAction("delete-mutex-group", s.handleTagItem))
	authenticated("GET /api/tags/{tagID}", bindRouteAction("get-tag", s.handleTagItem))
	authenticated("PUT /api/tags/{tagID}", bindRouteAction("update-tag", s.handleTagItem))
	authenticated("DELETE /api/tags/{tagID}", bindRouteAction("delete-tag", s.handleTagItem))
	authenticated("PATCH /api/tags/{tagID}/auto", bindRouteAction("auto", s.handleTagItem))

	authenticated("GET /api/geoip/status", s.handleGeoipStatus)
	authenticated("POST /api/geoip/download", s.handleGeoipDownload)
	authenticated("POST /api/geoip/update", s.handleGeoipUpdate)

	publicSubscriptions.HandleFunc("GET /sub/{groupID}", s.handleGroupSubscription)
	publicSubscriptions.HandleFunc("GET /sub/{groupID}/entry", s.handleGroupSubscription)

	static.HandleFunc("GET /", s.handleIndex)
	root.Handle("/api/", api)
	root.Handle("/sub/", publicSubscriptions)
	root.Handle("/", static)
	return root
}
