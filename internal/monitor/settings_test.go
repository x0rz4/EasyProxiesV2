package monitor

import (
	"testing"
	"time"

	"easy_proxies/internal/config"
)

func testSettingsConfig() *config.Config {
	enabled := true
	cfg := &config.Config{Mode: "pool", ExternalIP: "old"}
	cfg.Management.Enabled = &enabled
	cfg.Management.Listen = "127.0.0.1:9090"
	cfg.Management.Password = "old-password"
	cfg.Management.ProbeTarget = "http://example.com"
	cfg.Management.HealthCheckInterval = time.Minute
	return cfg
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
	if err := mgr.UpdateProbeTarget(""); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.DestinationForProbe(); ok {
		t.Fatal("probe destination remained ready after clearing target")
	}
}
