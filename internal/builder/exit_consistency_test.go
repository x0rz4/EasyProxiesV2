package builder

import (
	"encoding/json"
	"os"
	"testing"

	"easy_proxies/internal/nodecodec"
	"easy_proxies/internal/subrender"

	"github.com/sagernet/sing-box/option"
)

// TestEveryExitAgreesOnTheSameConnection is the cross-path guard for the whole
// codec layer.
//
// A node has three exits — the sing-box outbound it dials with, the link it is
// stored as, and the Clash proxy it is published as. They used to be produced by
// three independent interpreters of the raw string, so they could disagree: the
// subscription advertised a websocket while the outbound dialled raw TCP, an
// exported proxy omitted the certificate check the outbound disabled, and a
// published password was the escaped userinfo instead of the credential.
//
// Every corpus link is pushed through all three and the connection-defining
// fields are compared. Add new fields here when the codec learns one.
func TestEveryExitAgreesOnTheSameConnection(t *testing.T) {
	links := corpusLinks(t)
	checked := 0
	for _, link := range links {
		spec, err := nodecodec.FromURI(link)
		if err != nil {
			continue
		}
		outbound, err := buildSpecOutbound("consistency", spec, false)
		if err != nil {
			t.Errorf("%s: outbound build failed: %v", link, err)
			continue
		}
		proxy, err := subrender.ClashSpec("consistency", spec)
		if err != nil {
			t.Errorf("%s: clash render failed: %v", link, err)
			continue
		}
		checkEndpoint(t, spec, outbound, proxy)
		checkCredentials(t, spec, outbound, proxy)
		checkTLS(t, spec, outbound, proxy)
		checkTransport(t, spec, outbound, proxy)
		checked++
	}
	if checked < 60 {
		t.Fatalf("only %d links were cross-checked; the corpus lost coverage", checked)
	}
	t.Logf("cross-checked %d links across the outbound, link and Clash exits", checked)
}

func corpusLinks(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../nodecodec/testdata/identity_golden.json")
	if err != nil {
		t.Fatalf("read identity golden: %v", err)
	}
	var entries []struct {
		URI string `json:"uri"`
		Err string `json:"err,omitempty"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("parse identity golden: %v", err)
	}
	links := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Err == "" {
			links = append(links, entry.URI)
		}
	}
	return links
}

func checkEndpoint(t *testing.T, spec nodecodec.Spec, outbound option.Outbound, proxy map[string]any) {
	t.Helper()
	server, port := serverAndPort(outbound)
	if server != spec.Server || port != spec.Port {
		t.Errorf("%s: outbound dials %s:%d, spec says %s:%d", spec.Name, server, port, spec.Server, spec.Port)
	}
	if proxy["server"] != spec.Server || proxy["port"] != spec.Port {
		t.Errorf("%s: clash publishes %v:%v, spec says %s:%d", spec.Name, proxy["server"], proxy["port"], spec.Server, spec.Port)
	}
}

func checkCredentials(t *testing.T, spec nodecodec.Spec, outbound option.Outbound, proxy map[string]any) {
	t.Helper()
	var singBox, clash, want string
	switch opts := outbound.Options.(type) {
	case *option.VMessOutboundOptions:
		singBox, want = opts.UUID, spec.Auth.UUID
		clash, _ = proxy["uuid"].(string)
	case *option.VLESSOutboundOptions:
		singBox, want = opts.UUID, spec.Auth.UUID
		clash, _ = proxy["uuid"].(string)
	case *option.TrojanOutboundOptions:
		singBox, want = opts.Password, spec.AuthString()
		clash, _ = proxy["password"].(string)
	case *option.Hysteria2OutboundOptions:
		singBox, want = opts.Password, spec.AuthString()
		clash, _ = proxy["password"].(string)
	case *option.AnyTLSOutboundOptions:
		singBox, want = opts.Password, spec.AuthString()
		clash, _ = proxy["password"].(string)
	case *option.ShadowsocksOutboundOptions:
		singBox, want = opts.Password, spec.Auth.Password
		clash, _ = proxy["password"].(string)
		if opts.Method != spec.Cipher {
			t.Errorf("%s: outbound cipher is %q, spec says %q", spec.Name, opts.Method, spec.Cipher)
		}
		if clash != spec.Cipher {
			if cipher, _ := proxy["cipher"].(string); cipher != spec.Cipher {
				t.Errorf("%s: clash cipher is %q, spec says %q", spec.Name, cipher, spec.Cipher)
			}
		}
	case *option.HTTPOutboundOptions:
		singBox, want = opts.Password, spec.Auth.Password
		clash, _ = proxy["password"].(string)
		if opts.Username != spec.Auth.Username {
			t.Errorf("%s: outbound username is %q, spec says %q", spec.Name, opts.Username, spec.Auth.Username)
		}
	case *option.SOCKSOutboundOptions:
		singBox, want = opts.Password, spec.Auth.Password
		clash, _ = proxy["password"].(string)
		if opts.Username != spec.Auth.Username {
			t.Errorf("%s: outbound username is %q, spec says %q", spec.Name, opts.Username, spec.Auth.Username)
		}
	default:
		t.Fatalf("%s: unexpected outbound options %T", spec.Name, outbound.Options)
	}
	if singBox != want {
		t.Errorf("%s: outbound credential is %q, spec says %q", spec.Name, singBox, want)
	}
	if want != "" && clash != want {
		t.Errorf("%s: clash credential is %q, spec says %q", spec.Name, clash, want)
	}
}

func checkTLS(t *testing.T, spec nodecodec.Spec, outbound option.Outbound, proxy map[string]any) {
	t.Helper()
	tls := outboundTLSOptionsOf(outbound)

	// Every exit must agree on whether TLS is on at all.
	if (tls != nil) != spec.TLS.Enabled {
		t.Errorf("%s: outbound TLS=%v, spec says enabled=%v", spec.Name, tls != nil, spec.TLS.Enabled)
	}
	if !spec.TLS.Enabled || tls == nil {
		return
	}
	if spec.TLS.ServerName != "" {
		if tls.ServerName != spec.TLS.ServerName {
			t.Errorf("%s: outbound SNI is %q, spec says %q", spec.Name, tls.ServerName, spec.TLS.ServerName)
		}
		if published, _ := proxy["servername"].(string); published != "" && published != spec.TLS.ServerName {
			t.Errorf("%s: clash SNI is %q, spec says %q", spec.Name, published, spec.TLS.ServerName)
		}
	}
	if spec.TLS.InsecureStated && tls.Insecure != spec.TLS.Insecure {
		t.Errorf("%s: outbound insecure=%v, spec says %v", spec.Name, tls.Insecure, spec.TLS.Insecure)
	}
	if published, ok := proxy["skip-cert-verify"].(bool); ok && published != spec.TLS.Insecure {
		t.Errorf("%s: clash skip-cert-verify=%v, spec says %v", spec.Name, published, spec.TLS.Insecure)
	}
	if spec.TLS.Security == "reality" && tls.Reality != nil && tls.Reality.PublicKey != spec.TLS.PublicKey {
		t.Errorf("%s: outbound reality public key is %q, spec says %q", spec.Name, tls.Reality.PublicKey, spec.TLS.PublicKey)
	}
	if spec.Protocol == "ss" || spec.Protocol == "socks5" {
		if published, ok := proxy["tls"].(bool); ok && published {
			t.Errorf("%s: clash claims TLS for a protocol that has none", spec.Name)
		}
	}
}

func checkTransport(t *testing.T, spec nodecodec.Spec, outbound option.Outbound, proxy map[string]any) {
	t.Helper()
	if !spec.ParameterIsV2RayTransport() {
		return
	}
	want := spec.Transport.Type
	if want == "" {
		want = "tcp"
	}
	if got := transportTypeOf(outbound); got != want {
		t.Errorf("%s: outbound transport is %q, spec says %q", spec.Name, got, want)
	}
	// Clash spells sing-box's http transport "h2"; tcp is the implicit default.
	if published, ok := proxy["network"].(string); ok && published != want {
		t.Errorf("%s: clash transport is %q, spec says %q", spec.Name, published, want)
	}
	if spec.Transport.Path == "" {
		return
	}
	if spec.Transport.Type == "ws" {
		if !containsPath(clashWebsocketPath(proxy), spec.Transport.Path) {
			t.Errorf("%s: clash websocket path does not carry %q: %v", spec.Name, spec.Transport.Path, proxy["ws-opts"])
		}
	}
}

func serverAndPort(outbound option.Outbound) (string, uint16) {
	switch opts := outbound.Options.(type) {
	case *option.VMessOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.VLESSOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.TrojanOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.Hysteria2OutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.AnyTLSOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.ShadowsocksOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.HTTPOutboundOptions:
		return opts.Server, opts.ServerPort
	case *option.SOCKSOutboundOptions:
		return opts.Server, opts.ServerPort
	default:
		return "", 0
	}
}

func outboundTLSOptionsOf(outbound option.Outbound) *option.OutboundTLSOptions {
	switch opts := outbound.Options.(type) {
	case *option.VMessOutboundOptions:
		return opts.TLS
	case *option.VLESSOutboundOptions:
		return opts.TLS
	case *option.TrojanOutboundOptions:
		return opts.TLS
	case *option.Hysteria2OutboundOptions:
		return opts.TLS
	case *option.AnyTLSOutboundOptions:
		return opts.TLS
	case *option.HTTPOutboundOptions:
		return opts.TLS
	default:
		return nil
	}
}

// transportTypeOf normalises sing-box's spelling back to the link's, so the
// comparison is about meaning rather than vocabulary.
func transportTypeOf(outbound option.Outbound) string {
	var transport *option.V2RayTransportOptions
	switch opts := outbound.Options.(type) {
	case *option.VMessOutboundOptions:
		transport = opts.Transport
	case *option.VLESSOutboundOptions:
		transport = opts.Transport
	case *option.TrojanOutboundOptions:
		transport = opts.Transport
	}
	if transport == nil || transport.Type == "" || transport.Type == "tcp" {
		return "tcp"
	}
	if transport.Type == "http" {
		return "h2"
	}
	return transport.Type
}

func clashWebsocketPath(proxy map[string]any) string {
	options, _ := proxy["ws-opts"].(map[string]any)
	path, _ := options["path"].(string)
	return path
}

func containsPath(actual, want string) bool {
	return actual == want || (len(actual) > len(want) && actual[:len(want)] == want)
}
