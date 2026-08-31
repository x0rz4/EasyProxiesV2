package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestManagementProbeSettingsDefaultsAndExplicitZeroRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Management.ProbeConcurrency != 0 || cfg.Management.StartupProbeTimeout != 5*time.Second ||
		cfg.Management.RoutineProbeTimeout != 10*time.Second || cfg.Management.ProbeDialTimeout != 3*time.Second ||
		cfg.Management.ProbeResponseTimeout != 2*time.Second || cfg.RoutineProbeRetryCount() != 1 ||
		cfg.Management.StartupAvailabilityPolicy != StartupAvailabilityOptimistic {
		t.Fatalf("unexpected defaults: %+v", cfg.Management)
	}

	if err := os.WriteFile(path, []byte("management:\n  routine_probe_retries: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Management.RoutineProbeRetries == nil || cfg.RoutineProbeRetryCount() != 0 {
		t.Fatalf("explicit zero retry was not preserved: %+v", cfg.Management.RoutineProbeRetries)
	}
}

func TestGeoIPAndSpeedDefaultsUseLocalDatabaseAndTenMegabyteTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GeoIP.DatabasePath != filepath.Join(dir, "GeoLite2-Country.mmdb") {
		t.Fatalf("GeoIP path = %q", cfg.GeoIP.DatabasePath)
	}
	check := cfg.Management.NodeCheck
	if check.SpeedURL != DefaultNodeCheckSpeedURL || check.MaxDownloadBytes != DefaultNodeCheckMaxDownloadBytes {
		t.Fatalf("node-check defaults = %+v", check)
	}
}

func TestLegacyCloudflareSpeedDefaultsAreMigratedWithoutChangingCustomTargets(t *testing.T) {
	legacy := NodeCheckConfig{SpeedURL: LegacyNodeCheckSpeedURL, MaxDownloadBytes: 100_000_000}
	NormalizeNodeCheckSpeedSettings(&legacy)
	if legacy.SpeedURL != DefaultNodeCheckSpeedURL || legacy.MaxDownloadBytes != DefaultNodeCheckMaxDownloadBytes {
		t.Fatalf("legacy settings not migrated: %+v", legacy)
	}

	custom := NodeCheckConfig{SpeedURL: "https://speed.example/download?bytes=100000000", MaxDownloadBytes: 100_000_000}
	NormalizeNodeCheckSpeedSettings(&custom)
	if custom.SpeedURL != "https://speed.example/download?bytes=100000000" || custom.MaxDownloadBytes != 100_000_000 {
		t.Fatalf("custom settings changed: %+v", custom)
	}
}

func TestManagementProbeSettingsCloneAndSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("management:\n  probe_target: example.com:80\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	retries := 2
	cfg.Management.ProbeConcurrency = 64
	cfg.Management.StartupAvailabilityPolicy = StartupAvailabilityStrict
	cfg.Management.StartupProbeTimeout = 6 * time.Second
	cfg.Management.RoutineProbeTimeout = 18 * time.Second
	cfg.Management.ProbeDialTimeout = 3 * time.Second
	cfg.Management.ProbeResponseTimeout = 3 * time.Second
	cfg.Management.RoutineProbeRetries = &retries
	clone := cfg.Clone()
	*clone.Management.RoutineProbeRetries = 1
	if cfg.RoutineProbeRetryCount() != 2 {
		t.Fatal("clone shared the retry pointer with its source")
	}
	if err := cfg.SaveSettings(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Management.ProbeConcurrency != 64 || reloaded.Management.RoutineProbeTimeout != 18*time.Second || reloaded.RoutineProbeRetryCount() != 2 || reloaded.Management.StartupAvailabilityPolicy != StartupAvailabilityStrict {
		t.Fatalf("saved probe settings = %+v", reloaded.Management)
	}
}

func TestManagementRejectsUnknownStartupAvailabilityPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("management:\n  startup_availability_policy: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadForReload(path); err == nil || !strings.Contains(err.Error(), "startup_availability_policy") {
		t.Fatalf("unknown policy error = %v", err)
	}
}

func TestValidateManagementProbeSettings(t *testing.T) {
	valid := func(concurrency, retries int, startup, routine, dial, response time.Duration) error {
		return ValidateManagementProbeSettings(concurrency, startup, routine, dial, response, retries)
	}
	if err := valid(0, 1, 5*time.Second, 10*time.Second, 3*time.Second, 2*time.Second); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	for name, err := range map[string]error{
		"concurrency": valid(513, 1, 5*time.Second, 10*time.Second, 3*time.Second, 2*time.Second),
		"retries":     valid(32, 3, 5*time.Second, 15*time.Second, 3*time.Second, 2*time.Second),
		"startup":     valid(32, 1, 0, 10*time.Second, 3*time.Second, 2*time.Second),
		"routine":     valid(32, 2, 5*time.Second, 0, 3*time.Second, 2*time.Second),
	} {
		if err == nil {
			t.Fatalf("%s validation accepted an invalid value", name)
		}
	}
}

func TestIsProxyURIRecognizesHTTPAndSOCKS5(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{name: "http", uri: "http://alice:secret@example.com:8080", want: true},
		{name: "socks5", uri: "socks5://alice:secret@example.com:1080", want: true},
		{name: "vmess", uri: "vmess://example", want: true},
		{name: "invalid", uri: "ftp://example.com", want: false},
	}

	for _, tt := range tests {
		if got := IsProxyURI(tt.uri); got != tt.want {
			t.Fatalf("%s: IsProxyURI(%q) = %v, want %v", tt.name, tt.uri, got, tt.want)
		}
	}
}

func TestEntrySwitchesDefaultOffAndIgnoreLegacyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: hybrid\nlog_level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listener.Enabled || cfg.MultiPort.Enabled || cfg.EntryMode() != "disabled" {
		t.Fatalf("legacy mode enabled an entry: listener=%v multi=%v mode=%q", cfg.Listener.Enabled, cfg.MultiPort.Enabled, cfg.EntryMode())
	}
	if err := cfg.SaveSettings(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["mode"]; exists {
		t.Fatalf("legacy top-level mode remained after save:\n%s", data)
	}
}

func TestNormalizeAllocatesPortsOnlyWhenMultiPortEnabled(t *testing.T) {
	makeNodes := func(count int) []NodeConfig {
		nodes := make([]NodeConfig, count)
		for i := range nodes {
			nodes[i] = NodeConfig{Name: fmt.Sprintf("node-%d", i), URI: fmt.Sprintf("http://node-%d.example:80", i)}
		}
		return nodes
	}

	disabled := &Config{Nodes: makeNodes(500)}
	if err := disabled.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	for _, node := range disabled.Nodes {
		if node.Port != 0 {
			t.Fatalf("disabled multi-port assigned port %d to %q", node.Port, node.Name)
		}
	}

	enabled := &Config{MultiPort: MultiPortConfig{Enabled: true, Address: "127.0.0.1", BasePort: 30000}, Nodes: makeNodes(3)}
	if err := enabled.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	seen := map[uint16]bool{}
	for _, node := range enabled.Nodes {
		if node.Port == 0 || seen[node.Port] {
			t.Fatalf("invalid allocated port %d for %q", node.Port, node.Name)
		}
		seen[node.Port] = true
	}

	retained := &Config{Nodes: []NodeConfig{{Name: "retained", URI: "http://retained.example:80", Port: 31000}}}
	if err := retained.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	if retained.Nodes[0].Port != 31000 || !strings.Contains(retained.EntryMode(), "disabled") {
		t.Fatalf("disabled normalization changed retained node: %+v", retained.Nodes[0])
	}
	retained.MultiPort = MultiPortConfig{Enabled: true, Address: "127.0.0.1", BasePort: 32000, Username: "default-user", Password: "default-password"}
	if err := retained.NormalizeWithPortMap(retained.BuildPortMap()); err != nil {
		t.Fatal(err)
	}
	if retained.Nodes[0].Port != 31000 {
		t.Fatalf("re-enabling multi-port replaced retained port with %d", retained.Nodes[0].Port)
	}
	if retained.Nodes[0].Username != "default-user" || retained.Nodes[0].Password != "default-password" {
		t.Fatalf("re-enabling multi-port did not apply default credentials: %+v", retained.Nodes[0])
	}
}

func TestNormalizeRejectsExhaustedMultiPortRangeWithoutWrapping(t *testing.T) {
	cfg := &Config{
		MultiPort: MultiPortConfig{Enabled: true, Address: "127.0.0.1", BasePort: 65535},
		Nodes: []NodeConfig{
			{Name: "first", URI: "http://first.example:80"},
			{Name: "second", URI: "http://second.example:80"},
		},
	}
	if err := cfg.NormalizeWithPortMap(nil); err == nil || !strings.Contains(err.Error(), "no available ports") {
		t.Fatalf("NormalizeWithPortMap error=%v, want exhausted port range", err)
	}
}
