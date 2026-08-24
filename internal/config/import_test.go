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
