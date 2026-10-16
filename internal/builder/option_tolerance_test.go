package builder

import (
	"testing"

	"easy_proxies/internal/config"

	"github.com/sagernet/sing-box/option"
)

// tlsOf extracts the TLS options of any outbound that carries them, so a test
// can assert on one field without knowing the protocol.
func tlsOf(t *testing.T, uri string, skipCertVerify bool) *option.OutboundTLSOptions {
	t.Helper()
	outbound, err := BuildNodeOutbound("tol", config.NodeConfig{URI: uri}, skipCertVerify)
	if err != nil {
		t.Fatalf("build %q: %v", uri, err)
	}
	switch opts := outbound.Options.(type) {
	case *option.VLESSOutboundOptions:
		return opts.TLS
	case *option.TrojanOutboundOptions:
		return opts.TLS
	case *option.AnyTLSOutboundOptions:
		return opts.TLS
	case *option.HTTPOutboundOptions:
		return opts.TLS
	case *option.Hysteria2OutboundOptions:
		return opts.TLS
	default:
		t.Fatalf("outbound for %q carries no TLS options", uri)
		return nil
	}
}

// TestCertificateVerificationSpellingsAgreeAcrossProtocols pins the fix for the
// divergence that made a node verify its certificate on one code path and not on
// another: the four TLS builders resolved allowInsecure vs insecure in opposite
// orders, and only two of them understood skip-cert-verify at all.
func TestCertificateVerificationSpellingsAgreeAcrossProtocols(t *testing.T) {
	links := map[string]string{
		"vless":  "vless://abc@example.com:443?type=tcp&security=tls&%s",
		"trojan": "trojan://pass@example.com:443?%s",
		"anytls": "anytls://pass@example.com:443?%s",
		"http":   "http://user:pass@example.com:8080?security=tls&%s",
		"hy2":    "hysteria2://pass@example.com:443?%s",
	}
	spellings := []string{
		"insecure=1", "insecure=true", "insecure=yes", "insecure=on",
		"allowInsecure=1", "skip-cert-verify=true",
	}
	for protocol, template := range links {
		for _, spelling := range spellings {
			uri := template
			if idx := indexOfPercentS(uri); idx >= 0 {
				uri = replaceOnce(uri, "%s", spelling)
			} else {
				t.Fatalf("%s template is missing a placeholder", protocol)
			}
			if tls := tlsOf(t, uri, false); tls == nil || !tls.Insecure {
				t.Errorf("%s: %q did not disable certificate verification", protocol, spelling)
			}
		}
	}
}

// TestExplicitVerificationVerdictOverridesGlobalSetting pins the semantics that
// made the old code correct only by accident: a link that states its verdict
// overrides the global SkipCertVerify setting, and an absent one must not.
func TestExplicitVerificationVerdictOverridesGlobalSetting(t *testing.T) {
	if tls := tlsOf(t, "trojan://pass@example.com:443?insecure=0", true); tls.Insecure {
		t.Error("an explicit insecure=0 must override the global skip-cert-verify setting")
	}
	if tls := tlsOf(t, "trojan://pass@example.com:443", true); !tls.Insecure {
		t.Error("an absent verdict must leave the global skip-cert-verify setting alone")
	}
	if tls := tlsOf(t, "trojan://pass@example.com:443?insecure=1", false); !tls.Insecure {
		t.Error("an explicit insecure=1 must enable it even when the global setting is off")
	}
}

// TestTLSHostSpellingsAreResolvedForEveryProtocol pins the alias set: the
// identity folds peer / servername / server_name into sni, but the data plane
// read only "sni", so a link written with "peer" was verified against the
// dialled address instead of the configured name.
func TestTLSHostSpellingsAreResolvedForEveryProtocol(t *testing.T) {
	for _, spelling := range []string{"sni", "peer", "servername", "server_name"} {
		for protocol, template := range map[string]string{
			"vless":  "vless://abc@example.com:443?type=tcp&security=tls&%s=edge.example",
			"trojan": "trojan://pass@example.com:443?%s=edge.example",
		} {
			uri := replaceOnce(template, "%s", spelling)
			if tls := tlsOf(t, uri, false); tls.ServerName != "edge.example" {
				t.Errorf("%s: %s was not resolved to the TLS server name (got %q)", protocol, spelling, tls.ServerName)
			}
		}
	}
}

// TestHysteria2AuthStringIsPercentDecoded pins the fix for sending an escaped
// password: u.User.String() returns the encoded userinfo, so a password
// containing ":" or "@" reached the server percent-encoded and was rejected.
func TestHysteria2AuthStringIsPercentDecoded(t *testing.T) {
	for _, tc := range []struct{ uri, want string }{
		{"hysteria2://pass@example.com:443", "pass"},
		{"hysteria2://p%40ss%3Aword@example.com:443", "p@ss:word"},
		{"hysteria2://user:pass@example.com:443", "user:pass"},
	} {
		outbound, err := BuildNodeOutbound("hy2", config.NodeConfig{URI: tc.uri}, false)
		if err != nil {
			t.Fatalf("build %q: %v", tc.uri, err)
		}
		opts, ok := outbound.Options.(*option.Hysteria2OutboundOptions)
		if !ok {
			t.Fatalf("unexpected options type for %q", tc.uri)
		}
		if opts.Password != tc.want {
			t.Errorf("%q: auth string is %q, want %q", tc.uri, opts.Password, tc.want)
		}
	}
}

// TestOptionKeySpellingsReachTheOutbound pins the alias gap that let a node keep
// a setting in its identity while the data plane dropped it: the identity folds
// keys to lower case, but the outbound builder read camelCase spellings.
func TestOptionKeySpellingsReachTheOutbound(t *testing.T) {
	vlessCases := []string{
		"vless://abc@example.com:443?type=tcp&security=tls&packetEncoding=xudp&flow=xtls-rprx-vision",
		"vless://abc@example.com:443?type=tcp&security=tls&packet_encoding=xudp&flow=xtls-rprx-vision",
		"vless://abc@example.com:443?type=tcp&security=tls&PACKETENCODING=xudp&flow=xtls-rprx-vision",
	}
	for _, uri := range vlessCases {
		outbound, err := BuildNodeOutbound("pe", config.NodeConfig{URI: uri}, false)
		if err != nil {
			t.Fatalf("build %q: %v", uri, err)
		}
		opts := outbound.Options.(*option.VLESSOutboundOptions)
		if opts.PacketEncoding == nil || *opts.PacketEncoding != "xudp" {
			t.Errorf("%q: packet encoding was dropped", uri)
		}
	}

	for _, spelling := range []string{"serviceName", "service_name", "servicename", "grpc-service-name"} {
		uri := "vless://abc@example.com:443?type=grpc&security=tls&" + spelling + "=my-svc"
		outbound, err := BuildNodeOutbound("grpc", config.NodeConfig{URI: uri}, false)
		if err != nil {
			t.Fatalf("build %q: %v", uri, err)
		}
		opts := outbound.Options.(*option.VLESSOutboundOptions)
		if opts.Transport == nil || opts.Transport.GRPCOptions.ServiceName != "my-svc" {
			t.Errorf("%q: gRPC service name was dropped", spelling)
		}
	}
}

// TestNetworkAliasSelectsTheSameTransportAsType pins the fix for subrender
// advertising one transport while the outbound dialled another.
func TestNetworkAliasSelectsTheSameTransportAsType(t *testing.T) {
	for _, uri := range []string{
		"vless://abc@example.com:443?type=ws&path=%2Fws",
		"vless://abc@example.com:443?network=ws&path=%2Fws",
	} {
		outbound, err := BuildNodeOutbound("ws", config.NodeConfig{URI: uri}, false)
		if err != nil {
			t.Fatalf("build %q: %v", uri, err)
		}
		opts := outbound.Options.(*option.VLESSOutboundOptions)
		if opts.Transport == nil || opts.Transport.Type != "ws" {
			t.Errorf("%q: transport is not websocket", uri)
		}
	}
}

func indexOfPercentS(value string) int {
	for i := 0; i+1 < len(value); i++ {
		if value[i] == '%' && value[i+1] == 's' {
			return i
		}
	}
	return -1
}

func replaceOnce(value, old, replacement string) string {
	index := indexOfPercentS(value)
	if index < 0 {
		return value
	}
	return value[:index] + replacement + value[index+2:]
}
