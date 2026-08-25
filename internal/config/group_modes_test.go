package config

import "testing"

func TestApplyDefaultsNormalizesGroupDispatchModes(t *testing.T) {
	cfg := Config{Groups: []GroupPoolConfig{
		{Name: "fixed", DispatchMode: "fixed"},
		{Name: "lowest", DispatchMode: " LOWEST_LATENCY "},
		{Name: "random", DispatchMode: "RANDOM"},
		{Name: "fallback", DispatchMode: "unsupported"},
	}}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	want := []string{"fixed", "lowest_latency", "random", "fixed"}
	for index, group := range cfg.Groups {
		if group.DispatchMode != want[index] {
			t.Fatalf("group %q mode = %q, want %q", group.Name, group.DispatchMode, want[index])
		}
	}
}
