package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

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
