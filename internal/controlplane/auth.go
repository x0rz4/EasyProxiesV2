package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sync"
	"time"

	"easy_proxies/internal/store"
)

// Session is an issued management credential. ExpiresAt is stamped at creation
// from the service's TTL; Validate treats an expired session as absent.
type Session struct {
	Token     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Login rate limiting: maxLoginFailuresWithin wrong passwords from one source
// inside loginFailureWindow lock that source out for loginLockout. The counter
// decays by the window, so slow guessing stays throttled too.
const (
	MaxLoginFailuresWithin = 8
	LoginFailureWindow     = 10 * time.Minute
	LoginLockout           = 5 * time.Minute

	// MaxSessions caps the in-memory session table. Every login inserts one, so
	// without a cap a scripted login loop grows the map (and the SQLite sessions
	// table) without bound.
	MaxSessions = 256

	// DefaultSessionTTL is used when AuthOptions leaves SessionTTL zero.
	DefaultSessionTTL = 24 * time.Hour

	// storeCallTimeout bounds auth-path store calls. A stalled single-writer
	// SQLite queue must not hold a login or an unauthenticated request open-ended.
	storeCallTimeout = 3 * time.Second
)

var (
	// ErrStoreUnavailable reports that a store-backed operation ran without a
	// bound store. The HTTP layer maps it to its own 503 wording.
	ErrStoreUnavailable = errors.New("数据存储不可用")
	// ErrPasswordNotRequired means the panel runs without a management password.
	ErrPasswordNotRequired = errors.New("无需密码")
	// ErrInvalidCredentials covers both a wrong password and an unknown API key.
	ErrInvalidCredentials = errors.New("密码错误")
	// ErrLoginLockedOut means the source exceeded its failure budget. Callers
	// must answer exactly like ErrInvalidCredentials so the lockout is not
	// observable.
	ErrLoginLockedOut = errors.New("登录尝试过于频繁")
)

type loginFailureState struct {
	count       int
	firstSeen   time.Time
	lockedUntil time.Time
}

// AuthOptions wires the service's dependencies. Password is a function rather
// than a string because the management password is edited at runtime by the
// settings API; reading it through a callback keeps one source of truth.
type AuthOptions struct {
	Store      store.Store
	Logger     *log.Logger
	Password   func() string
	SessionTTL time.Duration
	// Now is injectable so rate-limit and expiry tests do not sleep.
	Now func() time.Time
}

// Auth owns management authentication: password checks, session issuance and
// verification, the per-source login rate limiter, and API-key credentials.
// It holds no HTTP concepts — handlers decide status codes and wording.
type Auth struct {
	mu     sync.Mutex // guards store and logger rebinding
	store  store.Store
	logger *log.Logger
	passwd func() string
	ttl    time.Duration
	now    func() time.Time

	sessionMu sync.RWMutex
	sessions  map[string]*Session

	loginMu           sync.Mutex
	loginFailures     map[string]*loginFailureState
	loginFailuresLast time.Time
	usage             apiKeyUsage
}

// NewAuth builds the service. A nil Now or SessionTTL falls back to the
// production defaults, which is what every caller wants outside tests.
func NewAuth(opts AuthOptions) *Auth {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	passwd := opts.Password
	if passwd == nil {
		passwd = func() string { return "" }
	}
	return &Auth{
		store:         opts.Store,
		logger:        logger,
		passwd:        passwd,
		ttl:           ttl,
		now:           now,
		sessions:      make(map[string]*Session),
		loginFailures: make(map[string]*loginFailureState),
	}
}

// SetStore binds or replaces the persistence layer. Sessions already in memory
// stay valid; a nil store means sessions are process-local only.
func (a *Auth) SetStore(st store.Store) {
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := reflect.ValueOf(a.store)
	if previous.Comparable() && previous.Equal(reflect.ValueOf(st)) {
		return
	}
	if a.store == nil && st == nil {
		return
	}
	a.store = st
	a.usage.generation++
	a.usage.entries = nil // IDs from the previous database must not cross bindings.
}

func (a *Auth) currentStore() store.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// SessionTTL is the lifetime stamped on new sessions, also used by the login
// handler for the cookie Max-Age.
func (a *Auth) SessionTTL() time.Duration { return a.ttl }

// PasswordRequired reports whether a management password is configured. The
// login page reads this to decide whether to render the form at all.
func (a *Auth) PasswordRequired() bool { return a.passwd() != "" }

func (a *Auth) password() string { return a.passwd() }

// LoginAllowed reports whether source may attempt a login now, reserving the
// attempt. It is a separate step from Authenticate because the HTTP handler
// checks the lockout before it parses the request body: a locked-out source
// must get the same answer for a malformed body as for a wrong password,
// otherwise the lockout is observable.
func (a *Auth) LoginAllowed(source string) bool {
	if a.password() == "" {
		return true
	}
	return a.loginAllowed(source)
}

// Authenticate verifies password for source and, on success, issues a session.
//
// Callers are expected to have called LoginAllowed first. Errors are
// deliberately coarse: ErrPasswordNotRequired and ErrInvalidCredentials. A
// caller must render a lockout exactly like ErrInvalidCredentials — that is the
// point of the lockout.
func (a *Auth) Authenticate(ctx context.Context, source, password string) (*Session, error) {
	configured := a.password()
	if configured == "" {
		return nil, ErrPasswordNotRequired
	}
	if !SecureCompare(password, configured) {
		a.recordLoginResult(source, false)
		return nil, ErrInvalidCredentials
	}
	a.recordLoginResult(source, true)
	session, err := a.CreateSession(ctx)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// loginAllowed reports whether the source may attempt a login now, recording
// the attempt when it may.
func (a *Auth) loginAllowed(source string) bool {
	now := a.now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if a.loginFailures == nil {
		a.loginFailures = make(map[string]*loginFailureState)
	}
	// Periodically drop stale buckets so the map cannot grow with rotated IPs.
	if now.Sub(a.loginFailuresLast) > time.Hour {
		for key, state := range a.loginFailures {
			if now.Sub(state.firstSeen) > LoginFailureWindow && now.After(state.lockedUntil) {
				delete(a.loginFailures, key)
			}
		}
		a.loginFailuresLast = now
	}
	state, exists := a.loginFailures[source]
	if !exists {
		a.loginFailures[source] = &loginFailureState{firstSeen: now}
		return true
	}
	if now.Before(state.lockedUntil) {
		return false
	}
	if now.Sub(state.firstSeen) > LoginFailureWindow {
		// Window expired without reaching the lockout: reset.
		state.count = 0
		state.firstSeen = now
	}
	return true
}

// recordLoginResult counts a failed attempt and resets on success.
func (a *Auth) recordLoginResult(source string, success bool) {
	now := a.now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if success {
		delete(a.loginFailures, source)
		return
	}
	state, exists := a.loginFailures[source]
	if !exists {
		a.loginFailures[source] = &loginFailureState{firstSeen: now, count: 1}
		return
	}
	state.count++
	if state.count >= MaxLoginFailuresWithin {
		state.lockedUntil = now.Add(LoginLockout)
		state.count = 0
		state.firstSeen = now
	}
}

// CreateSession issues a session and persists it when a store is bound.
// Persistence failures are logged, not returned: the in-memory copy still
// authenticates this process, which is strictly better than failing the login.
func (a *Auth) CreateSession(ctx context.Context) (*Session, error) {
	token, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	now := a.now()
	session := &Session{Token: token, CreatedAt: now, ExpiresAt: now.Add(a.ttl)}

	if st := a.currentStore(); st != nil {
		callCtx, cancel := context.WithTimeout(ctx, storeCallTimeout)
		defer cancel()
		if err := st.CreateSession(callCtx, &store.Session{
			Token:     session.Token,
			CreatedAt: session.CreatedAt,
			ExpiresAt: session.ExpiresAt,
		}); err != nil {
			a.logger.Printf("Failed to persist session to store: %v", err)
		}
	}

	a.sessionMu.Lock()
	if len(a.sessions) >= MaxSessions {
		a.evictOldestLocked()
	}
	a.sessions[token] = session
	a.sessionMu.Unlock()
	return session, nil
}

// Validate reports whether token names a live session. An expired session is
// dropped from both the cache and the store before returning false.
func (a *Auth) Validate(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	a.sessionMu.RLock()
	session, exists := a.sessions[token]
	a.sessionMu.RUnlock()

	if exists {
		if a.now().After(session.ExpiresAt) {
			a.dropSession(ctx, token)
			return false
		}
		return true
	}

	// Fallback: consult the store, e.g. after a restart.
	st := a.currentStore()
	if st == nil {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, storeCallTimeout)
	defer cancel()
	stored, err := st.GetSession(callCtx, token)
	if err != nil || stored == nil {
		return false
	}
	if a.now().After(stored.ExpiresAt) {
		_ = st.DeleteSession(callCtx, token)
		return false
	}
	a.sessionMu.Lock()
	a.sessions[token] = &Session{Token: stored.Token, CreatedAt: stored.CreatedAt, ExpiresAt: stored.ExpiresAt}
	a.sessionMu.Unlock()
	return true
}

func (a *Auth) dropSession(ctx context.Context, token string) {
	a.sessionMu.Lock()
	delete(a.sessions, token)
	a.sessionMu.Unlock()
	if st := a.currentStore(); st != nil {
		callCtx, cancel := context.WithTimeout(ctx, storeCallTimeout)
		defer cancel()
		_ = st.DeleteSession(callCtx, token)
	}
}

// evictOldestLocked removes the oldest in-memory session. Callers must hold
// sessionMu for writing.
func (a *Auth) evictOldestLocked() {
	var oldestToken string
	var oldest time.Time
	for token, session := range a.sessions {
		if oldestToken == "" || session.CreatedAt.Before(oldest) {
			oldestToken, oldest = token, session.CreatedAt
		}
	}
	if oldestToken == "" {
		return
	}
	delete(a.sessions, oldestToken)
	if st := a.currentStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
		defer cancel()
		_ = st.DeleteSession(ctx, oldestToken)
	}
}

// CleanupExpired purges expired sessions from memory and the store. It is the
// policy half of the periodic sweep; the caller owns the ticker and lifetime.
func (a *Auth) CleanupExpired(ctx context.Context) {
	now := a.now()
	a.sessionMu.Lock()
	for token, session := range a.sessions {
		if now.After(session.ExpiresAt) {
			delete(a.sessions, token)
		}
	}
	a.sessionMu.Unlock()

	if st := a.currentStore(); st != nil {
		callCtx, cancel := context.WithTimeout(ctx, storeCallTimeout)
		defer cancel()
		_ = st.CleanupExpiredSessions(callCtx)
	}
}

// APIKeyByToken resolves a caller-supplied credential. A nil key with a nil
// error means the token is unknown; callers must not distinguish unknown from
// malformed in their answer.
func (a *Auth) APIKeyByToken(ctx context.Context, token string) (*store.APIKey, error) {
	st := a.currentStore()
	if st == nil {
		return nil, ErrStoreUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, storeCallTimeout)
	defer cancel()
	key, err := st.GetAPIKeyByToken(callCtx, token)
	if err != nil {
		return nil, err
	}
	if key == nil || !SecureCompare(key.Token, token) {
		return nil, nil
	}
	return key, nil
}

// RecordAPIKeyUse refreshes last_used_at without blocking the request. Failures
// are logged only: the credential already authenticated the call.
func (a *Auth) RecordAPIKeyUse(id int64) {
	a.recordAPIKeyUse(id)
}

// ListAPIKeys returns every session-free credential.
func (a *Auth) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	st := a.currentStore()
	if st == nil {
		return nil, ErrStoreUnavailable
	}
	return st.ListAPIKeys(ctx)
}

// CreateAPIKey mints a credential with the given display name.
func (a *Auth) CreateAPIKey(ctx context.Context, name string) (*store.APIKey, error) {
	st := a.currentStore()
	if st == nil {
		return nil, ErrStoreUnavailable
	}
	token, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	key := &store.APIKey{Name: name, Token: token}
	if err := st.CreateAPIKey(ctx, key); err != nil {
		return nil, err
	}
	return key, nil
}

// DeleteAPIKey revokes a credential.
func (a *Auth) DeleteAPIKey(ctx context.Context, id int64) error {
	st := a.currentStore()
	if st == nil {
		return ErrStoreUnavailable
	}
	if err := st.DeleteAPIKey(ctx, id); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.usage.entries, id)
	a.mu.Unlock()
	return nil
}

// GenerateToken creates a cryptographically secure random token (256 bits).
func GenerateToken() (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	return hex.EncodeToString(tokenBytes), nil
}

// SecureCompare compares two secrets in constant time. Length differences still
// perform a dummy comparison so the timing does not leak the expected length.
func SecureCompare(a, b string) bool {
	aBytes := []byte(a)
	bBytes := []byte(b)
	if len(aBytes) != len(bBytes) {
		dummy := make([]byte, 32)
		subtle.ConstantTimeCompare(dummy, dummy)
		return false
	}
	return subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}
