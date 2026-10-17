package httpapi

import (
	"context"
	"errors"
	mathrand "math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"easy_proxies/internal/controlplane"
	json "easy_proxies/internal/jsonx"
)

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		if !s.authService().PasswordRequired() {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && s.authService().Validate(r.Context(), cookie.Value) {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if s.authService().Validate(r.Context(), token) {
				next(w, r)
				return
			}
		}

		// 未授权
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "未授权，请先登录"})
	}
}

// apiKeyHeader is the canonical header carrying a session-free API credential.
const apiKeyHeader = "X-API-Key"

// apiKeyFromRequest extracts the caller-supplied API credential. The canonical
// carrier is the X-API-Key header; `Authorization: Bearer` and the `api_key`
// query parameter are also accepted so plain curl/scripts stay convenient.
func apiKeyFromRequest(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get(apiKeyHeader)); key != "" {
		return key
	}
	if header := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(header), "bearer ") {
		if token := strings.TrimSpace(header[len("Bearer "):]); token != "" {
			return token
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("api_key"))
}

// withAPIKey guards the session-free public API surface. A stored API key is
// required, and session cookies/Bearer sessions are deliberately NOT accepted
// here: these endpoints must stay usable by unattended scripts and must not be
// reachable with a browser cookie alone. The privilege granted is exactly the
// set of routes wrapped by this middleware, i.e. adding a subscription and
// importing nodes — no read, update, delete, refresh or group access.
func (s *Server) withAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireStore(w, storeUnavailableCredentials) {
			return
		}
		token := apiKeyFromRequest(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", apiKeyHeader)
			writeAPIError(w, http.StatusUnauthorized, "缺少 API Key，请在 X-API-Key 请求头中提供")
			return
		}
		key, err := s.authService().APIKeyByToken(r.Context(), token)
		if err != nil {
			s.logger.Printf("Failed to load API key: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "读取 API 凭证失败")
			return
		}
		if key == nil {
			// Unknown and malformed keys answer identically to avoid probing.
			w.Header().Set("WWW-Authenticate", apiKeyHeader)
			writeAPIError(w, http.StatusUnauthorized, "API Key 无效或已被撤销")
			return
		}
		s.authService().RecordAPIKeyUse(key.ID)
		next(w, r)
	}
}

// handleAPIKeysList lists session-free API credentials. These routes are
// session-authenticated; the resulting tokens are only usable on /api/public/*.
func (s *Server) handleAPIKeysList(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, storeUnavailableCredentials) {
		return
	}
	keys, err := s.authService().ListAPIKeys(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"api_keys": keys})
}

func (s *Server) handleAPIKeysCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, storeUnavailableCredentials) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 64 {
		writeAPIError(w, http.StatusBadRequest, "凭证名称过长（最多 64 字符）")
		return
	}
	key, err := s.authService().CreateAPIKey(r.Context(), name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "生成 API Key 失败")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, key)
}

// handleAPIKeyDelete revokes a single credential.
func (s *Server) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, storeUnavailableCredentials) {
		return
	}
	id, ok := pathID(w, r, "apiKeyID", "无效的凭证 ID")
	if !ok {
		return
	}
	if err := s.authService().DeleteAPIKey(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "API Key 已撤销"})
}

// clientSource identifies the rate-limit bucket for a login attempt. The
// X-Forwarded-For header is deliberately ignored: the management endpoint is
// typically reached directly or through a trusted reverse proxy already
// covered by RemoteAddr, and honoring a spoofable header would let an
// attacker rotate buckets freely.
func clientSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleAuthStatus tells the frontend whether a password is required. It is
// deliberately readable without a session, because the login page needs it to
// decide whether to render the form at all.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authService().PasswordRequired() {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}
	writeJSON(w, map[string]any{"message": "需要密码", "no_password": false})
}

// handleAuthLogin validates the password and issues a session cookie.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authService().PasswordRequired() {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	source := clientSource(r)
	if !s.authService().LoginAllowed(source) {
		// Same random delay as a wrong password so lockout is not observable
		// through timing. Deliberately before body parsing: a locked-out source
		// must not be able to tell a malformed body from a wrong password.
		time.Sleep(time.Duration(100+mathrand.Intn(200)) * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "请求格式错误"})
		return
	}

	session, err := s.authService().Authenticate(r.Context(), source, req.Password)
	switch {
	case err == nil:
		// continue below
	case errors.Is(err, controlplane.ErrInvalidCredentials):
		// 添加随机延迟防止暴力破解
		time.Sleep(time.Duration(100+mathrand.Intn(200)) * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	default:
		s.logger.Printf("Failed to create session: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": "服务器错误"})
		return
	}

	// 设置 HttpOnly Cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    session.Token,
		Path:     "/",
		HttpOnly: true,
		// The panel is usually served over plain HTTP on a LAN; Secure would
		// break cookie-based login there. Enable via reverse proxy TLS
		// termination environments reconfiguring this build for HTTPS.
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.authService().SessionTTL().Seconds()),
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   session.Token,
	})
}

// cleanupExpiredSessions periodically removes expired sessions.
func (s *Server) cleanupExpiredSessions() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.authService().CleanupExpired(context.Background())
		}
	}
}

// authPassword returns the configured management password, or "" when the
// panel runs without one. The Auth service calls it on every check so a
// settings change takes effect without rebinding anything.
func (s *Server) authPassword() string {
	if s == nil {
		return ""
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Password
}
