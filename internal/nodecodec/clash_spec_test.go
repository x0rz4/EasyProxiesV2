package nodecodec

import "testing"

// TestClashProxyImportKeepsEveryModelledField pins the fix for silent
// information loss on the Clash import path.
//
// The old Clash → URI conversion was hand-written per protocol and emitted only
// the fields its author remembered. `AlterId` and `Cipher` were declared on the
// Clash struct and never read, so a vmess node whose cipher was not the default
// was imported, deduplicated and dialled with the wrong cipher — a failure that
// only surfaced as an authentication error much later.
func TestClashProxyImportKeepsEveryModelledField(t *testing.T) {
	spec, err := FromClashProxy(ClashProxy{
		Name: "hk", Type: "vmess", Server: "Example.COM", Port: 443,
		UUID: "4F8A9C0E-1111-2222-3333-444455556666", AlterId: 64, Cipher: "aes-128-gcm",
		Network: "ws", TLS: true, ServerName: "edge.example.com", ClientFingerprint: "chrome",
		Alpn:   []string{"h2", "http/1.1"},
		WSOpts: &ClashWSOptions{Path: "/ws", Headers: map[string]string{"Host": "cdn.example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.AlterID != 64 {
		t.Errorf("alterId was dropped: %d", spec.AlterID)
	}
	if spec.Cipher != "aes-128-gcm" {
		t.Errorf("cipher was dropped: %q", spec.Cipher)
	}
	if spec.Auth.UUID != "4f8a9c0e-1111-2222-3333-444455556666" {
		t.Errorf("uuid was not lower-cased: %q", spec.Auth.UUID)
	}

	// Everything the codec models must survive the Clash → link → Spec trip.
	reparsed, err := FromURI(spec.ToURI())
	if err != nil {
		t.Fatalf("rendered link does not parse: %v\n  %s", err, spec.ToURI())
	}
	if !spec.Equal(reparsed) {
		t.Fatalf("clash import lost information\n  rendered: %s\n  before:   %s\n  after:    %s",
			spec.ToURI(), describe(spec), describe(reparsed))
	}
	if reparsed.AlterID != 64 || reparsed.Cipher != "aes-128-gcm" {
		t.Fatalf("alterId/cipher did not survive: %s", describe(reparsed))
	}

	// The identity must carry them too, or two nodes differing only in cipher
	// would deduplicate into one.
	withDefaultCipher, err := FromClashProxy(ClashProxy{
		Name: "hk", Type: "vmess", Server: "Example.COM", Port: 443,
		UUID: "4F8A9C0E-1111-2222-3333-444455556666", AlterId: 0, Cipher: "auto",
		Network: "ws", TLS: true, ServerName: "edge.example.com", ClientFingerprint: "chrome",
		Alpn:   []string{"h2", "http/1.1"},
		WSOpts: &ClashWSOptions{Path: "/ws", Headers: map[string]string{"Host": "cdn.example.com"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := spec.Result()
	if err != nil {
		t.Fatal(err)
	}
	second, err := withDefaultCipher.Result()
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash == second.Hash {
		t.Fatal("nodes differing in alterId and cipher share an identity")
	}
}

// TestClashProxyImportCoversEveryRunnableProtocol guards the branch table: a
// protocol that silently stops importing is worse than one that never did.
func TestClashProxyImportCoversEveryRunnableProtocol(t *testing.T) {
	entries := []ClashProxy{
		{Name: "vmess", Type: "vmess", Server: "a.example.com", Port: 443, UUID: "u", Network: "tcp"},
		{Name: "vless", Type: "vless", Server: "a.example.com", Port: 443, UUID: "u", Network: "tcp", TLS: true},
		{Name: "trojan", Type: "trojan", Server: "a.example.com", Port: 443, Password: "p"},
		{Name: "ss", Type: "ss", Server: "a.example.com", Port: 8388, Cipher: "aes-256-gcm", Password: "p"},
		{Name: "hy2", Type: "hysteria2", Server: "a.example.com", Port: 443, Password: "p"},
		{Name: "anytls", Type: "anytls", Server: "a.example.com", Port: 443, Password: "p"},
		{Name: "http", Type: "http", Server: "a.example.com", Port: 8080, Username: "u", Password: "p"},
		{Name: "socks5", Type: "socks5", Server: "a.example.com", Port: 1080, Username: "u", Password: "p"},
	}
	for _, entry := range entries {
		spec, err := FromClashProxy(entry)
		if err != nil {
			t.Errorf("%s: import failed: %v", entry.Name, err)
			continue
		}
		reparsed, err := FromURI(spec.ToURI())
		if err != nil {
			t.Errorf("%s: rendered %q does not parse: %v", entry.Name, spec.ToURI(), err)
			continue
		}
		if !spec.Equal(reparsed) {
			t.Errorf("%s: %s != %s\n  rendered: %s", entry.Name, describe(spec), describe(reparsed), spec.ToURI())
		}
	}
}
