package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseImportContent_ClashInlineYAML(t *testing.T) {
	// Single inline Clash YAML proxy item, the exact form miaomiaowu accepts.
	content := `- { name: '🇸🇬新加坡 1 - 【XG1】', type: ss, server: 22.33.44.55, port: 443, cipher: chacha20-ietf-poly1305, password: 123456, udp: true }`

	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatalf("ParseImportContent returned error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d (%+v)", len(nodes), nodes)
	}

	n := nodes[0]
	if !IsProxyURI(n.URI) {
		t.Errorf("resulting URI is not a valid proxy URI: %s", n.URI)
	}
	if n.Name != "🇸🇬新加坡 1 - 【XG1】" {
		t.Errorf("expected name to be preserved, got %q", n.Name)
	}
	if !strings.HasPrefix(n.URI, "ss://") {
		t.Errorf("expected ss:// URI, got %s", n.URI)
	}
}

func TestParseImportContent_ClashFullDocument(t *testing.T) {
	// Full Clash YAML document with a top-level proxies: key.
	content := `proxies:
  - name: my-trojan
    type: trojan
    server: example.com
    port: 443
    password: secretpass
    sni: example.com
  - name: my-vmess
    type: vmess
    server: example.com
    port: 443
    uuid: 11111111-2222-3333-4444-555555555555
    alterId: 0
    network: ws
    ws-opts:
      path: /path
      headers:
        Host: example.com
`

	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatalf("ParseImportContent returned error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d (%+v)", len(nodes), nodes)
	}

	for i, n := range nodes {
		if !IsProxyURI(n.URI) {
			t.Errorf("node %d URI is not valid: %s", i, n.URI)
		}
	}
	if nodes[0].Name != "my-trojan" {
		t.Errorf("node[0] name = %q, want my-trojan", nodes[0].Name)
	}
	if nodes[1].Name != "my-vmess" {
		t.Errorf("node[1] name = %q, want my-vmess", nodes[1].Name)
	}
}

func TestParseImportContent_PlainURIList(t *testing.T) {
	content := `# my nodes
trojan://secretpass@example.com:443?sni=example.com#trojan-node

vless://11111111-2222-3333-4444-555555555555@example.com:443?encryption=none#vless-node
`

	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatalf("ParseImportContent returned error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Name != "trojan-node" {
		t.Errorf("node[0] name = %q, want trojan-node", nodes[0].Name)
	}
	if nodes[1].Name != "vless-node" {
		t.Errorf("node[1] name = %q, want vless-node", nodes[1].Name)
	}
}

func TestParseImportContent_HTTPProxyAndMarkdownLinks(t *testing.T) {
	raw := "http://user:password@104.207.47.150:3129"
	content := raw + "\n[backup](http://other:secret@198.51.100.7:8080)\n<socks5://127.0.0.1:1080>\n"
	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes=%+v", nodes)
	}
	want := []string{raw, "http://other:secret@198.51.100.7:8080", "socks5://127.0.0.1:1080"}
	for index, uri := range want {
		if nodes[index].URI != uri || !IsProxyURI(nodes[index].URI) {
			t.Fatalf("node %d=%+v, want URI %q", index, nodes[index], uri)
		}
	}
}

func TestParseImportContentRejectsUnrecognizedText(t *testing.T) {
	if _, err := ParseImportContent("this is not a proxy"); err == nil {
		t.Fatal("unrecognized import content unexpectedly succeeded")
	}
}

func TestParseImportContent_Base64Payload(t *testing.T) {
	// Base64-encoded URI list, as v2ray subscriptions deliver.
	plain := "trojan://secretpass@example.com:443?sni=example.com#trojan-node\n"
	content := base64.StdEncoding.EncodeToString([]byte(plain))

	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatalf("ParseImportContent returned error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Name != "trojan-node" {
		t.Errorf("node[0] name = %q, want trojan-node", nodes[0].Name)
	}
}

func TestParseImportContent_MixedInlineYAMLAndComments(t *testing.T) {
	content := `# clash nodes
- { name: ss1, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-256-gcm, password: pass1 }
# comment line
- { name: ss2, type: ss, server: 5.6.7.8, port: 8388, cipher: aes-256-gcm, password: pass2 }`

	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatalf("ParseImportContent returned error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d (%+v)", len(nodes), nodes)
	}
	if nodes[0].Name != "ss1" || nodes[1].Name != "ss2" {
		t.Errorf("names = %q, %q; want ss1, ss2", nodes[0].Name, nodes[1].Name)
	}
}

func TestParseImportContent_Empty(t *testing.T) {
	if _, err := ParseImportContent("   "); err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
}

func TestParseImportContentSingBoxJSONC(t *testing.T) {
	content := `{
		// comments are accepted by sing-box
		"outbounds": [
			{"type":"vless","tag":"edge","server":"Example.COM","server_port":443,"uuid":"ABC","tls":{"enabled":true,"server_name":"edge.example"},"transport":{"type":"ws","path":"/proxy","headers":{"Host":"cdn.example"}}},
			{"type":"direct","tag":"direct"},
		]
	}`
	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "edge" || nodes[0].IdentityHash == "" {
		t.Fatalf("nodes=%+v", nodes)
	}
	reported, issues, err := ParseImportContentReport(content)
	if err != nil || len(reported) != 1 || len(issues) != 1 || !strings.Contains(issues[0], "direct") {
		t.Fatalf("reported=%+v issues=%v err=%v", reported, issues, err)
	}
}

func TestParseImportContentKeepsDuplicatesForReporting(t *testing.T) {
	nodes, err := ParseImportContent("http://user:pass@EXAMPLE.com:80#one\nhttp://user:pass@example.com#two")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].IdentityHash != nodes[1].IdentityHash {
		t.Fatalf("nodes=%+v", nodes)
	}
	subscriptionNodes, err := ParseSubscriptionContent("http://user:pass@EXAMPLE.com:80#one\nhttp://user:pass@example.com#two")
	if err != nil {
		t.Fatal(err)
	}
	if len(subscriptionNodes) != 1 {
		t.Fatalf("subscription nodes=%+v", subscriptionNodes)
	}
}

func TestParseImportContentRejectsUnsupportedRuntimeProtocol(t *testing.T) {
	if _, err := ParseImportContent("ssr://example"); err == nil {
		t.Fatal("expected unsupported SSR error")
	}
}

func TestParseClashHTTPAndSOCKSNodes(t *testing.T) {
	content := `proxies:
  - { name: http-node, type: http, server: proxy.example, port: 8080, username: alice, password: secret }
  - { name: socks-node, type: socks5, server: 127.0.0.1, port: 1080 }`
	nodes, err := ParseImportContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || !strings.HasPrefix(nodes[0].URI, "http://") || !strings.HasPrefix(nodes[1].URI, "socks5://") {
		t.Fatalf("nodes=%+v", nodes)
	}
}
