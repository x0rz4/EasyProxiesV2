package httpapi

import (
	"net/http"

	"easy_proxies/internal/store"
)

// handleSubscriptionStatus returns the current subscription refresh status.
// Works even when subRefresher is nil by reading config directly.
func (s *Server) handleSubscriptionStatus(w http.ResponseWriter, r *http.Request) {

	if s.subRefresher == nil {
		// No subscription manager — read config directly to provide accurate status
		s.cfgMu.RLock()
		c := s.cfgSrc
		s.cfgMu.RUnlock()

		resp := map[string]any{
			"enabled":           false,
			"has_subscriptions": false,
			"message":           "订阅管理器未初始化",
		}
		if c != nil {
			c.RLock()
			resp["enabled"] = c.SubscriptionRefresh.Enabled
			resp["has_subscriptions"] = len(c.Subscriptions) > 0
			c.RUnlock()
		}
		writeJSON(w, resp)
		return
	}

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"enabled":            status.Enabled,
		"has_subscriptions":  status.HasSubscriptions,
		"last_refresh":       status.LastRefresh,
		"next_refresh":       status.NextRefresh,
		"node_count":         status.NodeCount,
		"last_error":         status.LastError,
		"refresh_count":      status.RefreshCount,
		"is_refreshing":      status.IsRefreshing,
		"parsed":             status.Parsed,
		"created":            status.Created,
		"updated":            status.Updated,
		"duplicates_skipped": status.DuplicatesSkipped,
		"invalid":            status.Invalid,
	})
}

// handleSubscriptionRefresh triggers an immediate subscription refresh.
func (s *Server) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {

	if s.subRefresher == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"error": "订阅管理器未初始化，请重启程序"})
		return
	}

	if err := s.subRefresher.RefreshNow(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	// A refresh creates, updates and disables nodes, so which nodes exist changed
	// and every one of them needs its auto tags re-derived.
	s.enqueueRetagAll()

	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{
		"message":            "刷新成功",
		"node_count":         status.NodeCount,
		"parsed":             status.Parsed,
		"created":            status.Created,
		"updated":            status.Updated,
		"duplicates_skipped": status.DuplicatesSkipped,
		"invalid":            status.Invalid,
	})
}

// subscriptionItemID resolves the {subscriptionID} path segment, answering 400
// itself when it is not a positive ID. Every /api/subscriptions/{id} handler
// starts here, so the guard cannot drift between them.
func (s *Server) subscriptionItemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if s.subRefresher == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "订阅管理器未初始化")
		return 0, false
	}
	return pathID(w, r, "subscriptionID", "无效的订阅 ID")
}

// writeSubscriptionError maps a subscription-manager error onto its HTTP status.
func writeSubscriptionError(w http.ResponseWriter, err error) {
	writeAPIError(w, subscriptionErrorStatus(err), err.Error())
}

func (s *Server) handleSubscriptionsList(w http.ResponseWriter, r *http.Request) {
	if s.subRefresher == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "订阅管理器未初始化")
		return
	}
	subs, err := s.subRefresher.List(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"subscriptions": subs})
}

func (s *Server) handleSubscriptionsCreate(w http.ResponseWriter, r *http.Request) {
	if s.subRefresher == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "订阅管理器未初始化")
		return
	}
	var input store.Subscription
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := s.subRefresher.Create(r.Context(), input)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

func (s *Server) handleSubscriptionGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	sub, err := s.subRefresher.Get(r.Context(), id)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, sub)
}

func (s *Server) handleSubscriptionUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	var input store.Subscription
	if err := decodeJSON(r, &input); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := s.subRefresher.Update(r.Context(), id, input)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, sub)
}

func (s *Server) handleSubscriptionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	if err := s.subRefresher.Delete(r.Context(), id); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSubscriptionSetEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &input); err != nil || input.Enabled == nil {
		writeAPIError(w, http.StatusBadRequest, "enabled 字段必填")
		return
	}
	if err := s.subRefresher.SetEnabled(r.Context(), id, *input.Enabled); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSubscriptionActivate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	if err := s.subRefresher.ActivateExclusive(r.Context(), id); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSubscriptionRefreshOne(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	if err := s.subRefresher.RefreshOne(r.Context(), id); err != nil {
		writeSubscriptionError(w, err)
		return
	}
	status := s.subRefresher.Status()
	writeJSON(w, map[string]any{"ok": true, "parsed": status.Parsed, "created": status.Created,
		"updated": status.Updated, "duplicates_skipped": status.DuplicatesSkipped, "invalid": status.Invalid})
}

func (s *Server) handleSubscriptionNodes(w http.ResponseWriter, r *http.Request) {
	id, ok := s.subscriptionItemID(w, r)
	if !ok {
		return
	}
	nodes, err := s.subRefresher.Nodes(r.Context(), id)
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	writeJSON(w, map[string]any{"nodes": nodes})
}
