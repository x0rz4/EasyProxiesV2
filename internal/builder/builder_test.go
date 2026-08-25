package builder

import (
	"testing"
	"time"

	"easy_proxies/internal/config"
	poolout "easy_proxies/internal/outbound/pool"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
)

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
	cfg := &config.Config{Mode: "pool", Listener: config.ListenerConfig{Address: "127.0.0.1", Port: 2323, Protocol: "http"},
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
