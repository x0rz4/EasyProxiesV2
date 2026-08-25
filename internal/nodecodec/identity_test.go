package nodecodec

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestSemanticIdentityIgnoresPresentationAndQueryOrder(t *testing.T) {
	a, err := ParseURI("vless://ABC@Example.COM:443?type=ws&security=tls&path=%2Fproxy&sni=edge.example#A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseURI("vless://abc@example.com?SNI=edge.example&path=%2Fproxy&security=tls&type=WS#B")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash mismatch\n%s\n%s", a.CanonicalJSON, b.CanonicalJSON)
	}
}

func TestSemanticIdentityKeepsConnectionDifferences(t *testing.T) {
	a, _ := ParseURI("trojan://one@example.com:443?type=ws&path=/a")
	b, _ := ParseURI("trojan://two@example.com:443?type=ws&path=/a")
	c, _ := ParseURI("trojan://one@example.com:443?type=ws&path=/b")
	if a.Hash == b.Hash || a.Hash == c.Hash {
		t.Fatal("connection differences were merged")
	}
	if a.EndpointKey != b.EndpointKey {
		t.Fatal("same endpoint should share endpoint key")
	}
}

func TestVMessLegacyAndURLHaveSameIdentity(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"v": "2", "add": "example.com", "port": "443", "id": "ABC", "aid": "0", "scy": "auto", "net": "ws", "host": "cdn.example", "path": "/ws", "tls": "tls", "sni": "edge.example"})
	legacy := "vmess://" + base64.StdEncoding.EncodeToString(payload) + "#legacy"
	urlForm := "vmess://abc@example.com:443?alterId=0&encryption=auto&type=ws&host=cdn.example&path=%2Fws&security=tls&sni=edge.example#url"
	a, err := ParseURI(legacy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseURI(urlForm)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash mismatch\n%s\n%s", a.CanonicalJSON, b.CanonicalJSON)
	}
}

func TestShadowsocksSIP002Forms(t *testing.T) {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	full := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret@example.com:8388"))
	a, err := ParseURI("ss://" + userinfo + "@example.com:8388#one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseURI("ss://" + full + "#two")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hash mismatch\n%s\n%s", a.CanonicalJSON, b.CanonicalJSON)
	}
}
