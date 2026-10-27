package httpapi

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easy_proxies/internal/controlplane"
	json "easy_proxies/internal/jsonx"
)

func TestBrowserSessionStatusAndLogoutRevokeCookieAndBearer(t *testing.T) {
	s, db := newTagAPITestServer(t)
	s.cfg.Password = "synthetic-password"
	handler := s.routes()
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest("POST", "/api/auth", strings.NewReader(`{"password":"synthetic-password"}`)))
	if login.Code != 200 {
		t.Fatalf("login=%d %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &payload); err != nil || payload.Token == "" {
		t.Fatal("missing session")
	}
	other, err := s.authService().CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/auth", nil)
	request.AddCookie(cookies[0])
	handler.ServeHTTP(status, request)
	if !strings.Contains(status.Body.String(), `"authenticated":true`) || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%s", status.Body.String())
	}
	logout := httptest.NewRecorder()
	request = httptest.NewRequest("POST", "/api/auth/logout", nil)
	request.AddCookie(cookies[0])
	request.Header.Set("Authorization", "Bearer "+other.Token)
	handler.ServeHTTP(logout, request)
	if logout.Code != 200 {
		t.Fatalf("logout=%d %s", logout.Code, logout.Body.String())
	}
	if cleared := logout.Result().Cookies(); len(cleared) != 1 || cleared[0].MaxAge >= 0 || !cleared[0].HttpOnly {
		t.Fatal("cookie not cleared")
	}
	for _, token := range []string{payload.Token, other.Token} {
		denied := httptest.NewRecorder()
		request = httptest.NewRequest("GET", "/api/settings", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(denied, request)
		if denied.Code != 401 {
			t.Fatal("revoked session can still read settings")
		}
		restarted := controlplane.NewAuth(controlplane.AuthOptions{Store: db})
		if restarted.Validate(context.Background(), token) {
			t.Fatal("session survived restart")
		}
	}
}

func TestFrontendStaticBoundariesAndSensitiveResponseCaching(t *testing.T) {
	s, _ := newTagAPITestServer(t)
	handler := s.routes()
	for _, path := range []string{"/", "/index.html", "/unknown-spa-route"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("HTML %s status=%d", path, response.Code)
		}
		policy := response.Header().Get("Content-Security-Policy")
		for _, directive := range []string{"script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "font-src 'self'"} {
			if !strings.Contains(policy, directive) {
				t.Fatalf("missing policy %s", directive)
			}
		}
		if response.Header().Get("X-Frame-Options") != "DENY" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing static protection")
		}
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest("GET", "/assets/missing.js", nil))
	if missing.Code != 404 {
		t.Fatal("missing script returned SPA HTML")
	}
	assets, err := fs.Glob(embeddedFS, "assets/assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatal("build frontend before this test")
	}
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest("GET", strings.TrimPrefix(assets[0], "assets"), nil))
	if asset.Code != 200 || asset.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("hashed asset not cacheable")
	}
	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest("GET", "/api/tags", nil))
	if api.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive API response can be stored")
	}
}
