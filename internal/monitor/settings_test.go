package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
)

func testSettingsConfig() *config.Config {
	enabled := true
	cfg := &config.Config{ExternalIP: "old"}
	cfg.Management.Enabled = &enabled
	cfg.Management.Listen = "127.0.0.1:9090"
	cfg.Management.Password = "old-password"
	cfg.Management.ProbeTarget = "http://example.com"
	cfg.Management.HealthCheckInterval = time.Minute
	return cfg
}

func TestSettingsEntrySwitchRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: hybrid\nlog_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	server := &Server{cfgSrc: cfg}
	request := allSettingsRequest{
		LogLevel: "info", ListenerEnabled: true, ListenerAddress: "127.0.0.1", ListenerPort: 2323, ListenerProtocol: "http",
		MultiPortEnabled: false, MultiPortAddress: "127.0.0.1", MultiPortBasePort: 24000, MultiPortProtocol: "mixed",
		PoolMode: "sequential", PoolFailureThreshold: 3, PoolBlacklistDuration: "24h",
		ManagementEnabled: &enabled, ManagementListen: "127.0.0.1:9090", ManagementHealthCheckInterval: "2h",
		SubRefreshInterval: "1h", SubRefreshTimeout: "30s", SubRefreshHealthCheckTimeout: "1m", SubRefreshDrainTimeout: "30s",
		SubRefreshMinAvailableNodes: 1, GeoIPAutoUpdateInterval: "24h",
	}
	invalidRequest := request
	invalidRequest.ManagementProbeTarget = "ftp://example.com/check"
	if _, err := server.updateAllSettings(t.Context(), invalidRequest); err == nil {
		t.Fatal("settings API accepted an invalid probe target")
	}
	if _, err := server.updateAllSettings(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	got := server.getAllSettings()
	if !got.ListenerEnabled || got.MultiPortEnabled {
		t.Fatalf("settings switches=%+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || containsTopLevelMode(data) {
		t.Fatalf("saved settings retained legacy mode:\n%s", data)
	}
}

func containsTopLevelMode(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "mode:") {
			return true
		}
	}
	return false
}

func TestSettingsApplyPlan(t *testing.T) {
	tests := []struct {
		name        string
		change      func(*config.Config)
		needReload  bool
		needRestart bool
	}{
		{name: "dynamic fields", change: func(c *config.Config) {
			c.ExternalIP = "new"
			c.Management.Password = "new-password"
			c.Management.ProbeTarget = ""
			c.Management.HealthCheckInterval = 2 * time.Minute
		}},
		{name: "runtime field", change: func(c *config.Config) { c.SkipCertVerify = true }, needReload: true},
		{name: "listener switch", change: func(c *config.Config) { c.Listener.Enabled = true }, needReload: true},
		{name: "multi-port switch", change: func(c *config.Config) { c.MultiPort.Enabled = true }, needReload: true},
		{name: "subscription enabled", change: func(c *config.Config) { c.SubscriptionRefresh.Enabled = true }},
		{name: "management listen", change: func(c *config.Config) { c.Management.Listen = "127.0.0.1:9191" }, needRestart: true},
		{name: "management enabled", change: func(c *config.Config) {
			disabled := false
			c.Management.Enabled = &disabled
		}, needRestart: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := testSettingsConfig()
			updated := old.Clone()
			tt.change(updated)
			plan := settingsApplyPlan(old, updated)
			if plan.NeedReload != tt.needReload || plan.NeedRestart != tt.needRestart {
				t.Fatalf("plan=%+v, want reload=%v restart=%v", plan, tt.needReload, tt.needRestart)
			}
		})
	}
}

func TestUpdateProbeTargetClearAndHTTPSDefaultPort(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "http://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateProbeTarget("https://secure.example/path"); err != nil {
		t.Fatal(err)
	}
	dst, ok := mgr.DestinationForProbe()
	if !ok || dst.Port != 443 {
		t.Fatalf("HTTPS destination=%v ready=%v, want port 443", dst, ok)
	}
	target, ok := mgr.TargetForProbe()
	if !ok || target.Scheme != "https" || target.ServerName != "secure.example" || target.RequestURI != "/path" {
		t.Fatalf("HTTPS target=%+v ready=%v", target, ok)
	}
	if err := mgr.UpdateProbeTarget(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.DestinationForProbe(); ok {
		t.Fatal("probe destination remained ready after clearing target")
	}
}

func TestProbeTargetValidationAndDefaults(t *testing.T) {
	mgr, err := NewManager(Config{ProbeTarget: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	target, ok := mgr.TargetForProbe()
	if !ok || target.Scheme != "http" || target.Destination.Port != 80 || target.RequestURI != "/generate_204" {
		t.Fatalf("default target=%+v ready=%v", target, ok)
	}
	for _, invalid := range []string{"ftp://example.com/check", "http://example.com:0/check", "http://user:pass@example.com/check"} {
		if err := mgr.UpdateProbeTarget(invalid); err == nil {
			t.Fatalf("invalid target %q was accepted", invalid)
		}
	}
}
