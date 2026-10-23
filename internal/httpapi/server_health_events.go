package httpapi

import (
	"context"
	"net/http"
	"time"
)

// handleHealthEventStats exposes health event delivery state: journal capacity,
// backlog, oldest retained event and each consumer's cursor.
//
// It replaces the previous silent FIFO overflow as the way an operator learns
// that a consumer is wedged. Growing backlog or a stalled cursor is now visible
// instead of showing up later as a group that never recovers.
func (s *Server) handleHealthEventStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stats, err := s.mgr.HealthEventStats(ctx)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "health event stats unavailable: "+err.Error())
		return
	}
	writeJSON(w, stats)
}
