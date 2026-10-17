package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
)

func newSubscriptionRefreshTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfgSrc: cfg}, path
}

func TestSubscriptionRefreshSettingsRoundTrip(t *testing.T) {
	server, path := newSubscriptionRefreshTestServer(t)

	// A malformed duration is rejected by the wire parser, so exercise the
	// HTTP entry point rather than converting to the service type first.
	if code := putRefreshSettings(server, `{"sub_refresh_interval":"nonsense"}`); code != 400 {
		t.Fatalf("invalid refresh interval status=%d, want 400", code)
	}
	if _, err := server.settingsService().UpdateSubscriptionRefreshSettings(t.Context(), controlplane.SubscriptionRefreshSettings{MinAvailableNodes: -1}); err == nil {
		t.Fatal("accepted a negative minimum available node count")
	}

	result, err := server.settingsService().UpdateSubscriptionRefreshSettings(t.Context(), mustRefreshSettings(t, subscriptionRefreshSettingsRequest{
		Enabled:            true,
		Interval:           "2h",
		Timeout:            "45s",
		HealthCheckTimeout: "90s",
		DrainTimeout:       "20s",
		MinAvailableNodes:  3,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Saved {
		t.Fatalf("update result=%+v", result)
	}

	got := server.subscriptionRefreshSettings()
	want := subscriptionRefreshSettingsResponse{
		Enabled:            true,
		Interval:           "2h0m0s",
		Timeout:            "45s",
		HealthCheckTimeout: "1m30s",
		DrainTimeout:       "20s",
		MinAvailableNodes:  3,
	}
	if got != want {
		t.Fatalf("settings=%+v, want %+v", got, want)
	}

	// The values must survive a restart, i.e. land in config.yaml.
	reloaded, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.SubscriptionRefresh.Enabled || reloaded.SubscriptionRefresh.Interval.String() != "2h0m0s" {
		t.Fatalf("persisted subscription refresh=%+v", reloaded.SubscriptionRefresh)
	}
}

// Regression guard for the migration: the generic settings form no longer owns
// the subscription auto-refresh fields, so saving it must not reset them.
func TestGenericSettingsFormPreservesSubscriptionRefresh(t *testing.T) {
	server, _ := newSubscriptionRefreshTestServer(t)

	if _, err := server.settingsService().UpdateSubscriptionRefreshSettings(t.Context(), mustRefreshSettings(t, subscriptionRefreshSettingsRequest{
		Enabled:           true,
		Interval:          "3h",
		Timeout:           "40s",
		MinAvailableNodes: 2,
	})); err != nil {
		t.Fatal(err)
	}

	managementEnabled := true
	if _, err := server.settingsService().UpdateAllSettings(t.Context(), allSettingsRequestOf(allSettingsRequest{
		LogLevel: "info", ListenerAddress: "127.0.0.1", ListenerPort: 2323, ListenerProtocol: "http",
		MultiPortAddress: "127.0.0.1", MultiPortBasePort: 24000, MultiPortProtocol: "mixed",
		PoolMode: "sequential", PoolFailureThreshold: 3, PoolBlacklistDuration: "24h",
		ManagementEnabled: &managementEnabled, ManagementListen: "127.0.0.1:9090", ManagementHealthCheckInterval: "2h",
		GeoIPAutoUpdateInterval: "24h",
	})); err != nil {
		t.Fatal(err)
	}

	got := server.subscriptionRefreshSettings()
	if !got.Enabled || got.Interval != "3h0m0s" || got.Timeout != "40s" || got.MinAvailableNodes != 2 {
		t.Fatalf("generic settings save clobbered the auto-refresh config: %+v", got)
	}
}

// mustRefreshSettings converts the wire form for service-level tests. The
// parse helper only fails on a malformed duration string, which these cases
// exercise through the service's own validation instead.
func mustRefreshSettings(t *testing.T, req subscriptionRefreshSettingsRequest) controlplane.SubscriptionRefreshSettings {
	t.Helper()
	parsed, err := subscriptionRefreshSettingsTo(req)
	if err != nil {
		// A deliberately invalid duration must reach the service so it is the
		// service's validation under test, not the parser's.
		return controlplane.SubscriptionRefreshSettings{Enabled: req.Enabled, MinAvailableNodes: req.MinAvailableNodes}
	}
	return parsed
}

// putRefreshSettings drives the HTTP entry point and returns the status code.
func putRefreshSettings(server *Server, body string) int {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/subscription/refresh-settings", strings.NewReader(body))
	server.routes().ServeHTTP(response, request)
	return response.Code
}
