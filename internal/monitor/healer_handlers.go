package monitor

import (
	"net/http"
	"strconv"
)

func (s *Server) handleGroupHealEvents(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储不可用")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("groupID"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, http.StatusBadRequest, "无效分组 ID")
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
