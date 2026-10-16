package monitor

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"easy_proxies/internal/store"
)

// Node maintenance statuses exposed to the maintenance page.
const (
	// maintenanceOnline means the node passed its latest health check.
	maintenanceOnline = "online"
	// maintenanceOffline means the node finished health checking but is
	// currently unreachable. Only this status carries an offline duration.
	maintenanceOffline = "offline"
	// maintenancePending means no health result has been recorded yet.
	maintenancePending = "pending"
	// maintenanceDisabled means an operator switched the node off; it is
	// excluded from availability accounting so it can never be mistaken for a
	// node that keeps failing.
	maintenanceDisabled = "disabled"
)

// NodeMaintenanceEntry is one row of the node maintenance page: the persisted
// node plus its runtime health and how long it has been continuously offline.
type NodeMaintenanceEntry struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	URI     string   `json:"uri"`
	Source  string   `json:"source"`
	Region  string   `json:"region,omitempty"`
	Country string   `json:"country,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Enabled bool     `json:"enabled"`

	// Status is one of online | offline | pending | disabled.
	Status string `json:"status"`

	Available        bool `json:"available"`
	InitialCheckDone bool `json:"initial_check_done"`
	Blacklisted      bool `json:"blacklisted"`

	// OfflineSince is when the current offline streak began: the last
	// successful health check, or the creation time for nodes that never
	// succeeded. Zero unless Status is offline.
	OfflineSince time.Time `json:"offline_since,omitempty"`
	// OfflineDays is how many days OfflineSince lies in the past. It is 0 for
	// every status other than offline.
	OfflineDays float64 `json:"offline_days"`

	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastFailureAt time.Time `json:"last_failure_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	SuccessCount  int64     `json:"success_count"`
	FailureCount  int       `json:"failure_count"`
}

// handleNodeMaintenance lists every node visible on the node management page
// together with its online/offline state, so the UI can filter out nodes that
// have been continuously offline for a user-supplied number of days.
//
// The offline streak starts at the last successful health check. Nodes that
// never succeeded fall back to their creation time, which is exactly the
// promise an operator wants to see: "imported N days ago and still dead".
// Disabled and not-yet-checked nodes report no offline duration at all — their
// frozen last known state must never be read as a live outage.
func (s *Server) handleNodeMaintenance(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "数据存储未初始化")
		return
	}

	nodes, err := s.store.ListManagedNodes(r.Context(), nil)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stats, err := s.store.GetAllNodeStats(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	entries := make([]NodeMaintenanceEntry, 0, len(nodes))
	summary := map[string]int{
		"total":    len(nodes),
		"online":   0,
		"offline":  0,
		"pending":  0,
		"disabled": 0,
	}
	for _, node := range nodes {
		entry := buildNodeMaintenanceEntry(node, stats[node.ID], now)
		switch entry.Status {
		case maintenanceOnline:
			summary["online"]++
		case maintenanceOffline:
			summary["offline"]++
		case maintenancePending:
			summary["pending"]++
		case maintenanceDisabled:
			summary["disabled"]++
		}
		entries = append(entries, entry)
	}

	// Longest-offline first: the maintenance page is read top-down to decide
	// what to remove. Everything else is alphabetical for a stable list.
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		leftOffline, rightOffline := left.Status == maintenanceOffline, right.Status == maintenanceOffline
		if leftOffline != rightOffline {
			return leftOffline
		}
		if leftOffline && left.OfflineDays != right.OfflineDays {
			return left.OfflineDays > right.OfflineDays
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})

	writeJSON(w, map[string]any{
		"generated_at": now,
		"nodes":        entries,
		"summary":      summary,
	})
}

// buildNodeMaintenanceEntry derives the maintenance view of a single node. It
// is pure so the offline-duration rule can be unit tested without a server.
func buildNodeMaintenanceEntry(node store.ManagedNode, stat *store.NodeStats, now time.Time) NodeMaintenanceEntry {
	entry := NodeMaintenanceEntry{
		ID:      node.ID,
		Name:    node.Name,
		URI:     node.URI,
		Source:  node.Source,
		Region:  node.Region,
		Country: node.Country,
		Tags:    node.Tags,
		Enabled: node.Enabled,
	}

	// A node that was never health checked has no persisted stats row.
	if stat == nil {
		entry.Status = maintenancePending
		if !node.Enabled {
			entry.Status = maintenanceDisabled
		}
		return entry
	}

	entry.Available = stat.Available
	entry.InitialCheckDone = stat.InitialCheckDone
	entry.Blacklisted = stat.Blacklisted
	entry.LastSuccessAt = stat.LastSuccessAt
	entry.LastFailureAt = stat.LastFailureAt
	entry.LastError = stat.LastError
	entry.SuccessCount = stat.SuccessCount
	entry.FailureCount = stat.FailureCount

	switch {
	case !node.Enabled:
		entry.Status = maintenanceDisabled
	case !stat.InitialCheckDone:
		entry.Status = maintenancePending
	case stat.Available:
		entry.Status = maintenanceOnline
	default:
		entry.Status = maintenanceOffline
	}
	if entry.Status != maintenanceOffline {
		return entry
	}

	if !stat.LastSuccessAt.IsZero() {
		entry.OfflineSince = stat.LastSuccessAt
	} else {
		entry.OfflineSince = node.CreatedAt
	}
	if !entry.OfflineSince.IsZero() && now.After(entry.OfflineSince) {
		entry.OfflineDays = now.Sub(entry.OfflineSince).Hours() / 24
	}
	return entry
}
