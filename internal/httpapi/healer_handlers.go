package httpapi

import (
	"net/http"
	"strconv"
)

func (s *Server) handleGroupHealEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, storeUnavailableGeneric) {
		return
	}
	id, ok := pathID(w, r, "groupID", "无效分组 ID")
	if !ok {
		return
	}
	pool, err := s.store.GetGroupPool(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pool == nil {
		writeAPIError(w, http.StatusNotFound, "分组不存在")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListGroupHealEvents(r.Context(), id, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"events": events})
}
