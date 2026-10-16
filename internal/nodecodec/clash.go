package nodecodec

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ClashProxy is one entry of a Clash / Mihomo `proxies:` list.
//
// The schema lives here because it is an input format of the codec: importing it
// is a Clash → Spec conversion, exactly like FromURI. It used to live next to the
// converter in package config, which is how the two drifted apart.
//
// Only the fields EasyProxies can actually run are modelled. Two known gaps,
// both inherited rather than introduced: hysteria2 bandwidth (`up` / `down`) and
// the per-protocol option blocks other than ws / grpc / reality are ignored.
type ClashProxy struct {
	Name              string                 `yaml:"name"`
	Type              string                 `yaml:"type"`
	Server            string                 `yaml:"server"`
	Port              int                    `yaml:"port"`
	UUID              string                 `yaml:"uuid"`
	Username          string                 `yaml:"username"`
	Password          string                 `yaml:"password"`
	Cipher            string                 `yaml:"cipher"`
	AlterId           int                    `yaml:"alterId"`
	Network           string                 `yaml:"network"`
	TLS               bool                   `yaml:"tls"`
	SkipCertVerify    bool                   `yaml:"skip-cert-verify"`
	ServerName        string                 `yaml:"servername"`
	SNI               string                 `yaml:"sni"`
	Flow              string                 `yaml:"flow"`
	UDP               bool                   `yaml:"udp"`
	Alpn              []string               `yaml:"alpn"`
	Obfs              string                 `yaml:"obfs"`
	ObfsPassword      string                 `yaml:"obfs-password"`
	WSOpts            *ClashWSOptions        `yaml:"ws-opts"`
	GrpcOpts          *ClashGrpcOptions      `yaml:"grpc-opts"`
	RealityOpts       *ClashRealityOptions   `yaml:"reality-opts"`
	ClientFingerprint string                 `yaml:"client-fingerprint"`
	Plugin            string                 `yaml:"plugin"`
	PluginOpts        map[string]interface{} `yaml:"plugin-opts"`
}

// ClashWSOptions is the websocket sub-block of a Clash proxy.
type ClashWSOptions struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

// ClashGrpcOptions is the gRPC sub-block of a Clash proxy.
type ClashGrpcOptions struct {
	GrpcServiceName string `yaml:"grpc-service-name"`
}

// ClashRealityOptions is the REALITY sub-block of a Clash proxy.
type ClashRealityOptions struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

// FromClashProxy converts one Clash proxy entry into a Spec.
//
// Unlike the hand-written URI formatters this replaces, every field it
// understands survives into the rendered link. The old path dropped vmess
// alterId and cipher outright — `AlterId` and `Cipher` were declared on the
// Clash struct and then never read — so a node imported from Clash silently fell
// back to the default cipher until authentication failed.
//
// The conversion renders the entry to a link and parses it back, so the returned
// Spec is byte-for-byte the spec of the link the importer stores. Filling the
// defaults here instead would mean a second, hand-maintained copy of
// normalisation rules (the default cipher, the default security mode, the SNI
// fallbacks) that could quietly drift from FromURI.
func FromClashProxy(proxy ClashProxy) (Spec, error) {
	draft, err := draftFromClashProxy(proxy)
	if err != nil {
		return Spec{}, err
	}
	spec, err := FromURI(draft.ToURI())
	if err != nil {
		return Spec{}, err
	}
	spec.Name = draft.Name
	return spec, nil
}

func draftFromClashProxy(proxy ClashProxy) (Spec, error) {
	protocol := NormalizeProtocol(proxy.Type)
	server, err := normalizeHost(proxy.Server)
	if err != nil {
		return Spec{}, err
	}
	if server == "" {
		return Spec{}, errors.New("clash proxy missing server")
	}
	port := proxy.Port
	if port <= 0 {
		port = int(DefaultPort(protocol))
	}
	if port > 65535 {
		return Spec{}, fmt.Errorf("clash proxy has invalid port %d", proxy.Port)
	}

	spec := Spec{Protocol: protocol, Server: server, Port: uint16(port), Name: proxy.Name}
	switch protocol {
	case "vmess", "vless":
		spec.Auth.UUID = strings.ToLower(strings.TrimSpace(proxy.UUID))
		if spec.Auth.UUID == "" {
			return Spec{}, errors.New("clash proxy missing uuid")
		}
	case "ss":
		spec.Cipher = strings.ToLower(strings.TrimSpace(proxy.Cipher))
		spec.Auth = Auth{Password: proxy.Password, PasswordSet: true}
		if spec.Cipher == "" || spec.Auth.Password == "" {
			return Spec{}, errors.New("clash ss proxy missing cipher or password")
		}
		spec.Plugin = Plugin{Name: proxy.Plugin, Options: joinPluginOptions(proxy.PluginOpts)}
	case "trojan", "hysteria2", "anytls":
		if proxy.Password == "" {
			return Spec{}, errors.New("clash proxy missing password")
		}
		spec.Auth = Auth{Username: proxy.Password}
	case "http", "socks5":
		spec.Auth = Auth{Username: proxy.Username, Password: proxy.Password, PasswordSet: proxy.Password != ""}
	default:
		// Wrap the shared sentinel rather than wording a user-facing message:
		// importers own the prose (and its language), the codec owns the cause.
		return Spec{}, fmt.Errorf("%w: clash proxy type %q", ErrUnsupportedProtocol, proxy.Type)
	}

	if spec.ParameterIsV2RayTransport() {
		spec.Transport.Type = strings.ToLower(strings.TrimSpace(proxy.Network))
		if spec.Transport.Type == "" {
			spec.Transport.Type = "tcp"
		}
		if proxy.WSOpts != nil {
			spec.Transport.Path = proxy.WSOpts.Path
			spec.Transport.Host = proxy.WSOpts.Headers["Host"]
		}
		if proxy.GrpcOpts != nil {
			spec.Transport.ServiceName = proxy.GrpcOpts.GrpcServiceName
		}
	}

	spec.TLS.Security = "none"
	if proxy.TLS {
		spec.TLS.Security = "tls"
	}
	if proxy.RealityOpts != nil {
		spec.TLS.Security = "reality"
		spec.TLS.PublicKey = proxy.RealityOpts.PublicKey
		spec.TLS.ShortID = proxy.RealityOpts.ShortID
	}
	spec.TLS.Enabled = proxy.TLS || proxy.RealityOpts != nil
	switch protocol {
	case "trojan", "hysteria2", "anytls":
		// These cannot run without TLS, whatever the entry says.
		spec.TLS.Enabled = true
		spec.TLS.Security = "tls"
		if proxy.RealityOpts != nil {
			spec.TLS.Security = "reality"
		}
	case "ss", "socks5":
		spec.TLS.Security = ""
		spec.TLS.Enabled = false
	}
	spec.TLS.ServerName = DefaultString(strings.TrimSpace(proxy.ServerName), strings.TrimSpace(proxy.SNI))
	spec.TLS.Insecure = proxy.SkipCertVerify
	spec.TLS.InsecureStated = proxy.SkipCertVerify
	switch protocol {
	case "hysteria2", "anytls":
		// These fall back to the dialled address when the entry names no SNI.
		// FromURI applies the same fallback; without it here, a Clash entry and
		// the link rendered from it would describe different certificates.
		if spec.TLS.ServerName == "" {
			spec.TLS.ServerName = server
		}
	}
	spec.TLS.ALPN = append([]string(nil), proxy.Alpn...)
	spec.TLS.Fingerprint = proxy.ClientFingerprint

	switch protocol {
	case "vmess":
		spec.AlterID = proxy.AlterId
		spec.Cipher = strings.ToLower(strings.TrimSpace(proxy.Cipher))
	case "vless":
		spec.Flow = proxy.Flow
	case "hysteria2":
		spec.Obfs = proxy.Obfs
		spec.ObfsPassword = proxy.ObfsPassword
	case "http", "socks5":
		spec.Network = strings.ToLower(strings.TrimSpace(proxy.Network))
	}
	return spec, nil
}

// joinPluginOptions renders Clash's plugin-opts mapping back into the wire form
// the link carries. Keys are sorted, which is what the previous implementation
// did as well, so an imported node keeps the same identity it had before.
func joinPluginOptions(options map[string]interface{}) string {
	if len(options) == 0 {
		return ""
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+fmt.Sprint(options[key]))
	}
	return strings.Join(parts, ";")
}
