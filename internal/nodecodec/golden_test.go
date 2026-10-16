package nodecodec

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateIdentityGolden rewrites testdata/identity_golden.json from the current
// implementation. Run it only when the identity contract is deliberately
// changed (which also requires bumping IdentityVersion and a data migration):
//
//	go test ./internal/nodecodec/ -run TestIdentityGolden -update-identity-golden
var updateIdentityGolden = flag.Bool("update-identity-golden", false,
	"rewrite testdata/identity_golden.json from the current implementation")

const identityGoldenPath = "testdata/identity_golden.json"

// Known identity defects locked by this snapshot
// ---------------------------------------------------------------------------
// The golden records CURRENT behaviour, including four defects. They are
// deliberately NOT fixed by the NodeSpec refactor: each one changes
// IdentityHash for already-stored nodes, so fixing it requires an explicit
// IdentityVersion bump plus a data migration, not an incidental change. The
// corpus cases that pin them:
//
//  1. vless/network-alias-only vs vless/network-only-no-path — both produce the
//     SAME canonical JSON, yet the real transport differs: builder.buildV2RayTransport
//     only reads "type", so with no type it infers ws from the presence of
//     "path" and otherwise falls back to tcp. Two behaviourally different nodes
//     therefore share an IdentityHash and dedup silently drops one.
//  2. trojan/allowInsecure-wins-over-insecure — normalizeQueryKey folds
//     allowInsecure into insecure, so options["insecure"] holds BOTH "false"
//     and "true". The identity cannot express which one builder resolves, and
//     the two builder branches resolve it in opposite orders.
//  3. ss/plugin-opts-segment-order — plugin options stay an opaque string, so
//     "obfs-host=x;obfs=http" and "obfs=http;obfs-host=x" hash differently even
//     though they configure the same plugin.
//  4. vmess/legacy/padded-url — accepted here, rejected by builder, which only
//     tries StdEncoding and RawURLEncoding.
//
// Anything not on this list must reproduce byte-for-byte.

// identitySnapshot is the locked, byte-level output of ParseURI for one corpus
// entry. Every field here is part of the persisted contract: IdentityHash is a
// deduplication key in store/node_identity.go and config.deduplicateParsedNodes,
// so a change to CanonicalJSON silently re-identifies every stored node.
type identitySnapshot struct {
	Name          string `json:"name"`
	URI           string `json:"uri"`
	Err           string `json:"err,omitempty"`
	Unsupported   bool   `json:"unsupported,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	Server        string `json:"server,omitempty"`
	Port          uint16 `json:"port,omitempty"`
	CanonicalJSON string `json:"canonical_json,omitempty"`
	Hash          string `json:"hash,omitempty"`
	EndpointKey   string `json:"endpoint_key,omitempty"`
}

// TestIdentityGolden locks the identity contract. It must pass unchanged across
// refactors: if it fails, the refactor changed node identity semantics.
func TestIdentityGolden(t *testing.T) {
	cases := identityCorpus()
	got := make([]identitySnapshot, 0, len(cases))
	for _, c := range cases {
		got = append(got, snapshotOf(c))
	}

	if *updateIdentityGolden {
		encoded, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshal golden: %v", err)
		}
		encoded = append(encoded, '\n')
		if err := os.MkdirAll(filepath.Dir(identityGoldenPath), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(identityGoldenPath, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %d snapshots to %s", len(got), identityGoldenPath)
		return
	}

	raw, err := os.ReadFile(identityGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-identity-golden to create it): %v", err)
	}
	var want []identitySnapshot
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(want) != len(got) {
		t.Fatalf("corpus size changed: golden has %d entries, current corpus has %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			expected, _ := json.Marshal(want[i])
			actual, _ := json.Marshal(got[i])
			t.Errorf("identity drift at case %d (%s)\n uri:  %s\n want: %s\n got:  %s",
				i, want[i].Name, want[i].URI, expected, actual)
		}
	}
}

func snapshotOf(c identityCase) identitySnapshot {
	snapshot := identitySnapshot{Name: c.Name, URI: c.URI}
	result, err := ParseURI(c.URI)
	if err != nil {
		snapshot.Err = err.Error()
		snapshot.Unsupported = errors.Is(err, ErrUnsupportedProtocol)
		return snapshot
	}
	snapshot.Protocol = result.Identity.Protocol
	snapshot.Server = result.Identity.Server
	snapshot.Port = result.Identity.Port
	snapshot.CanonicalJSON = result.CanonicalJSON
	snapshot.Hash = result.Hash
	snapshot.EndpointKey = result.EndpointKey
	return snapshot
}

type identityCase struct {
	Name string
	URI  string
}

// vmessPayload returns a representative VMess v2 JSON object. encoding/json
// sorts map keys, so the base64 payload is deterministic.
func vmessPayload() map[string]any {
	return map[string]any{
		"v": "2", "ps": "示例节点", "add": "Example.COM", "port": "443",
		"id": "4F8A9C0E-1111-2222-3333-444455556666", "aid": "0", "scy": "auto",
		"net": "ws", "type": "none", "host": "cdn.example.com", "path": "/ws",
		"tls": "tls", "sni": "edge.example.com", "alpn": "h2,http/1.1", "fp": "chrome",
	}
}

func vmessLegacyURI(encoding *base64.Encoding, payload map[string]any, fragment string) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	uri := "vmess://" + encoding.EncodeToString(raw)
	if fragment != "" {
		uri += "#" + fragment
	}
	return uri
}

func vmessLegacyWith(overrides map[string]any, fragment string) string {
	payload := vmessPayload()
	for key, value := range overrides {
		if value == nil {
			delete(payload, key)
			continue
		}
		payload[key] = value
	}
	return vmessLegacyURI(base64.StdEncoding, payload, fragment)
}

// identityCorpus covers every protocol, every accepted input shape, and the
// normalisation edge cases that Codec implementations have historically got
// wrong (aliases, defaults, case folding, trailing dots, IDNA, IPv6).
func identityCorpus() []identityCase {
	paddedStd := vmessLegacyURI(base64.StdEncoding, vmessPayload(), "padded-std")
	paddedURL := vmessLegacyURI(base64.URLEncoding, vmessPayload(), "padded-url")
	rawStd := vmessLegacyURI(base64.RawStdEncoding, vmessPayload(), "raw-std")
	rawURL := vmessLegacyURI(base64.RawURLEncoding, vmessPayload(), "raw-url")

	wrappedPayloadRaw, err := json.Marshal(vmessPayload())
	if err != nil {
		panic(err)
	}
	wrappedBody := base64.StdEncoding.EncodeToString(wrappedPayloadRaw)
	wrapped := "vmess://" + wrappedBody[:40] + "\n" + wrappedBody[40:] + "#wrapped"

	ssUserInfoRawURL := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	ssUserInfoStd := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	ssWholeRawURL := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret@example.com:8388"))
	ssWholeStd := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret@example.com:8388"))

	return []identityCase{
		// --- vmess: legacy base64 JSON in all four encodings ---
		{Name: "vmess/legacy/padded-std", URI: paddedStd},
		{Name: "vmess/legacy/padded-url", URI: paddedURL},
		{Name: "vmess/legacy/raw-std", URI: rawStd},
		{Name: "vmess/legacy/raw-url", URI: rawURL},
		{Name: "vmess/legacy/line-wrapped", URI: wrapped},
		{Name: "vmess/legacy/uppercase-uuid-lowered", URI: vmessLegacyWith(nil, "uuid-case")},
		{Name: "vmess/legacy/missing-aid-scy-net", URI: vmessLegacyWith(map[string]any{"aid": nil, "scy": nil, "net": nil}, "")},
		{Name: "vmess/legacy/numeric-aid", URI: vmessLegacyWith(map[string]any{"aid": float64(64)}, "")},
		{Name: "vmess/legacy/grpc-service", URI: vmessLegacyWith(map[string]any{"net": "grpc", "path": "svc-name"}, "")},
		{Name: "vmess/legacy/h2", URI: vmessLegacyWith(map[string]any{"net": "h2"}, "")},
		{Name: "vmess/legacy/tcp-drops-transport-keys", URI: vmessLegacyWith(map[string]any{"net": "tcp"}, "")},
		{Name: "vmess/legacy/no-tls", URI: vmessLegacyWith(map[string]any{"tls": nil, "sni": nil}, "")},
		{Name: "vmess/legacy/alpn-order", URI: vmessLegacyWith(map[string]any{"alpn": "http/1.1,h2"}, "")},
		{Name: "vmess/legacy/no-port", URI: vmessLegacyWith(map[string]any{"port": nil}, "")},
		{Name: "vmess/legacy/no-server", URI: vmessLegacyWith(map[string]any{"add": nil}, "")},
		{Name: "vmess/legacy/no-uuid", URI: vmessLegacyWith(map[string]any{"id": nil}, "")},
		// --- vmess: URL form ---
		{Name: "vmess/url/basic", URI: "vmess://abc@example.com:443#url-basic"},
		{Name: "vmess/url/full", URI: "vmess://abc@example.com:443?alterId=0&encryption=auto&type=ws&host=cdn.example&path=%2Fws&security=tls&sni=edge.example#url-full"},
		{Name: "vmess/url/aid-alias", URI: "vmess://abc@example.com:443?aid=7&type=ws&path=%2Fws#url-aid"},
		{Name: "vmess/url/upper-security", URI: "vmess://abc@example.com:443?security=TLS&sni=edge.example#url-upper"},
		{Name: "vmess/url/allowinsecure-alias", URI: "vmess://abc@example.com:443?security=tls&allowInsecure=1#url-insecure"},
		{Name: "vmess/url/skip-cert-verify-alias", URI: "vmess://abc@example.com:443?security=tls&skip-cert-verify=true#url-skipverify"},
		{Name: "vmess/url/grpc-servicename-keys", URI: "vmess://abc@example.com:443?type=grpc&serviceName=a&servicename=b&grpc-service-name=c#url-grpc"},
		{Name: "vmess/url/no-uuid", URI: "vmess://example.com:443#url-nouuid"},
		{Name: "vmess/url/reality", URI: "vmess://abc@example.com:443?security=reality&pbk=KEY&sid=ab#url-reality"},
		{Name: "vmess/url/unknown-query", URI: "vmess://abc@example.com:443?mux=1&type=ws&path=%2Fws#url-unknown"},

		// --- vless ---
		{Name: "vless/basic", URI: "vless://ABC@Example.COM:443#vless-basic"},
		{Name: "vless/ws-tls", URI: "vless://abc@example.com:443?type=ws&security=tls&path=%2Fproxy&sni=edge.example#vless-ws"},
		{Name: "vless/defaults", URI: "vless://abc@example.com?type=WS&security=TLS&path=%2Fproxy&sni=edge.example#vless-defaults"},
		{Name: "vless/encryption-default", URI: "vless://abc@example.com:443?encryption=none&type=tcp#vless-enc"},
		{Name: "vless/reality", URI: "vless://abc@example.com:443?type=tcp&security=reality&fp=chrome&pbk=PUBKEY&sid=1234&sni=www.example.com&flow=xtls-rprx-vision#vless-reality"},
		{Name: "vless/flow", URI: "vless://abc@example.com:443?type=tcp&security=tls&flow=xtls-rprx-vision#vless-flow"},
		{Name: "vless/packetencoding-camel", URI: "vless://abc@example.com:443?type=tcp&packetEncoding=xudp#vless-pe-camel"},
		{Name: "vless/packetencoding-snake", URI: "vless://abc@example.com:443?type=tcp&packet_encoding=xudp#vless-pe-snake"},
		{Name: "vless/network-alias-only", URI: "vless://abc@example.com:443?network=ws&path=%2Fws#vless-network-alias"},
		{Name: "vless/network-only-no-path", URI: "vless://abc@example.com:443?network=ws#vless-network-only"},
		{Name: "vless/network-grpc-only", URI: "vless://abc@example.com:443?network=grpc&serviceName=svc#vless-network-grpc"},
		{Name: "vless/type-and-network", URI: "vless://abc@example.com:443?type=ws&network=grpc&path=%2Fws#vless-both"},
		{Name: "vless/sni-alias-peer", URI: "vless://abc@example.com:443?type=tcp&security=tls&peer=edge.example#vless-peer"},
		{Name: "vless/sni-alias-servername", URI: "vless://abc@example.com:443?type=tcp&security=tls&server_name=edge.example#vless-servername"},
		{Name: "vless/alpn-order", URI: "vless://abc@example.com:443?type=tcp&security=tls&alpn=h2,http/1.1#vless-alpn"},
		{Name: "vless/grpc-service-name", URI: "vless://abc@example.com:443?type=grpc&security=tls&grpc-service-name=svc#vless-grpc"},
		{Name: "vless/insecure-false", URI: "vless://abc@example.com:443?type=tcp&security=tls&insecure=false#vless-insecure-false"},
		{Name: "vless/insecure-yes", URI: "vless://abc@example.com:443?type=tcp&security=tls&insecure=yes#vless-insecure-yes"},
		{Name: "vless/missing-uuid", URI: "vless://example.com:443#vless-nouuid"},
		{Name: "vless/spanish-host", URI: "vless://abc@例え.EXAMPLE.com:443?type=tcp#vless-idn"},
		{Name: "vless/ipv6", URI: "vless://abc@[2001:db8::1]:8443?type=tcp&security=tls#vless-ipv6"},
		{Name: "vless/trailing-dot", URI: "vless://abc@example.com.:443?type=tcp#vless-dot"},

		// --- trojan ---
		{Name: "trojan/basic", URI: "trojan://one@example.com:443#trojan-basic"},
		{Name: "trojan/defaults", URI: "trojan://one@example.com#trojan-defaults"},
		{Name: "trojan/ws", URI: "trojan://one@example.com:443?type=ws&path=%2Fa&host=cdn.example#trojan-ws"},
		{Name: "trojan/tcp-drops-transport-keys", URI: "trojan://one@example.com:443?type=tcp&path=%2Fa&host=cdn.example#trojan-tcp"},
		{Name: "trojan/insecure", URI: "trojan://one@example.com:443?insecure=1#trojan-insecure"},
		{Name: "trojan/allowInsecure", URI: "trojan://one@example.com:443?allowInsecure=1#trojan-allowinsecure"},
		{Name: "trojan/allowInsecure-wins-over-insecure", URI: "trojan://one@example.com:443?allowInsecure=1&insecure=0#trojan-precedence"},
		{Name: "trojan/skip-cert-verify", URI: "trojan://one@example.com:443?skip-cert-verify=true#trojan-skipverify"},
		{Name: "trojan/missing-password", URI: "trojan://example.com:443#trojan-nopass"},

		// --- shadowsocks ---
		{Name: "ss/sip002-rawurl-userinfo", URI: "ss://" + ssUserInfoRawURL + "@example.com:8388#ss-rawurl"},
		{Name: "ss/sip002-std-userinfo", URI: "ss://" + ssUserInfoStd + "@example.com:8388#ss-std"},
		{Name: "ss/whole-rawurl", URI: "ss://" + ssWholeRawURL + "#ss-whole-rawurl"},
		{Name: "ss/whole-std", URI: "ss://" + ssWholeStd + "#ss-whole-std"},
		{Name: "ss/plain-userinfo", URI: "ss://aes-256-gcm:secret@example.com:8388#ss-plain"},
		{Name: "ss/escaped-password", URI: "ss://aes-256-gcm:se%40cret@example.com:8388#ss-escaped"},
		{Name: "ss/method-uppercase-lowered", URI: "ss://" + base64.RawURLEncoding.EncodeToString([]byte("AES-256-GCM:secret")) + "@example.com:8388#ss-upper"},
		{Name: "ss/default-port", URI: "ss://aes-256-gcm:secret@example.com#ss-default-port"},
		{Name: "ss/plugin", URI: "ss://" + ssUserInfoRawURL + "@example.com:8388?plugin=obfs-local&plugin-opts=obfs%3Dhttp%3Bobfs-host%3Dcdn.example#ss-plugin"},
		{Name: "ss/plugin-opts-snake-alias", URI: "ss://" + ssUserInfoRawURL + "@example.com:8388?plugin=obfs-local&plugin_opts=obfs%3Dhttp#ss-plugin-snake"},
		{Name: "ss/plugin-opts-segment-order", URI: "ss://" + ssUserInfoRawURL + "@example.com:8388?plugin=obfs-local&plugin-opts=obfs-host%3Dcdn.example%3Bobfs%3Dhttp#ss-plugin-order"},
		{Name: "ss/missing-password", URI: "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:")) + "@example.com:8388#ss-nopass"},
		{Name: "ss/missing-method", URI: "ss://" + base64.RawURLEncoding.EncodeToString([]byte(":secret")) + "@example.com:8388#ss-nomethod"},

		// --- hysteria2 ---
		{Name: "hysteria2/basic", URI: "hysteria2://pass@example.com:443#hy2-basic"},
		{Name: "hysteria2/alias-hy2", URI: "hy2://pass@example.com:443#hy2-alias"},
		{Name: "hysteria2/default-port", URI: "hysteria2://pass@example.com#hy2-default-port"},
		{Name: "hysteria2/userinfo-userpass", URI: "hysteria2://user:pass@example.com:443#hy2-userpass"},
		{Name: "hysteria2/full", URI: "hysteria2://pass@example.com:443?sni=edge.example&insecure=1&alpn=h3&obfs=salamander&obfs-password=obfspass&upMbps=100&downMbps=200#hy2-full"},
		{Name: "hysteria2/obfsPassword-alias", URI: "hysteria2://pass@example.com:443?obfs=salamander&obfsPassword=x#hy2-obfspass-alias"},
		{Name: "hysteria2/missing-password", URI: "hysteria2://example.com:443#hy2-nopass"},

		// --- anytls ---
		{Name: "anytls/basic", URI: "anytls://pass@example.com:443#anytls-basic"},
		{Name: "anytls/default-port", URI: "anytls://pass@example.com#anytls-default-port"},
		{Name: "anytls/full", URI: "anytls://pass@example.com:443?sni=edge.example&insecure=1&fp=chrome&alpn=h2#anytls-full"},
		{Name: "anytls/missing-password", URI: "anytls://example.com:443#anytls-nopass"},

		// --- http ---
		{Name: "http/basic", URI: "http://user:pass@example.com:8080#http-basic"},
		{Name: "http/default-port", URI: "http://user:pass@example.com#http-default-port"},
		{Name: "http/tls-default-port", URI: "http://user:pass@example.com?security=tls#http-tls-default-port"},
		{Name: "http/with-path", URI: "http://user:pass@example.com:8080/proxy#http-path"},
		{Name: "http/no-credentials", URI: "http://example.com:8080#http-anon"},

		// --- socks5 ---
		{Name: "socks5/basic", URI: "socks5://user:pass@example.com:1080#socks5-basic"},
		{Name: "socks5/alias-socks", URI: "socks://user:pass@example.com:1080#socks5-alias"},
		{Name: "socks5/default-port", URI: "socks5://user:pass@example.com#socks5-default-port"},
		{Name: "socks5/network-query", URI: "socks5://user:pass@example.com:1080?network=tcp#socks5-network"},

		// --- explicitly unsupported / malformed ---
		{Name: "unsupported/ssr", URI: "ssr://example.com:8388#ssr"},
		{Name: "unsupported/hysteria-v1", URI: "hysteria://pass@example.com:443#hysteria"},
		{Name: "unsupported/unknown-scheme", URI: "wireguard://key@example.com:51820#wg"},
		{Name: "malformed/empty", URI: ""},
		{Name: "malformed/no-scheme", URI: "example.com:443"},
		{Name: "malformed/blank", URI: "   "},
		{Name: "malformed/bom-prefixed", URI: "\ufeffvless://abc@example.com:443?type=tcp#bom"},
	}
}

// TestIdentityKnownCollisionNetworkAlias is an executable record of defect 1
// above: two URIs whose real transport differs share one identity. This is
// data loss, not cosmetics — store/node_identity.go groups by IdentityHash and
// deletes the losing row.
//
// If this test starts failing, the collision was fixed. That is welcome, but it
// means every stored node's hash may have changed: bump IdentityVersion and
// regenerate the golden in the same commit, and treat the rollout as a data
// migration rather than a refactor.
func TestIdentityKnownCollisionNetworkAlias(t *testing.T) {
	withPath, err := ParseURI("vless://abc@example.com:443?network=ws&path=%2Fws")
	if err != nil {
		t.Fatal(err)
	}
	withoutPath, err := ParseURI("vless://abc@example.com:443?network=ws")
	if err != nil {
		t.Fatal(err)
	}
	if withPath.Hash != withoutPath.Hash {
		t.Fatalf("collision is gone; see TestIdentityKnownCollisionNetworkAlias doc comment before adjusting the golden\n%s\n%s",
			withPath.CanonicalJSON, withoutPath.CanonicalJSON)
	}
	t.Logf("known defect confirmed, path is absent from the identity: %s", withPath.CanonicalJSON)
}

// TestIdentityGoldenIsStableAcrossRepeatedRuns guards the snapshot itself: a
// non-deterministic codec (map iteration leaking into output) would otherwise
// make the golden fail intermittently.
func TestIdentityGoldenIsStableAcrossRepeatedRuns(t *testing.T) {
	for _, c := range identityCorpus() {
		first := snapshotOf(c)
		for run := 0; run < 8; run++ {
			if again := snapshotOf(c); again != first {
				t.Fatalf("non-deterministic identity for %s (%s)", c.Name, c.URI)
			}
		}
	}
}

// TestIdentityGoldenCorpusIsStable guards against silent corpus edits that
// would invalidate the snapshot's coverage claims.
func TestIdentityGoldenCorpusIsStable(t *testing.T) {
	cases := identityCorpus()
	seen := make(map[string]struct{}, len(cases))
	protocols := make(map[string]struct{})
	for _, c := range cases {
		if _, dup := seen[c.Name]; dup {
			t.Errorf("duplicate corpus case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
		if result, err := ParseURI(c.URI); err == nil {
			protocols[result.Identity.Protocol] = struct{}{}
		}
	}
	for _, want := range []string{"vmess", "vless", "trojan", "ss", "hysteria2", "anytls", "http", "socks5"} {
		if _, ok := protocols[want]; !ok {
			t.Errorf("corpus no longer covers protocol %q", want)
		}
	}
	if len(cases) < 80 {
		t.Errorf("corpus shrank to %d cases; it must keep broad coverage", len(cases))
	}
}
