package builder

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/subrender"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/transport/sip003"
	M "github.com/sagernet/sing/common/metadata"
)

func TestClashObfsBuildsSupportedPlugin(t *testing.T) {
	for _, mode := range []string{"http", "tls"} {
		t.Run(mode, func(t *testing.T) {
			nodes, err := config.ParseSubscriptionContent(fmt.Sprintf(`proxies:
  - name: obfs-node
    type: ss
    server: 127.0.0.1
    port: 8388
    cipher: aes-128-gcm
    password: test-password
    plugin: obfs
    plugin-opts:
      mode: %s
      host: cdn.example
`, mode))
			if err != nil || len(nodes) != 1 {
				t.Fatalf("import: nodes=%d err=%v", len(nodes), err)
			}
			node := nodes[0]
			before, err := nodecodec.ParseURI(node.URI)
			if err != nil {
				t.Fatal(err)
			}
			outbound, err := BuildNodeOutbound("0123456789abcdef@v1", node, false)
			if err != nil {
				t.Fatal(err)
			}
			opts := outbound.Options.(*option.ShadowsocksOutboundOptions)
			if opts.Plugin != "obfs-local" {
				t.Fatalf("plugin=%q, want obfs-local", opts.Plugin)
			}
			args, err := sip003.ParsePluginOptions(opts.PluginOptions)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, sip003.Args{"obfs": {mode}, "obfs-host": {"cdn.example"}}) {
				t.Fatalf("plugin options=%v, want %s mode and cdn.example host", args, mode)
			}
			if _, err := sip003.CreatePlugin(context.Background(), opts.Plugin, opts.PluginOptions, nil, nil, M.ParseSocksaddr("127.0.0.1:8388")); err != nil {
				t.Fatalf("embedded plugin initialization failed: %v", err)
			}
			// Runtime adaptation must not re-identify stored nodes or alter their export.
			after, err := nodecodec.ParseURI(node.URI)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("identity changed: err=%v", err)
			}
			spec, err := nodecodec.FromURI(node.URI)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := subrender.ClashSpec("obfs-node", spec)
			if err != nil || proxy["plugin"] != "obfs" {
				t.Fatalf("Clash plugin=%v err=%v", proxy["plugin"], err)
			}
			if !reflect.DeepEqual(proxy["plugin-opts"], map[string]any{"mode": mode, "host": "cdn.example"}) {
				t.Fatalf("Clash options changed: %v", proxy["plugin-opts"])
			}
		})
	}
}

func TestShadowsocksPluginCompatibility(t *testing.T) {
	tests := []struct {
		name, plugin, options, wantPlugin, wantOptions string
		wantError                                      bool
	}{
		{name: "plain"},
		{name: "native obfs", plugin: "obfs-local", options: "obfs=tls;obfs-host=cdn.example", wantPlugin: "obfs-local", wantOptions: "obfs=tls;obfs-host=cdn.example"},
		{name: "v2ray", plugin: "v2ray-plugin", options: "tls;host=cdn.example;path=/ws", wantPlugin: "v2ray-plugin", wantOptions: "tls;host=cdn.example;path=/ws"},
		{name: "unknown preserved", plugin: "other-plugin", options: "mode=test", wantPlugin: "other-plugin", wantOptions: "mode=test"},
		{name: "default obfs", plugin: "obfs", wantPlugin: "obfs-local"},
		{name: "native options on alias", plugin: "obfs", options: "obfs=tls;obfs-host=cdn.example", wantPlugin: "obfs-local", wantOptions: "obfs=tls;obfs-host=cdn.example"},
		{name: "native keys take precedence", plugin: "obfs", options: "mode=http;obfs=tls;host=alias.example;obfs-host=native.example", wantPlugin: "obfs-local", wantOptions: "obfs=tls;obfs-host=native.example"},
		{name: "escaped values", plugin: "obfs", options: `mode=tls;host=cdn\;test\=x\\end;extra=one;extra=two`, wantPlugin: "obfs-local", wantOptions: `extra=one;extra=two;obfs=tls;obfs-host=cdn\;test\=x\\end`},
		{name: "invalid escaping", plugin: "obfs", options: `mode=tls;host=broken\`, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query := url.Values{}
			query.Set("plugin", tt.plugin)
			query.Set("plugin-opts", tt.options)
			outbound, err := BuildNodeOutbound("0123456789abcdef@v1", config.NodeConfig{
				URI: "ss://aes-128-gcm:test-password@127.0.0.1:8388?" + query.Encode(),
			}, false)
			if (err != nil) != tt.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			opts := outbound.Options.(*option.ShadowsocksOutboundOptions)
			if opts.Plugin != tt.wantPlugin || opts.PluginOptions != tt.wantOptions {
				t.Fatalf("plugin=%q options=%q, want %q %q", opts.Plugin, opts.PluginOptions, tt.wantPlugin, tt.wantOptions)
			}
		})
	}
}
