package subrender

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestClashProxyVLESSRealityWebsocket(t *testing.T) {
	proxy, err := ClashProxy("HK", "vless://uuid@example.com:443?security=reality&sni=example.com&pbk=public&sid=abcd&fp=chrome&type=ws&path=%2Fws&host=cdn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if proxy["type"] != "vless" || proxy["uuid"] != "uuid" || proxy["network"] != "ws" || proxy["tls"] != true {
		t.Fatalf("unexpected proxy: %#v", proxy)
	}
	if _, ok := proxy["reality-opts"].(map[string]any); !ok {
		t.Fatalf("missing reality options: %#v", proxy)
	}
}

func TestClashProxyVMessAndRender(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"v": "2", "ps": "demo", "add": "vmess.example", "port": "443", "id": "uuid", "aid": "0", "net": "ws", "path": "/ws", "host": "cdn.example", "tls": "tls"})
	proxy, err := ClashProxy("VMess", "vmess://"+base64.RawStdEncoding.EncodeToString(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, err := RenderClash("香港组", "fixed", []map[string]any{proxy})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{"type: vmess", "type: fallback", "MATCH,香港组", "ws-opts"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in:\n%s", expected, text)
		}
	}
}

func TestEntryURIUsesSocksForMixed(t *testing.T) {
	uri := EntryURI("HK Entry", "mixed", "203.0.113.1", 10002, "alice", "secret")
	if !strings.HasPrefix(uri, "socks5://alice:secret@203.0.113.1:10002") {
		t.Fatalf("unexpected URI: %s", uri)
	}
}
