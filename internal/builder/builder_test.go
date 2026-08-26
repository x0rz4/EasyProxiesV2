package builder

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

func TestBuildBaseEntrySwitchCombinations(t *testing.T) {
	tests := []struct {
		name              string
		listener, multi   bool
		wantInbounds      int
		wantDerivedMode   string
		wantPoolOutbounds int
		wantRouteRules    int
		wantFinal         string
	}{
		{name: "disabled", wantDerivedMode: "disabled", wantPoolOutbounds: 1, wantFinal: poolout.Tag},
		{name: "pool", listener: true, wantInbounds: 1, wantDerivedMode: "pool", wantPoolOutbounds: 1, wantFinal: poolout.Tag},
		{name: "multi-port", multi: true, wantInbounds: 1, wantDerivedMode: "multi-port", wantPoolOutbounds: 1, wantRouteRules: 1},
		{name: "hybrid", listener: true, multi: true, wantInbounds: 2, wantDerivedMode: "hybrid", wantPoolOutbounds: 2, wantRouteRules: 1, wantFinal: poolout.Tag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Listener:  config.ListenerConfig{Enabled: tt.listener, Address: "127.0.0.1", Port: 2323, Protocol: "http"},
				MultiPort: config.MultiPortConfig{Enabled: tt.multi, Address: "127.0.0.1", BasePort: 24000, Protocol: "http"},
				Pool:      config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Hour},
				Nodes:     []config.NodeConfig{{Name: "node", URI: "http://node.example:80", Port: 24000}},
			}
			opts, err := BuildBase(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(opts.Inbounds) != tt.wantInbounds {
				t.Fatalf("inbounds=%d, want %d", len(opts.Inbounds), tt.wantInbounds)
			}
			poolCount := 0
			for _, outbound := range opts.Outbounds {
				if outbound.Type == poolout.Type {
					poolCount++
					poolOptions := outbound.Options.(*poolout.Options)
					for _, meta := range poolOptions.Metadata {
						if meta.Mode != tt.wantDerivedMode {
							t.Fatalf("monitor metadata mode=%q, want %q", meta.Mode, tt.wantDerivedMode)
						}
					}
				}
			}
			if poolCount != tt.wantPoolOutbounds {
				t.Fatalf("pool outbounds=%d, want %d", poolCount, tt.wantPoolOutbounds)
			}
			if got := cfg.EntryMode(); got != tt.wantDerivedMode {
				t.Fatalf("derived mode=%q, want %q", got, tt.wantDerivedMode)
			}
			if opts.Route == nil || len(opts.Route.Rules) != tt.wantRouteRules || opts.Route.Final != tt.wantFinal {
				t.Fatalf("route=%+v, want rules=%d final=%q", opts.Route, tt.wantRouteRules, tt.wantFinal)
			}
		})
	}
}

func TestBuildBaseWithManyNodesAndEntriesDisabledHasOnlySharedMonitoringPool(t *testing.T) {
	nodes := make([]config.NodeConfig, 500)
	for i := range nodes {
		nodes[i] = config.NodeConfig{Name: fmt.Sprintf("node-%d", i), URI: fmt.Sprintf("http://node-%d.example:80", i)}
	}
	opts, err := BuildBase(&config.Config{Pool: config.PoolConfig{Mode: "sequential"}, Nodes: nodes})
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.Inbounds) != 0 || len(opts.Outbounds) != len(nodes)+1 {
		t.Fatalf("disabled topology: inbounds=%d outbounds=%d, want 0/%d", len(opts.Inbounds), len(opts.Outbounds), len(nodes)+1)
	}
	poolCount := 0
	for _, outbound := range opts.Outbounds {
		if outbound.Type == poolout.Type {
			poolCount++
			if outbound.Tag != poolout.Tag {
				t.Fatalf("disabled topology contains per-node pool %q", outbound.Tag)
			}
		}
	}
	if poolCount != 1 {
		t.Fatalf("disabled topology pool outbounds=%d, want one shared monitoring pool", poolCount)
	}
}

func TestBuildShadowsocksFullSIP002URI(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret@example.com:8388"))
	outbound, err := buildNodeOutbound("ss-full", "ss://"+payload+"#node", false)
	if err != nil {
		t.Fatal(err)
	}
	opts := outbound.Options.(*option.ShadowsocksOutboundOptions)
	if opts.Server != "example.com" || opts.ServerPort != 8388 || opts.Method != "aes-256-gcm" || opts.Password != "secret" {
		t.Fatalf("options=%+v", opts)
	}
}

func TestBuildVMessLegacyURIWithFragment(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"v": "2", "add": "example.com", "port": "443", "id": "abc", "aid": "0", "net": "tcp"})
	_, err := buildNodeOutbound("vmess-fragment", "vmess://"+base64.StdEncoding.EncodeToString(payload)+"#node", false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildNodeOutboundSupportsSOCKS5(t *testing.T) {
	outbound, err := buildNodeOutbound("socks-node", "socks5://demo:secret@99.144.123.135:30350", false)
	if err != nil {
		t.Fatalf("buildNodeOutbound returned error: %v", err)
	}
	if outbound.Type != C.TypeSOCKS {
		t.Fatalf("outbound type = %q, want %q", outbound.Type, C.TypeSOCKS)
	}

	opts, ok := outbound.Options.(*option.SOCKSOutboundOptions)
	if !ok {
		t.Fatalf("outbound options type = %T, want *option.SOCKSOutboundOptions", outbound.Options)
	}
	if opts.Server != "99.144.123.135" {
		t.Fatalf("server = %q, want %q", opts.Server, "99.144.123.135")
	}
	if opts.ServerPort != 30350 {
		t.Fatalf("server port = %d, want %d", opts.ServerPort, 30350)
	}
	if opts.Username != "demo" {
		t.Fatalf("username = %q, want %q", opts.Username, "demo")
	}
	if opts.Password != "secret" {
		t.Fatalf("password = %q, want %q", opts.Password, "secret")
	}
	if opts.Version != "5" {
		t.Fatalf("version = %q, want %q", opts.Version, "5")
	}
}

func TestBuildCreatesIndependentGroupListenerSelectorAndPool(t *testing.T) {
	cfg := &config.Config{Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: 2323, Protocol: "http"},
		Pool: config.PoolConfig{Mode: "random", FailureThreshold: 3, BlacklistDuration: time.Hour},
		Nodes: []config.NodeConfig{{ID: 1, Name: "hk", URI: "http://hk.example:80", Region: "hk"},
			{ID: 2, Name: "jp", URI: "http://jp.example:80", Region: "jp"}},
		Groups: []config.GroupPoolConfig{{ID: 7, Name: "mixed", BindAddress: "127.0.0.1", BindPort: 10007,
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, ExplicitNodeIDs: []int64{2},
			ExcludedNodeIDs: []int64{2},
			FailureWindow:   5 * time.Minute, FailureThreshold: 3, Enabled: true}}}
	opts, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var foundInbound, foundSelector, foundPool bool
	for _, inbound := range opts.Inbounds {
		if inbound.Tag == "group-in-7" {
			foundInbound = true
		}
	}
	for _, outbound := range opts.Outbounds {
		switch outbound.Tag {
		case "group-selector-7":
			foundSelector = outbound.Type == C.TypeSelector
		case "group-pool-7":
			options, ok := outbound.Options.(*poolout.Options)
			foundPool = ok && options.SelectorTag == "group-selector-7" && len(options.Members) == 1
		}
	}
	if !foundInbound || !foundSelector || !foundPool {
		t.Fatalf("missing group components: inbound=%v selector=%v pool=%v", foundInbound, foundSelector, foundPool)
	}
}

func TestBuildGroupFiltersExcludedPersistedState(t *testing.T) {
	cfg := &config.Config{Pool: config.PoolConfig{Mode: "random", FailureThreshold: 3, BlacklistDuration: time.Hour},
		Nodes: []config.NodeConfig{{ID: 1, Name: "kept", URI: "http://kept.example:80", Region: "hk"},
			{ID: 2, Name: "excluded", URI: "http://excluded.example:80", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 17, Name: "HK", BindAddress: "127.0.0.1", BindPort: 10017,
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, ExcludedNodeIDs: []int64{2},
			FailureWindow: 5 * time.Minute, FailureThreshold: 3, Enabled: true,
			NodeStates: []config.GroupNodeStateConfig{{NodeID: 1, FailureHistory: []int64{1}}, {NodeID: 2, Evicted: true, LastError: "auth failed"}}}}}
	opts, err := BuildGroup(cfg, 17)
	if err != nil {
		t.Fatal(err)
	}
	for _, outbound := range opts.Outbounds {
		if outbound.Tag != "group-pool-17" {
			continue
		}
		poolOptions := outbound.Options.(*poolout.Options)
		if len(poolOptions.Members) != 1 || len(poolOptions.InitialGroupState) != 1 {
			t.Fatalf("members=%v initial=%v", poolOptions.Members, poolOptions.InitialGroupState)
		}
		memberTag := poolOptions.Members[0]
		if poolOptions.Metadata[memberTag].NodeID != 1 || poolOptions.InitialGroupState[memberTag].NodeID != 1 {
			t.Fatalf("excluded state leaked into group: members=%v initial=%v metadata=%v", poolOptions.Members, poolOptions.InitialGroupState, poolOptions.Metadata)
		}
		return
	}
	t.Fatal("group pool outbound not found")
}

func TestBuildLeavesLowestLatencyGroupWithoutSyntheticCurrent(t *testing.T) {
	cfg := &config.Config{Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: 2323, Protocol: "http"},
		Pool: config.PoolConfig{Mode: "random", FailureThreshold: 3, BlacklistDuration: time.Hour},
		Nodes: []config.NodeConfig{{ID: 1, Name: "first", URI: "http://first.example:80", Region: "hk"},
			{ID: 2, Name: "second", URI: "http://second.example:80", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 8, Name: "lowest", BindAddress: "127.0.0.1", BindPort: 10008,
			Protocol: "mixed", DispatchMode: "lowest_latency", Regions: []string{"hk"},
			FailureWindow: 5 * time.Minute, FailureThreshold: 3, Enabled: true}}}
	opts, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, outbound := range opts.Outbounds {
		if outbound.Tag != "group-pool-8" {
			continue
		}
		poolOptions, ok := outbound.Options.(*poolout.Options)
		if !ok {
			t.Fatalf("group pool options type = %T", outbound.Options)
		}
		if poolOptions.Mode != "lowest_latency" || poolOptions.PreferredMember != "" {
			t.Fatalf("mode=%q preferred=%q, want lowest_latency with no synthetic current", poolOptions.Mode, poolOptions.PreferredMember)
		}
		return
	}
	t.Fatal("group pool outbound not found")
}

func TestBuildBaseAndGroupAreRuntimeIsolated(t *testing.T) {
	cfg := &config.Config{Listener: config.ListenerConfig{Address: "127.0.0.1", Port: 2323, Protocol: "http"},
		Pool:  config.PoolConfig{Mode: "random", FailureThreshold: 3, BlacklistDuration: time.Hour},
		Nodes: []config.NodeConfig{{ID: 1, Name: "hk", URI: "http://hk.example:80", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 9, Name: "isolated", BindAddress: "127.0.0.1", BindPort: 10009,
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"},
			FailureWindow: 5 * time.Minute, FailureThreshold: 3, Enabled: true}}}
	base, err := BuildBase(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Inbounds) != 0 {
		t.Fatalf("disabled base entries produced %d inbounds", len(base.Inbounds))
	}
	for _, inbound := range base.Inbounds {
		if strings.HasPrefix(inbound.Tag, "group-in-") {
			t.Fatalf("base topology contains group inbound %q", inbound.Tag)
		}
	}
	groupOptions, err := BuildGroup(cfg, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(groupOptions.Inbounds) != 1 || groupOptions.Inbounds[0].Tag != "group-in-9" || groupOptions.Route.Final != "group-pool-9" {
		t.Fatalf("isolated group topology: inbounds=%v final=%q", groupOptions.Inbounds, groupOptions.Route.Final)
	}
	for _, outbound := range groupOptions.Outbounds {
		if outbound.Tag == "group-pool-9" {
			poolOptions := outbound.Options.(*poolout.Options)
			if !poolOptions.MonitorObserverOnly {
				t.Fatal("isolated group pool became a monitor callback owner")
			}
			return
		}
	}
	t.Fatal("isolated group pool not found")
}

func TestBuildNodeOutboundSupportsHTTP(t *testing.T) {
	outbound, err := buildNodeOutbound("http-node", "http://alice:wonderland@example.com:8080/proxy", false)
	if err != nil {
		t.Fatalf("buildNodeOutbound returned error: %v", err)
	}
	if outbound.Type != C.TypeHTTP {
		t.Fatalf("outbound type = %q, want %q", outbound.Type, C.TypeHTTP)
	}

	opts, ok := outbound.Options.(*option.HTTPOutboundOptions)
	if !ok {
		t.Fatalf("outbound options type = %T, want *option.HTTPOutboundOptions", outbound.Options)
	}
	if opts.Server != "example.com" {
		t.Fatalf("server = %q, want %q", opts.Server, "example.com")
	}
	if opts.ServerPort != 8080 {
		t.Fatalf("server port = %d, want %d", opts.ServerPort, 8080)
	}
	if opts.Username != "alice" {
		t.Fatalf("username = %q, want %q", opts.Username, "alice")
	}
	if opts.Password != "wonderland" {
		t.Fatalf("password = %q, want %q", opts.Password, "wonderland")
	}
	if opts.Path != "/proxy" {
		t.Fatalf("path = %q, want %q", opts.Path, "/proxy")
	}
}
