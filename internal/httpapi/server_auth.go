package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/store"
)

// withAuth 认证中间件，如果配置了密码则需要验证
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 如果没有配置密码，直接放行
		s.cfgMu.RLock()
		password := s.cfg.Password
		s.cfgMu.RUnlock()
		if password == "" {
			next(w, r)
			return
		}

		// 检查 Cookie 中的 session token
		cookie, err := r.Cookie("session_token")
		if err == nil && s.validateSession(cookie.Value) {
			next(w, r)
			return
		}

		// 检查 Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if s.validateSession(token) {
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
		key, err := s.store.GetAPIKeyByToken(r.Context(), token)
		if err != nil {
			s.logger.Printf("Failed to load API key: %v", err)
			writeAPIError(w, http.StatusInternalServerError, "读取 API 凭证失败")
			return
		}
		if key == nil || !secureCompareStrings(key.Token, token) {
			// Unknown and malformed keys answer identically to avoid probing.
			w.Header().Set("WWW-Authenticate", apiKeyHeader)
			writeAPIError(w, http.StatusUnauthorized, "API Key 无效或已被撤销")
			return
		}
		s.recordAPIKeyUse(key.ID)
		next(w, r)
	}
}

// recordAPIKeyUse refreshes last_used_at without blocking the request.
func (s *Server) recordAPIKeyUse(id int64) {
	storeImpl := s.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := storeImpl.TouchAPIKey(ctx, id, time.Now()); err != nil {
			s.logger.Printf("Failed to record API key usage: %v", err)
		}
	}()
}

// handleAPIKeysList lists session-free API credentials. These routes are
// session-authenticated; the resulting tokens are only usable on /api/public/*.
func (s *Server) handleAPIKeysList(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w, storeUnavailableCredentials) {
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context())
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
	token, err := s.generateSessionToken()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "生成 API Key 失败")
		return
	}
	key := &store.APIKey{Name: name, Token: token}
	if err := s.store.CreateAPIKey(r.Context(), key); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
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
	if err := s.store.DeleteAPIKey(r.Context(), id); err != nil {
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

// loginAllowed reports whether the source may attempt a login now, recording
// the attempt when it may. Locked-out sources get a consistent delay so the
// lockout itself is not distinguishable from a wrong password by timing.
func (s *Server) loginAllowed(source string) bool {
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFailures == nil {
		s.loginFailures = make(map[string]*loginFailureState)
	}
	// Periodically drop stale buckets so the map cannot grow with rotated IPs.
	if now.Sub(s.loginFailuresLast) > time.Hour {
		for key, state := range s.loginFailures {
			if now.Sub(state.firstSeen) > loginFailureWindow && now.After(state.lockedUntil) {
				delete(s.loginFailures, key)
			}
		}
		s.loginFailuresLast = now
	}
	state, exists := s.loginFailures[source]
	if !exists {
		s.loginFailures[source] = &loginFailureState{firstSeen: now}
		return true
	}
	if now.Before(state.lockedUntil) {
		return false
	}
	if now.Sub(state.firstSeen) > loginFailureWindow {
		// Window expired without reaching the lockout: reset.
		state.count = 0
		state.firstSeen = now
	}
	return true
}

// recordLoginResult counts a failed attempt and resets on success.
func (s *Server) recordLoginResult(source string, success bool) {
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if success {
		delete(s.loginFailures, source)
		return
	}
	state, exists := s.loginFailures[source]
	if !exists {
		s.loginFailures[source] = &loginFailureState{firstSeen: now, count: 1}
		return
	}
	state.count++
	if state.count >= maxLoginFailuresWithin {
		state.lockedUntil = now.Add(loginLockout)
		state.count = 0
		state.firstSeen = now
	}
}

// evictOldestSession bounds the in-memory session table: the oldest session is
// removed when at capacity. Callers must hold sessionMu for writing.
func (s *Server) evictOldestSessionLocked() {
	var oldestToken string
	var oldest time.Time
	for token, session := range s.sessions {
		if oldestToken == "" || session.CreatedAt.Before(oldest) {
			oldestToken, oldest = token, session.CreatedAt
		}
	}
	if oldestToken != "" {
		delete(s.sessions, oldestToken)
		if s.store != nil {
			_ = s.store.DeleteSession(context.Background(), oldestToken)
		}
	}
}

// authPassword returns the configured management password, or "" when the
// panel runs without one.
func (s *Server) authPassword() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Password
}

// handleAuthStatus tells the frontend whether a password is required. It is
// deliberately readable without a session, because the login page needs it to
// decide whether to render the form at all.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.authPassword() == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}
	writeJSON(w, map[string]any{"message": "需要密码", "no_password": false})
}

// handleAuthLogin validates the password and issues a session cookie.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	password := s.authPassword()
	// 如果没有配置密码，直接返回成功（不需要token）
	if password == "" {
		writeJSON(w, map[string]any{"message": "无需密码", "no_password": true})
		return
	}

	source := clientSource(r)
	if !s.loginAllowed(source) {
		// Same random delay as a wrong password so lockout is not observable
		// through timing.
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

	// 使用 constant-time 比较防止时序攻击
	if !secureCompareStrings(req.Password, password) {
		s.recordLoginResult(source, false)
		// 添加随机延迟防止暴力破解
		time.Sleep(time.Duration(100+mathrand.Intn(200)) * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "密码错误"})
		return
	}
	s.recordLoginResult(source, true)

	// 创建新会话
	session, err := s.createSession()
	if err != nil {
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
		MaxAge:   int(s.sessionTTL.Seconds()),
	})

	writeJSON(w, map[string]any{
		"message": "登录成功",
		"token":   session.Token,
	})
}

// generateSessionToken creates a cryptographically secure random token.
func (s *Server) generateSessionToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	return hex.EncodeToString(tokenBytes), nil
}

// createSession creates a new session with expiration.
func (s *Server) createSession() (*Session, error) {
	token, err := s.generateSessionToken()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	session := &Session{
		Token:     token,
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessionTTL),
	}

	// Persist to Store if available. Auth calls get a bounded context: a
	// stalled single-writer SQLite queue must not hold the login (and its
	// goroutine) open-ended.
	if s.store != nil {
		storeSession := &store.Session{
			Token:     session.Token,
			CreatedAt: session.CreatedAt,
			ExpiresAt: session.ExpiresAt,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.store.CreateSession(ctx, storeSession); err != nil {
			s.logger.Printf("Failed to persist session to store: %v", err)
		}
	}

	// Also keep in memory for fast lookups, bounded at maxSessions.
	s.sessionMu.Lock()
	if len(s.sessions) >= maxSessions {
		s.evictOldestSessionLocked()
	}
	s.sessions[token] = session
	s.sessionMu.Unlock()

	return session, nil
}

// validateSession checks if a session token is valid and not expired.
func (s *Server) validateSession(token string) bool {
	// Check in-memory cache first
	s.sessionMu.RLock()
	session, exists := s.sessions[token]
	s.sessionMu.RUnlock()

	if exists {
		if time.Now().After(session.ExpiresAt) {
			s.sessionMu.Lock()
			delete(s.sessions, token)
			s.sessionMu.Unlock()
			// Also delete from store
			if s.store != nil {
				_ = s.store.DeleteSession(context.Background(), token)
			}
			return false
		}
		return true
	}

	// Fallback: check Store (e.g., after restart). Auth lookups get a bounded
	// context: a stalled single-writer SQLite queue must not hold every
	// unauthenticated request open-ended.
	if s.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		storeSess, err := s.store.GetSession(ctx, token)
		if err != nil || storeSess == nil {
			return false
		}
		if time.Now().After(storeSess.ExpiresAt) {
			_ = s.store.DeleteSession(ctx, token)
			return false
		}
		// Restore to in-memory cache
		s.sessionMu.Lock()
		s.sessions[token] = &Session{
			Token:     storeSess.Token,
			CreatedAt: storeSess.CreatedAt,
			ExpiresAt: storeSess.ExpiresAt,
		}
		s.sessionMu.Unlock()
		return true
	}

	return false
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
			now := time.Now()
			s.sessionMu.Lock()
			for token, session := range s.sessions {
				if now.After(session.ExpiresAt) {
					delete(s.sessions, token)
				}
			}
			s.sessionMu.Unlock()

			// Also cleanup in Store
			if s.store != nil {
				_ = s.store.CleanupExpiredSessions(context.Background())
			}
		}
	}
}

// secureCompareStrings performs constant-time string comparison to prevent timing attacks.
func secureCompareStrings(a, b string) bool {
	aBytes := []byte(a)
	bBytes := []byte(b)

	// If lengths differ, still perform a dummy comparison to maintain constant time
	if len(aBytes) != len(bBytes) {
		dummy := make([]byte, 32)
		subtle.ConstantTimeCompare(dummy, dummy)
		return false
	}

	return subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}
