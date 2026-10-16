package monitor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"easy_proxies/internal/geoip"
)

// geoipResponse is the JSON structure returned by the GeoIP management endpoints.
type geoipResponse struct {
	Enabled     bool                     `json:"enabled"`
	Database    geoip.DatabaseStatusInfo `json:"database"`
	Message     string                   `json:"message,omitempty"`
	ReloadHint  bool                     `json:"reload_hint,omitempty"` // true when the runtime GeoIP lookup should be reloaded to pick up the new db
	Reloaded    bool                     `json:"reloaded"`
	ReloadError string                   `json:"reload_error,omitempty"`
}

// geoipDatabasePath reads the configured GeoIP database path under cfgSrc.
func (s *Server) geoipDatabasePath() string {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return ""
	}
	c.RLock()
	defer c.RUnlock()
	return c.GeoIP.DatabasePath
}

// geoipEnabled reads the configured GeoIP enabled flag under cfgSrc.
func (s *Server) geoipEnabled() bool {
	s.cfgMu.RLock()
	c := s.cfgSrc
	s.cfgMu.RUnlock()
	if c == nil {
		return false
	}
	c.RLock()
	defer c.RUnlock()
	return c.GeoIP.Enabled
}

// handleGeoipStatus reports the on-disk state of the GeoIP database.
func (s *Server) handleGeoipStatus(w http.ResponseWriter, r *http.Request) {
	dbPath := s.geoipDatabasePath()
	writeJSON(w, geoipResponse{
		Enabled:  s.geoipEnabled(),
		Database: geoip.DatabaseStatus(dbPath),
	})
}

// handleGeoipDownload downloads the GeoIP database if it is missing.
func (s *Server) handleGeoipDownload(w http.ResponseWriter, r *http.Request) {
	dbPath := s.geoipDatabasePath()
	if dbPath == "" {
		writeAPIError(w, http.StatusBadRequest, "GeoIP 数据库路径未配置")
		return
	}

	s.closeGeoipLookup()
	if err := geoip.EnsureDatabase(dbPath); err != nil {
		s.logger.Printf("GeoIP database download failed: %v", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("下载 IP 库失败: %v", err))
		return
	}
	reloaded, reloadError := s.reloadAfterGeoipMutation(r.Context())
	writeJSON(w, geoipResponse{
		Enabled: s.geoipEnabled(), Database: geoip.DatabaseStatus(dbPath),
		Message: "IP 库下载完成", Reloaded: reloaded, ReloadError: reloadError,
	})
}

// handleGeoipUpdate forces a re-download (update) of the GeoIP database,
// overwriting the existing file.
func (s *Server) handleGeoipUpdate(w http.ResponseWriter, r *http.Request) {
	dbPath := s.geoipDatabasePath()
	if dbPath == "" {
		writeAPIError(w, http.StatusBadRequest, "GeoIP 数据库路径未配置")
		return
	}

	s.closeGeoipLookup()
	if err := geoip.DownloadDatabase(dbPath); err != nil {
		s.logger.Printf("GeoIP database update failed: %v", err)
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("更新 IP 库失败: %v", err))
		return
	}
	reloaded, reloadError := s.reloadAfterGeoipMutation(r.Context())
	writeJSON(w, geoipResponse{
		Enabled: s.geoipEnabled(), Database: geoip.DatabaseStatus(dbPath),
		Message: "IP 库更新完成", Reloaded: reloaded, ReloadError: reloadError,
	})
}

func (s *Server) reloadAfterGeoipMutation(ctx context.Context) (bool, string) {
	if s.nodeMgr == nil {
		return false, "节点管理器未初始化，无法自动回填区域"
	}
	reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := s.nodeMgr.TriggerReload(reloadCtx); err != nil {
		s.logger.Printf("GeoIP database installed but node location backfill failed: %v", err)
		return false, err.Error()
	}
	return true, ""
}

// Session management functions
