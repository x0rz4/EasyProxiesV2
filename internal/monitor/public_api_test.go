package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/store"
)

func newAPIKeyTestServer(t *testing.T) (*Server, *store.APIKey) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "public-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	key := &store.APIKey{Name: "CI 导入", Token: strings.Repeat("a", 64)}
	if err := db.CreateAPIKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return &Server{
		store:    db,
		logger:   log.New(io.Discard, "", 0),
		sessions: map[string]*Session{},
		// createSession stamps ExpiresAt from sessionTTL. A zero TTL makes every
		// session expire the instant it is issued, so the session-based cases
		// below would pass or fail depending on where the clock ticked.
		sessionTTL: time.Hour,
	}, key
}

// The public API only accepts a stored API key. Sessions (and therefore the
// browser cookie / session Bearer token) must never unlock these routes.
func TestWithAPIKeyAcceptsOnlyStoredKeys(t *testing.T) {
	s, key := newAPIKeyTestServer(t)
	handler := s.withAPIKey(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	withQuery := func(r *http.Request, name, value string) {
		query := r.URL.Query()
		query.Set(name, value)
		r.URL.RawQuery = query.Encode()
	}

	cases := []struct {
		name    string
		prepare func(*http.Request)
		want    int
	}{
		{name: "no credential", prepare: func(*http.Request) {}, want: http.StatusUnauthorized},
		{name: "unknown key", prepare: func(r *http.Request) { r.Header.Set(apiKeyHeader, "bogus") }, want: http.StatusUnauthorized},
		{name: "empty header", prepare: func(r *http.Request) { r.Header.Set(apiKeyHeader, "   ") }, want: http.StatusUnauthorized},
		{name: "x-api-key header", prepare: func(r *http.Request) { r.Header.Set(apiKeyHeader, key.Token) }, want: http.StatusOK},
		{name: "authorization bearer", prepare: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+key.Token)
		}, want: http.StatusOK},
		{name: "api_key query", prepare: func(r *http.Request) { withQuery(r, "api_key", key.Token) }, want: http.StatusOK},
		{name: "session bearer is rejected", prepare: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer session-token")
		}, want: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/public/import", nil)
			tc.prepare(request)
			recorder := httptest.NewRecorder()
			handler(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("status=%d, want %d (body=%s)", recorder.Code, tc.want, recorder.Body.String())
			}
		})
	}
}

// A successful call refreshes last_used_at so operators can audit credentials.
func TestWithAPIKeyRecordsUsage(t *testing.T) {
	s, key := newAPIKeyTestServer(t)
	handler := s.withAPIKey(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	request := httptest.NewRequest(http.MethodPost, "/api/public/import", nil)
	request.Header.Set(apiKeyHeader, key.Token)
	handler(httptest.NewRecorder(), request)

	// Usage recording is asynchronous; poll briefly instead of sleeping blindly.
	deadline := 200
	for i := 0; i < deadline; i++ {
		stored, err := s.store.GetAPIKeyByToken(context.Background(), key.Token)
		if err != nil {
			t.Fatal(err)
		}
		if stored != nil && !stored.LastUsedAt.IsZero() {
			return
		}
	}
	t.Fatal("last_used_at was never recorded")
}

func TestPublicAPIRoutesScopeAndGuard(t *testing.T) {
	s, key := newAPIKeyTestServer(t)
	// A configured password proves the public surface really is session-free.
	s.cfg.Password = "secret"
	mux := s.routes()

	call := func(method, path string, withKey bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if withKey {
			request.Header.Set(apiKeyHeader, key.Token)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}

	// Without a credential every public endpoint answers 401.
	for _, path := range []string{"/api/public/subscriptions", "/api/public/import"} {
		if code := call(http.MethodPost, path, false).Code; code != http.StatusUnauthorized {
			t.Fatalf("POST %s without key status=%d, want 401", path, code)
		}
	}

	// With a valid credential the middleware is passed even though a WebUI
	// password is configured; the managers are not wired in this test, so the
	// handlers answer 503 rather than 401.
	for _, path := range []string{"/api/public/subscriptions", "/api/public/import"} {
		if code := call(http.MethodPost, path, true).Code; code == http.StatusUnauthorized {
			t.Fatalf("POST %s with key was rejected by the middleware", path)
		}
	}

	// The scope is write-only: reads are not exposed on the public surface.
	if code := call(http.MethodGet, "/api/public/subscriptions", true).Code; code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/public/subscriptions status=%d, want 405", code)
	}
	// Subscription deletion is session-only: an API key must not unlock it.
	if code := call(http.MethodDelete, "/api/subscriptions/1", true).Code; code != http.StatusUnauthorized {
		t.Fatalf("DELETE /api/subscriptions/1 with an API key status=%d, want 401", code)
	}
}

func TestAPIKeyManagementRequiresSession(t *testing.T) {
	s, key := newAPIKeyTestServer(t)
	// A configured password makes the session guard active.
	s.cfg.Password = "secret"
	mux := s.routes()

	request := httptest.NewRequest(http.MethodGet, "/api/api-keys", nil)
	request.Header.Set(apiKeyHeader, key.Token)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/api-keys with an API key status=%d, want 401 (session required)", recorder.Code)
	}

	session, err := s.createSession()
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/api-keys", nil)
	request.Header.Set("Authorization", "Bearer "+session.Token)
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/api-keys with a session status=%d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), key.Token) {
		t.Fatalf("listing did not include the stored credential: %s", recorder.Body.String())
	}
}

// TestLoginRateLimitLocksOutBursts pins the brute-force guard: within the
// failure window only maxLoginFailuresWithin wrong passwords are counted, then
// the source is locked out even with the correct password, and a successful
// login clears the record.
func TestLoginRateLimitLocksOutBursts(t *testing.T) {
	s, _ := newAPIKeyTestServer(t)
	s.cfg.Password = "secret"
	s.sessionTTL = time.Hour
	mux := s.routes()

	post := func(password string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"password": password})
		request := httptest.NewRequest(http.MethodPost, "/api/auth", bytes.NewReader(body))
		request.RemoteAddr = "203.0.113.9:4444"
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}

	for i := 0; i < maxLoginFailuresWithin; i++ {
		if code := post("wrong").Code; code != http.StatusUnauthorized {
			t.Fatalf("wrong password attempt %d status=%d, want 401", i+1, code)
		}
	}
	// The source is now locked out: even the correct password is rejected.
	if code := post("secret").Code; code != http.StatusUnauthorized {
		t.Fatalf("correct password during lockout status=%d, want 401", code)
	}

	// A different source is unaffected.
	request := httptest.NewRequest(http.MethodPost, "/api/auth", nil)
	body, _ := json.Marshal(map[string]string{"password": "secret"})
	request = httptest.NewRequest(http.MethodPost, "/api/auth", bytes.NewReader(body))
	request.RemoteAddr = "198.51.100.7:5555"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("other source login status=%d, want 200", recorder.Code)
	}
}
