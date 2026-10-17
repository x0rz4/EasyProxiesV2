package controlplane

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
)

type settingsFixture struct {
	svc     *Settings
	path    string
	current *config.Config
	logger  *log.Logger
}

func newSettingsFixture(t *testing.T) *settingsFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &settingsFixture{path: path, current: cfg, logger: log.New(io.Discard, "", 0)}
	f.svc = NewSettings(SettingsOptions{
		Logger: f.logger,
		Config: func() *config.Config { return f.current },
		Commit: func(updated *config.Config) { f.current = updated },
	})
	return f
}

// validAllSettings returns a request the validator accepts. The generic form
// requires a valid inbound protocol for both listeners, so every case that is
// not about rejection must start from this.
func validAllSettings() AllSettingsRequest {
	return AllSettingsRequest{
		LogLevel:        "info",
		ListenerEnabled: true, ListenerAddress: "127.0.0.1", ListenerPort: 2323, ListenerProtocol: "http",
		MultiPortEnabled: false, MultiPortAddress: "127.0.0.1", MultiPortBasePort: 24000, MultiPortProtocol: "mixed",
		PoolMode: "sequential", PoolFailureThreshold: 3, PoolBlacklistDuration: "24h",
		GeoIPAutoUpdateInterval: "24h", ManagementHealthCheckInterval: "2h",
		// The loaded config already defaults these; the generic form writes them
		// back verbatim, so leaving them empty would look like a listener change.
		ManagementListen: "127.0.0.1:9090",
	}
}

func TestSettingsSnapshotIsAnIndependentCopy(t *testing.T) {
	f := newSettingsFixture(t)
	snapshot := f.svc.Snapshot()
	if snapshot == nil {
		t.Fatal("snapshot must not be nil when a config is bound")
	}
	snapshot.LogLevel = "debug"
	if f.svc.Snapshot().LogLevel == "debug" {
		t.Fatal("mutating a snapshot must not affect the bound config")
	}
}

func TestSettingsWithoutConfigReportsUnavailable(t *testing.T) {
	svc := NewSettings(SettingsOptions{Config: func() *config.Config { return nil }})
	if _, err := svc.AllSettings(); !errors.Is(err, ErrConfigUnavailable) {
		t.Fatalf("AllSettings error = %v, want ErrConfigUnavailable", err)
	}
	if _, err := svc.ProbeSettings(); !errors.Is(err, ErrConfigUnavailable) {
		t.Fatalf("ProbeSettings error = %v, want ErrConfigUnavailable", err)
	}
	if _, err := svc.SubscriptionRefreshSettings(); !errors.Is(err, ErrConfigUnavailable) {
		t.Fatalf("SubscriptionRefreshSettings error = %v, want ErrConfigUnavailable", err)
	}
	if _, err := svc.UpdateAllSettings(context.Background(), validAllSettings()); !errors.Is(err, ErrConfigUnavailable) {
		t.Fatalf("UpdateAllSettings error = %v, want ErrConfigUnavailable", err)
	}
}

// A rejected request must not touch the file: validation happens before any
// write, so an invalid form leaves the persisted document byte-identical.
func TestUpdateAllSettingsRejectsInvalidInputWithoutWriting(t *testing.T) {
	f := newSettingsFixture(t)
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.UpdateAllSettings(context.Background(), AllSettingsRequest{ManagementProbeTarget: "ftp://example.com/check"})
	var validation ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a rejected request modified config.yaml")
	}
}

func TestUpdateAllSettingsPersistsAndPublishes(t *testing.T) {
	f := newSettingsFixture(t)
	enabled := true
	req := validAllSettings()
	req.LogLevel, req.ExternalIP = "warn", "203.0.113.7"
	req.ManagementEnabled, req.ManagementListen = &enabled, "127.0.0.1:9090"
	result, err := f.svc.UpdateAllSettings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Saved {
		t.Fatalf("result = %+v", result)
	}
	if f.current.LogLevel != "warn" || f.current.ExternalIP != "203.0.113.7" {
		t.Fatalf("published config = %+v", f.current)
	}
	reloaded, err := config.LoadForReload(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LogLevel != "warn" || reloaded.ExternalIP != "203.0.113.7" {
		t.Fatalf("persisted config = log_level %q external_ip %q", reloaded.LogLevel, reloaded.ExternalIP)
	}
}

// The generic form must never clobber the auto-refresh block, which has its own
// endpoint, nor the legacy subscriptions list.
func TestUpdateAllSettingsPreservesUnownedBlocks(t *testing.T) {
	f := newSettingsFixture(t)
	if _, err := f.svc.UpdateSubscriptionRefreshSettings(context.Background(), SubscriptionRefreshSettings{
		Enabled: true, Interval: 3 * time.Hour, Timeout: 40 * time.Second, MinAvailableNodes: 2,
	}); err != nil {
		t.Fatal(err)
	}
	before := f.svc.Snapshot().Subscriptions

	if _, err := f.svc.UpdateAllSettings(context.Background(), validAllSettings()); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.SubscriptionRefreshSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Interval != 3*time.Hour || got.Timeout != 40*time.Second || got.MinAvailableNodes != 2 {
		t.Fatalf("generic save clobbered auto-refresh: %+v", got)
	}
	if after := f.svc.Snapshot().Subscriptions; len(after) != len(before) {
		t.Fatalf("generic save clobbered subscriptions: %v -> %v", before, after)
	}
}

// An omitted duration means "keep the stored value"; validating the raw zeroes
// would reject it as 0s.
func TestUpdateSubscriptionRefreshKeepsOmittedDurations(t *testing.T) {
	f := newSettingsFixture(t)
	if _, err := f.svc.UpdateSubscriptionRefreshSettings(context.Background(), SubscriptionRefreshSettings{
		Enabled: true, Interval: 2 * time.Hour, Timeout: 45 * time.Second,
		HealthCheckTimeout: 90 * time.Second, DrainTimeout: 20 * time.Second, MinAvailableNodes: 3,
	}); err != nil {
		t.Fatal(err)
	}
	// Only the flag changes; every duration is zero and must survive.
	if _, err := f.svc.UpdateSubscriptionRefreshSettings(context.Background(), SubscriptionRefreshSettings{Enabled: false}); err != nil {
		t.Fatalf("omitting durations must be accepted: %v", err)
	}
	got, err := f.svc.SubscriptionRefreshSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("the flag change did not apply")
	}
	if got.Interval != 2*time.Hour || got.Timeout != 45*time.Second ||
		got.HealthCheckTimeout != 90*time.Second || got.DrainTimeout != 20*time.Second {
		t.Fatalf("omitted durations were reset: %+v", got)
	}
}

func TestUpdateSubscriptionRefreshRejectsNegativeMinimum(t *testing.T) {
	f := newSettingsFixture(t)
	_, err := f.svc.UpdateSubscriptionRefreshSettings(context.Background(), SubscriptionRefreshSettings{MinAvailableNodes: -1})
	var validation ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
}

// A reload failure is reported, not hidden: the caller must be able to tell the
// operator that the file is saved but the runtime is not serving it.
func TestUpdateAllSettingsReportsReloadFailure(t *testing.T) {
	f := newSettingsFixture(t)
	f.svc.reloader = failingReloader{}
	req := validAllSettings()
	req.ConfigurationOnlyReloadForTest()
	result, err := f.svc.UpdateAllSettings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.NeedReload || result.Reloaded {
		t.Fatalf("result = %+v, want NeedReload and not Reloaded", result)
	}
	if result.ReloadError == "" {
		t.Fatal("a failed reload must be reported")
	}
	if !strings.Contains(result.Message, "重载未完成") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestUpdateAllSettingsMarksRestartForManagementListener(t *testing.T) {
	f := newSettingsFixture(t)
	req := validAllSettings()
	req.ManagementListen = "127.0.0.1:9999"
	result, err := f.svc.UpdateAllSettings(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.NeedRestart {
		t.Fatalf("listener change must require a restart: %+v", result)
	}
	if !strings.Contains(result.Message, "重启进程") {
		t.Fatalf("message = %q", result.Message)
	}
}

// Mutate serialises the read-modify-write cycle and only commits on success.
func TestMutateDoesNotCommitOnFailure(t *testing.T) {
	f := newSettingsFixture(t)
	original := f.current
	_, err := f.svc.Mutate(context.Background(), func(*config.Config) error {
		return errors.New("nope")
	})
	if err == nil {
		t.Fatal("expected the callback error")
	}
	if f.current != original {
		t.Fatal("a failed mutation must not publish a new config")
	}

	updated, err := f.svc.Mutate(context.Background(), func(c *config.Config) error {
		c.LogLevel = "error"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.LogLevel != "error" || f.current.LogLevel != "error" {
		t.Fatalf("mutation not published: %+v", f.current)
	}
}

// ConfigurationOnlyReloadForTest sets a runtime-only field so the plan forces a
// reload without changing the listener shape.
func (r *AllSettingsRequest) ConfigurationOnlyReloadForTest() { r.SkipCertVerify = true }

func TestSettingsApplyPlanClassification(t *testing.T) {
	base := func() *config.Config {
		enabled := true
		cfg := &config.Config{ExternalIP: "old"}
		cfg.Management.Enabled = &enabled
		cfg.Management.Listen = "127.0.0.1:9090"
		cfg.Management.Password = "old"
		cfg.Management.ProbeTarget = "http://example.com"
		cfg.Management.HealthCheckInterval = time.Minute
		return cfg
	}
	tests := []struct {
		name        string
		change      func(*config.Config)
		needReload  bool
		needRestart bool
		applied     string
	}{
		{name: "password", change: func(c *config.Config) { c.Management.Password = "new" }, applied: "management_password"},
		{name: "probe target", change: func(c *config.Config) { c.Management.ProbeTarget = "https://x.example" }, applied: "management_probe_target"},
		{name: "external ip", change: func(c *config.Config) { c.ExternalIP = "new" }, applied: "external_ip"},
		{name: "skip cert verify", change: func(c *config.Config) { c.SkipCertVerify = true }, needReload: true},
		{name: "listener", change: func(c *config.Config) { c.Listener.Enabled = true }, needReload: true},
		{name: "management listen", change: func(c *config.Config) { c.Management.Listen = "127.0.0.1:9191" }, needRestart: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := base()
			updated := old.Clone()
			tt.change(updated)
			plan := settingsApplyPlan(old, updated)
			if plan.NeedReload != tt.needReload || plan.NeedRestart != tt.needRestart {
				t.Fatalf("plan = %+v, want reload=%v restart=%v", plan, tt.needReload, tt.needRestart)
			}
			if tt.applied != "" {
				found := false
				for _, name := range plan.Applied {
					if name == tt.applied {
						found = true
					}
				}
				if !found {
					t.Fatalf("applied = %v, want %q", plan.Applied, tt.applied)
				}
			}
		})
	}
}

type failingReloader struct{}

func (failingReloader) TriggerReload(context.Context) error { return errors.New("runtime busy") }
