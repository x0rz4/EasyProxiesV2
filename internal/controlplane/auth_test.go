package controlplane

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

// fakeClock lets session expiry and rate-limit windows be tested without
// sleeping. Every Auth timestamp goes through Now.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestAuth(t *testing.T, password string) (*Auth, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	auth := NewAuth(AuthOptions{
		Logger:   log.New(io.Discard, "", 0),
		Password: func() string { return password },
		Now:      clock.Now,
	})
	return auth, clock
}

func TestPasswordRequiredFollowsTheCallback(t *testing.T) {
	password := ""
	auth := NewAuth(AuthOptions{Password: func() string { return password }})
	if auth.PasswordRequired() {
		t.Fatal("empty password must mean no password required")
	}
	password = "secret"
	if !auth.PasswordRequired() {
		t.Fatal("a configured password must require login")
	}
}

// A session is live until its TTL elapses, then Validate rejects it and drops
// it from the cache so a second lookup cannot resurrect it.
func TestValidateRejectsExpiredSession(t *testing.T) {
	auth, clock := newTestAuth(t, "secret")
	session, err := auth.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !auth.Validate(context.Background(), session.Token) {
		t.Fatal("a fresh session must validate")
	}
	clock.Advance(DefaultSessionTTL + time.Second)
	if auth.Validate(context.Background(), session.Token) {
		t.Fatal("an expired session must not validate")
	}
	if auth.Validate(context.Background(), session.Token) {
		t.Fatal("an expired session must stay rejected")
	}
}

func TestValidateRejectsUnknownAndEmptyTokens(t *testing.T) {
	auth, _ := newTestAuth(t, "secret")
	for _, token := range []string{"", "not-a-session"} {
		if auth.Validate(context.Background(), token) {
			t.Fatalf("token %q must not validate", token)
		}
	}
}

// The in-memory table is capped: issuing more than MaxSessions must not grow
// it without bound, and the oldest session is the one dropped.
func TestCreateSessionEvictsOldestAtCapacity(t *testing.T) {
	auth, clock := newTestAuth(t, "secret")
	first, err := auth.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxSessions; i++ {
		clock.Advance(time.Millisecond) // keep CreatedAt strictly ordered
		if _, err := auth.CreateSession(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if auth.Validate(context.Background(), first.Token) {
		t.Fatal("the oldest session must have been evicted")
	}
	auth.sessionMu.RLock()
	size := len(auth.sessions)
	auth.sessionMu.RUnlock()
	if size != MaxSessions {
		t.Fatalf("session table size = %d, want %d", size, MaxSessions)
	}
}

// The brute-force guard: the failure budget locks the source out even for the
// correct password, a different source is unaffected, and success clears the
// record.
func TestLoginRateLimitLocksOutBursts(t *testing.T) {
	auth, _ := newTestAuth(t, "secret")
	ctx := context.Background()

	for i := 0; i < MaxLoginFailuresWithin; i++ {
		if !auth.LoginAllowed("203.0.113.9") {
			t.Fatalf("attempt %d must still be allowed", i+1)
		}
		if _, err := auth.Authenticate(ctx, "203.0.113.9", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidCredentials", i+1, err)
		}
	}

	if auth.LoginAllowed("203.0.113.9") {
		t.Fatal("the source must be locked out after the failure budget")
	}
	// Lockout is checked before the password, so even the right one is refused.
	if auth.LoginAllowed("203.0.113.9") {
		t.Fatal("a locked-out source stays locked regardless of the password")
	}

	if !auth.LoginAllowed("198.51.100.7") {
		t.Fatal("a different source must be unaffected")
	}
	if _, err := auth.Authenticate(ctx, "198.51.100.7", "secret"); err != nil {
		t.Fatalf("correct password for a clean source: %v", err)
	}
}

// A failure window that elapses without reaching the budget resets the count,
// so an attacker cannot accumulate failures indefinitely at a slow rate.
func TestLoginFailureWindowResets(t *testing.T) {
	auth, clock := newTestAuth(t, "secret")
	ctx := context.Background()

	for i := 0; i < MaxLoginFailuresWithin-1; i++ {
		_, _ = auth.Authenticate(ctx, "203.0.113.9", "wrong")
	}
	clock.Advance(LoginFailureWindow + time.Second)
	if !auth.LoginAllowed("203.0.113.9") {
		t.Fatal("the expired window must not lock the source out")
	}
	if _, err := auth.Authenticate(ctx, "203.0.113.9", "secret"); err != nil {
		t.Fatalf("correct password after the window reset: %v", err)
	}
}

func TestAuthenticateWithoutPasswordIsNotAnError(t *testing.T) {
	auth, _ := newTestAuth(t, "")
	if _, err := auth.Authenticate(context.Background(), "x", "anything"); !errors.Is(err, ErrPasswordNotRequired) {
		t.Fatalf("error = %v, want ErrPasswordNotRequired", err)
	}
}

// Sessions survive a restart through the store: a token missing from the cache
// is restored when the store still has it.
func TestValidateFallsBackToStore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	first, _ := newTestAuth(t, "secret")
	first.SetStore(db)
	session, err := first.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A second service instance simulates a restart: empty cache, same store.
	second, _ := newTestAuth(t, "secret")
	second.SetStore(db)
	if !second.Validate(context.Background(), session.Token) {
		t.Fatal("a persisted session must validate after a restart")
	}
}

func TestStoreBackedSessionExpiryIsDeleted(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth-expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	auth, clock := newTestAuth(t, "secret")
	auth.SetStore(db)
	session, err := auth.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(DefaultSessionTTL + time.Second)
	// Clear the cache so the lookup has to consult the store.
	auth.sessionMu.Lock()
	auth.sessions = map[string]*Session{}
	auth.sessionMu.Unlock()

	if auth.Validate(context.Background(), session.Token) {
		t.Fatal("an expired persisted session must not validate")
	}
	stored, err := db.GetSession(context.Background(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatal("the expired persisted session must have been deleted")
	}
}

func TestCleanupExpiredDropsMemoryAndStore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	auth, clock := newTestAuth(t, "secret")
	auth.SetStore(db)
	session, err := auth.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(DefaultSessionTTL + time.Second)
	auth.CleanupExpired(context.Background())

	auth.sessionMu.RLock()
	_, cached := auth.sessions[session.Token]
	auth.sessionMu.RUnlock()
	if cached {
		t.Fatal("cleanup must drop expired sessions from memory")
	}
	stored, err := db.GetSession(context.Background(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatal("cleanup must drop expired sessions from the store")
	}
}

// API keys: a stored token resolves, an unknown one returns (nil, nil) so the
// caller can answer identically for unknown and malformed, and no store means
// ErrStoreUnavailable rather than a silent failure.
func TestAPIKeyLifecycle(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth-keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	auth, _ := newTestAuth(t, "secret")
	auth.SetStore(db)
	ctx := context.Background()

	key, err := auth.CreateAPIKey(ctx, "CI")
	if err != nil {
		t.Fatal(err)
	}
	if len(key.Token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(key.Token))
	}

	got, err := auth.APIKeyByToken(ctx, key.Token)
	if err != nil || got == nil || got.ID != key.ID {
		t.Fatalf("resolve stored key: %+v %v", got, err)
	}
	if got, err := auth.APIKeyByToken(ctx, "bogus"); err != nil || got != nil {
		t.Fatalf("unknown key must be (nil, nil), got %+v %v", got, err)
	}

	keys, err := auth.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list = %+v %v, want one key", keys, err)
	}

	if err := auth.DeleteAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := auth.APIKeyByToken(ctx, key.Token); err != nil || got != nil {
		t.Fatalf("revoked key must not resolve, got %+v %v", got, err)
	}
}

func TestAPIKeyOperationsWithoutStore(t *testing.T) {
	auth, _ := newTestAuth(t, "secret")
	ctx := context.Background()
	if _, err := auth.APIKeyByToken(ctx, "x"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("APIKeyByToken error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := auth.ListAPIKeys(ctx); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("ListAPIKeys error = %v, want ErrStoreUnavailable", err)
	}
	if _, err := auth.CreateAPIKey(ctx, "x"); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("CreateAPIKey error = %v, want ErrStoreUnavailable", err)
	}
	if err := auth.DeleteAPIKey(ctx, 1); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("DeleteAPIKey error = %v, want ErrStoreUnavailable", err)
	}
}

// SecureCompare must reject differing lengths and differing bytes while
// accepting identical strings.
func TestSecureCompare(t *testing.T) {
	if !SecureCompare("secret", "secret") {
		t.Fatal("identical strings must compare equal")
	}
	if SecureCompare("secret", "secre") {
		t.Fatal("different lengths must not compare equal")
	}
	if SecureCompare("secret", "secreT") {
		t.Fatal("different bytes must not compare equal")
	}
	if !SecureCompare("", "") {
		t.Fatal("two empty strings must compare equal")
	}
}

func TestGenerateTokenIsRandom(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		token, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 64 {
			t.Fatalf("token %q has length %d, want 64", token, len(token))
		}
		if _, dup := seen[token]; dup {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = struct{}{}
	}
}
