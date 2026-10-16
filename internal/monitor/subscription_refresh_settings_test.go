package monitor

import (
	"os"
	"path/filepath"
	"testing"

	"easy_proxies/internal/config"
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

	if _, err := server.updateSubscriptionRefreshSettings(t.Context(), subscriptionRefreshSettingsRequest{Interval: "nonsense"}); err == nil {
		t.Fatal("accepted an invalid refresh interval")
	}
	if _, err := server.updateSubscriptionRefreshSettings(t.Context(), subscriptionRefreshSettingsRequest{MinAvailableNodes: -1}); err == nil {
		t.Fatal("accepted a negative minimum available node count")
	}

	result, err := server.updateSubscriptionRefreshSettings(t.Context(), subscriptionRefreshSettingsRequest{
		Enabled:            true,
		Interval:           "2h",
		Timeout:            "45s",
		HealthCheckTimeout: "90s",
		DrainTimeout:       "20s",
		MinAvailableNodes:  3,
	})
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

	if _, err := server.updateSubscriptionRefreshSettings(t.Context(), subscriptionRefreshSettingsRequest{
		Enabled:           true,
		Interval:          "3h",
		Timeout:           "40s",
		MinAvailableNodes: 2,
	}); err != nil {
		t.Fatal(err)
	}

	managementEnabled := true
	if _, err := server.updateAllSettings(t.Context(), allSettingsRequest{
		LogLevel: "info", ListenerAddress: "127.0.0.1", ListenerPort: 2323, ListenerProtocol: "http",
		MultiPortAddress: "127.0.0.1", MultiPortBasePort: 24000, MultiPortProtocol: "mixed",
		PoolMode: "sequential", PoolFailureThreshold: 3, PoolBlacklistDuration: "24h",
		ManagementEnabled: &managementEnabled, ManagementListen: "127.0.0.1:9090", ManagementHealthCheckInterval: "2h",
		GeoIPAutoUpdateInterval: "24h",
	}); err != nil {
		t.Fatal(err)
	}

	got := server.subscriptionRefreshSettings()
	if !got.Enabled || got.Interval != "3h0m0s" || got.Timeout != "40s" || got.MinAvailableNodes != 2 {
		t.Fatalf("generic settings save clobbered the auto-refresh config: %+v", got)
	}
}
