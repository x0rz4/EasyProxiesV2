package nodecodec

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	json "easy_proxies/internal/jsonx"
)

// Spec is the canonical, lossless description of one proxy node link.
//
// It is the single shape every entry point produces (FromURI, and later the
// Clash / sing-box importers) and every exit point consumes (ToURI here, the
// outbound builder and the Clash renderer elsewhere). Before Spec existed those
// four points each re-parsed the raw string, which is how the same link ended up
// identified one way and dialled another.
//
// Spec deliberately carries two views of the same link:
//
//   - The typed fields (Protocol, Auth, TLS, Transport, ...) are the behaviour
//     view. They are lossless and are what the data plane and the renderers read.
//   - canonicalAuth / canonicalOptions are the equality view: exactly what the
//     versioned normalisation produces, and what backs the persisted
//     IdentityHash. They are NOT re-derived from the typed fields, because the
//     normalisation is deliberately lossy (it drops display metadata, folds
//     aliases and deletes transport keys once a protocol defaults to tcp) and
//     deriving it twice would let two independent implementations drift.
//
// Typed fields are resolved from the raw link, not from the canonical bag. That
// matters: the canonical bag has already lost information the data plane needs.
// `?network=ws&path=/ws` is the clearest case — the canonicaliser defaults
// `type` to tcp and then deletes path/host/servicename, so the websocket survives
// only in the raw query. Reading the raw link is what makes Spec lossless.
type Spec struct {
	// Protocol is the canonical protocol name (ss, hysteria2, socks5, ...).
	Protocol string
	// Server is the normalised host: lower-cased, IDNA-encoded, IP unmapped.
	Server string
	Port   uint16
	// Name is the link's display label. It is never part of the identity.
	Name string

	Auth      Auth
	TLS       TLS
	Transport Transport
	Plugin    Plugin

	// Protocol specific scalars. Each one is connection-affecting, so each must
	// survive every round trip.
	AlterID        int    // vmess
	Cipher         string // vmess scy, vless encryption, ss method
	Flow           string // vless
	PacketEncoding string // vless
	UpMbps         int    // hysteria2
	DownMbps       int    // hysteria2
	Obfs           string // hysteria2
	ObfsPassword   string // hysteria2
	Network        string // http / socks5 allowed networks

	canonicalAuth    map[string]string
	canonicalOptions map[string][]string
}

// Auth holds the credentials a link carries.
type Auth struct {
	// Username is the userinfo component of an authority-form link. For trojan,
	// hysteria2 and anytls there is no username field and everything before "@"
	// is the credential, so it lives here.
	Username string
	// Password is set only when the link carried one.
	Password string
	// PasswordSet records that a password component was present, so an empty
	// password still round-trips instead of being dropped.
	PasswordSet bool
	// UUID is the vmess/vless credential, lower-cased as the identity requires.
	UUID string
}

// TLS holds the transport-layer security settings of a link.
type TLS struct {
	// Enabled reports whether the protocol will actually use TLS. It is true
	// unconditionally for trojan / hysteria2 / anytls, which cannot operate
	// without it — that knowledge lives here so the builder and the Clash
	// renderer cannot disagree about it.
	Enabled bool
	// Security is the normalised security mode: "", none, tls or reality.
	Security string
	// ServerName is the resolved certificate name, including the protocol's
	// fallback to the dialled address where one applies.
	ServerName string
	// Insecure is the link's certificate-verification verdict.
	Insecure bool
	// InsecureStated records whether the link stated a verdict at all. An
	// explicit verdict overrides the global SkipCertVerify setting; an absent one
	// must leave it alone.
	InsecureStated bool
	ALPN           []string
	Fingerprint    string
	// PublicKey / ShortID are the REALITY parameters.
	PublicKey string
	ShortID   string
}

// Transport describes the V2Ray-style transport of vless / vmess / trojan links.
type Transport struct {
	// Type is "", tcp, ws, http, h2, grpc, httpupgrade or xhttp.
	Type        string
	Path        string
	Host        string
	ServiceName string
}

// Plugin describes a shadowsocks plugin (`ss://...?plugin=`).
type Plugin struct {
	Name string
	// Options stays in its wire form ("k=v;k=v"). It is kept as a string rather
	// than a map because it feeds the identity verbatim: parsing it into a map
	// and re-serialising would reorder the segments and change every existing
	// plugin node's hash.
	Options string
}

// ParameterIsV2RayTransport reports whether the protocol uses the type/network
// transport parameters. http and socks5 do not: for them `network` means the set
// of allowed networks, which is a different thing entirely.
func (s Spec) ParameterIsV2RayTransport() bool {
	switch s.Protocol {
	case "vless", "vmess", "trojan":
		return true
	default:
		return false
	}
}

// AuthString returns the single credential trojan, hysteria2 and anytls carry.
// Those protocols have no username field, so whatever the link put before "@" is
// the credential and must be passed to the server unescaped.
func (s Spec) AuthString() string {
	if s.Auth.PasswordSet {
		return s.Auth.Username + ":" + s.Auth.Password
	}
	return s.Auth.Username
}

// FromURI parses a proxy link into a Spec.
func FromURI(rawURI string) (Spec, error) {
	rawURI = TrimLink(rawURI)
	if rawURI == "" {
		return Spec{}, errors.New("empty proxy URI")
	}
	scheme := URIScheme(rawURI)
	if scheme == "" {
		return Spec{}, errors.New("proxy URI missing scheme")
	}
	switch scheme {
	case "vmess":
		return specFromVMess(rawURI)
	case "ss":
		return specFromShadowsocks(rawURI)
	case "vless", "trojan", "hysteria2", "anytls", "http", "socks5":
		return specFromURLIdentity(rawURI, scheme)
	case "ssr", "hysteria":
		return Spec{}, fmt.Errorf("%w: %q is accepted by some clients but is not runnable by EasyProxies", ErrUnsupportedProtocol, scheme)
	default:
		return Spec{}, fmt.Errorf("%w: %q", ErrUnsupportedProtocol, scheme)
	}
}

// Result returns the persisted identity of the spec.
func (s Spec) Result() (Result, error) {
	if s.canonicalAuth == nil && s.canonicalOptions == nil {
		return Result{}, errors.New("spec was not produced by FromURI")
	}
	return finish(Identity{
		Version:  IdentityVersion,
		Protocol: s.Protocol,
		Server:   s.Server,
		Port:     s.Port,
		Auth:     s.canonicalAuth,
		Options:  s.canonicalOptions,
	})
}

func specFromURLIdentity(rawURI, protocol string) (Spec, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return Spec{}, fmt.Errorf("parse %s URI: %w", protocol, err)
	}
	server, err := normalizeHost(u.Hostname())
	if err != nil || server == "" {
		return Spec{}, fmt.Errorf("%s URI missing or invalid server", protocol)
	}
	port := DefaultPort(protocol)
	if protocol == "http" && strings.EqualFold(u.Query().Get("security"), "tls") {
		port = 443
	}
	if value := u.Port(); value != "" {
		parsed, parseErr := strconv.ParseUint(value, 10, 16)
		if parseErr != nil || parsed == 0 {
			return Spec{}, fmt.Errorf("%s URI has invalid port %q", protocol, value)
		}
		port = uint16(parsed)
	}

	auth := map[string]string{}
	if u.User != nil {
		auth["username"] = u.User.Username()
		if password, ok := u.User.Password(); ok {
			auth["password"] = password
		}
	}
	switch protocol {
	case "vless", "vmess":
		if auth["username"] == "" {
			return Spec{}, fmt.Errorf("%s URI missing UUID", protocol)
		}
		auth["username"] = strings.ToLower(auth["username"])
	case "trojan", "hysteria2", "anytls":
		if auth["username"] == "" {
			return Spec{}, fmt.Errorf("%s URI missing password", protocol)
		}
	}

	options := normalizeQuery(protocol, u.Query())
	if security := FirstOption(options, "security"); security == "tls" || security == "reality" {
		setDefault(options, "sni", server)
		setDefault(options, "insecure", "false")
	}
	if protocol == "http" && u.EscapedPath() != "" && u.EscapedPath() != "/" {
		options["path"] = []string{u.EscapedPath()}
	}

	spec := Spec{
		Protocol:         protocol,
		Server:           server,
		Port:             port,
		Name:             u.Fragment,
		canonicalAuth:    auth,
		canonicalOptions: options,
	}
	spec.resolveFields(u, u.Query())
	if protocol == "http" {
		if path := u.EscapedPath(); path != "" {
			spec.Transport.Path = path
		}
	}
	return spec, nil
}

func specFromVMess(rawURI string) (Spec, error) {
	payload := strings.TrimPrefix(rawURI, rawURI[:strings.Index(rawURI, "://")+3])
	if fragment := strings.IndexByte(payload, '#'); fragment >= 0 {
		payload = payload[:fragment]
	}
	if decoded, ok := DecodeBase64Loose(payload); ok {
		var data map[string]any
		if json.Unmarshal(decoded, &data) == nil && CoerceString(data["add"]) != "" {
			server, err := normalizeHost(CoerceString(data["add"]))
			if err != nil {
				return Spec{}, err
			}
			port64, _ := strconv.ParseUint(CoerceString(data["port"]), 10, 16)
			if port64 == 0 {
				port64 = 443
			}
			uuid := strings.ToLower(CoerceString(data["id"]))
			if uuid == "" {
				return Spec{}, errors.New("vmess URI missing UUID")
			}
			q := url.Values{}
			q.Set("alterId", DefaultString(CoerceString(data["aid"]), "0"))
			q.Set("encryption", DefaultString(CoerceString(data["scy"]), "auto"))
			q.Set("type", DefaultString(CoerceString(data["net"]), "tcp"))
			if value := CoerceString(data["tls"]); value != "" {
				q.Set("security", value)
			}
			for jsonKey, queryKey := range map[string]string{"host": "host", "path": "path", "sni": "sni", "alpn": "alpn", "fp": "fp"} {
				if value := CoerceString(data[jsonKey]); value != "" {
					q.Set(queryKey, value)
				}
			}
			options := normalizeQuery("vmess", q)
			if security := FirstOption(options, "security"); security == "tls" || security == "reality" {
				setDefault(options, "sni", server)
			}
			spec := Spec{
				Protocol:         "vmess",
				Server:           server,
				Port:             uint16(port64),
				Name:             vmessFragment(rawURI),
				canonicalAuth:    map[string]string{"username": uuid},
				canonicalOptions: options,
			}
			spec.resolveFields(nil, q)
			// The legacy JSON payload has always resolved the Host header as a
			// certificate-name fallback. The URL form never has. That asymmetry
			// predates Spec and is preserved here rather than normalised, so this
			// refactor stays behaviour-preserving.
			if spec.TLS.Enabled && spec.TLS.ServerName == "" {
				spec.TLS.ServerName = spec.Transport.Host
			}
			return spec, nil
		}
	}
	return specFromURLIdentity(rawURI, "vmess")
}

func vmessFragment(rawURI string) string {
	if index := strings.IndexByte(rawURI, '#'); index >= 0 {
		return rawURI[index+1:]
	}
	return ""
}

func specFromShadowsocks(rawURI string) (Spec, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return Spec{}, fmt.Errorf("parse shadowsocks URI: %w", err)
	}
	var method, password, server string
	var port uint16
	if u.Hostname() != "" && u.User != nil {
		server, err = normalizeHost(u.Hostname())
		if err != nil {
			return Spec{}, err
		}
		port = DefaultPort("ss")
		if u.Port() != "" {
			value, parseErr := strconv.ParseUint(u.Port(), 10, 16)
			if parseErr != nil || value == 0 {
				return Spec{}, errors.New("shadowsocks URI has invalid port")
			}
			port = uint16(value)
		}
		userinfo := ""
		if u.User != nil {
			userinfo = u.User.String()
		}
		method, password, err = decodeSSUserInfo(userinfo)
	} else {
		payload := strings.TrimPrefix(rawURI, rawURI[:strings.Index(rawURI, "://")+3])
		if idx := strings.IndexAny(payload, "#?"); idx >= 0 {
			payload = payload[:idx]
		}
		decoded, ok := DecodeBase64Loose(payload)
		if !ok {
			return Spec{}, errors.New("decode shadowsocks payload")
		}
		text := string(decoded)
		at := strings.LastIndex(text, "@")
		if at < 0 {
			return Spec{}, errors.New("shadowsocks payload missing endpoint")
		}
		method, password, err = splitMethodPassword(text[:at])
		if err == nil {
			hostURL, parseErr := url.Parse("ss://" + text[at+1:])
			if parseErr != nil {
				err = parseErr
			} else {
				server, err = normalizeHost(hostURL.Hostname())
				value, parseErr := strconv.ParseUint(hostURL.Port(), 10, 16)
				if parseErr != nil || value == 0 {
					err = errors.New("shadowsocks payload has invalid port")
				} else {
					port = uint16(value)
				}
			}
		}
	}
	if err != nil || method == "" || password == "" || server == "" {
		if err == nil {
			err = errors.New("shadowsocks URI missing method, password or server")
		}
		return Spec{}, err
	}
	options := normalizeQuery("ss", u.Query())
	options["method"] = []string{strings.ToLower(method)}

	spec := Spec{
		Protocol:         "ss",
		Server:           server,
		Port:             port,
		Name:             u.Fragment,
		Auth:             Auth{Password: password, PasswordSet: true},
		Cipher:           strings.ToLower(method),
		canonicalAuth:    map[string]string{"password": password},
		canonicalOptions: options,
	}
	spec.Plugin = Plugin{
		Name:    LookupOption(u.Query(), "plugin"),
		Options: LookupOption(u.Query(), "plugin-opts", "plugin_opts"),
	}
	return spec, nil
}

// resolveFields fills the behaviour view from the raw query.
//
// values is the raw query of the link; authority may be nil for the legacy vmess
// JSON payload, which carries credentials in the payload rather than the
// authority.
func (s *Spec) resolveFields(authority *url.URL, values url.Values) {
	if authority != nil && authority.User != nil {
		s.Auth.Username = authority.User.Username()
		if password, ok := authority.User.Password(); ok {
			s.Auth.Password = password
			s.Auth.PasswordSet = true
		}
	}
	switch s.Protocol {
	case "vmess", "vless":
		s.Auth.UUID = s.canonicalAuth["username"]
	case "ss":
		s.Plugin = Plugin{
			Name:    LookupOption(values, "plugin"),
			Options: LookupOption(values, "plugin-opts", "plugin_opts"),
		}
	}

	// Certificate verification: the raw query carries the precedence, which the
	// canonical bag has already collapsed into one key holding both values.
	s.TLS.Insecure, s.TLS.InsecureStated = TLSInsecureOverride(values)
	s.TLS.Security = FirstOption(s.canonicalOptions, "security")
	s.TLS.ServerName = TLSServerName(values)
	switch s.Protocol {
	case "hysteria2", "anytls":
		setDefault(s.canonicalOptions, "sni", s.Server)
		if s.TLS.ServerName == "" {
			s.TLS.ServerName = s.Server
		}
	}
	s.TLS.ALPN = splitList(LookupOption(values, "alpn"))
	s.TLS.Fingerprint = LookupOption(values, "fp")
	s.TLS.PublicKey = LookupOption(values, "pbk")
	s.TLS.ShortID = LookupOption(values, "sid")
	switch s.TLS.Security {
	case "tls", "reality":
		s.TLS.Enabled = true
	}
	switch s.Protocol {
	case "trojan", "hysteria2", "anytls":
		// These protocols cannot run without TLS, whatever the link says.
		s.TLS.Enabled = true
	}

	if s.ParameterIsV2RayTransport() {
		s.Transport.Type = strings.ToLower(LookupOption(values, "type", "network"))
		s.Transport.Path = LookupOption(values, "path")
		s.Transport.Host = LookupOption(values, "host")
		s.Transport.ServiceName = LookupOption(values, "servicename", "serviceName", "service_name", "grpc-service-name")
		if s.Transport.Type == "" {
			// An absent type means tcp unless a path implies a websocket. This is
			// the rule the outbound builder has always used; keeping it here gives
			// subrender the same answer.
			if s.Transport.Path != "" {
				s.Transport.Type = "ws"
			} else {
				s.Transport.Type = "tcp"
			}
		}
	}

	switch s.Protocol {
	case "vmess":
		s.Cipher = FirstOption(s.canonicalOptions, "encryption")
		s.AlterID, _ = strconv.Atoi(DefaultString(FirstOption(s.canonicalOptions, "alterid"), "0"))
	case "vless":
		s.Cipher = FirstOption(s.canonicalOptions, "encryption")
		s.Flow = LookupOption(values, "flow")
		s.PacketEncoding = LookupOption(values, "packetencoding", "packetEncoding", "packet_encoding")
	case "hysteria2":
		s.UpMbps, _ = strconv.Atoi(LookupOption(values, "upMbps", "up_mbps"))
		s.DownMbps, _ = strconv.Atoi(LookupOption(values, "downMbps", "down_mbps"))
		s.Obfs = LookupOption(values, "obfs")
		s.ObfsPassword = LookupOption(values, "obfs-password", "obfsPassword", "obfs_password")
	case "http", "socks5":
		s.Network = LookupOption(values, "network")
	}
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Equal reports whether two specs describe the same connection. It compares the
// behaviour view; callers that care about identity should compare Result instead.
func (s Spec) Equal(other Spec) bool {
	if s.Protocol != other.Protocol || s.Server != other.Server || s.Port != other.Port {
		return false
	}
	switch s.Protocol {
	case "vmess", "vless":
		// Username mirrors the authority spelling; UUID is the lower-cased
		// credential and the only one that reaches the server.
		if s.Auth.UUID != other.Auth.UUID {
			return false
		}
	case "ss":
		if s.Auth.Password != other.Auth.Password {
			return false
		}
	case "trojan", "hysteria2", "anytls":
		// No username field: only the joined credential is meaningful.
		if s.AuthString() != other.AuthString() {
			return false
		}
	default:
		if s.Auth != other.Auth {
			return false
		}
	}
	if s.Transport != other.Transport || s.Plugin != other.Plugin {
		return false
	}
	if s.AlterID != other.AlterID || s.Cipher != other.Cipher || s.Flow != other.Flow ||
		s.PacketEncoding != other.PacketEncoding || s.UpMbps != other.UpMbps ||
		s.DownMbps != other.DownMbps || s.Obfs != other.Obfs ||
		s.ObfsPassword != other.ObfsPassword || s.Network != other.Network {
		return false
	}
	return s.TLS.Equal(other.TLS)
}

// Equal reports whether two TLS settings describe the same connection.
func (t TLS) Equal(other TLS) bool {
	if t.Enabled != other.Enabled || t.Security != other.Security || t.ServerName != other.ServerName {
		return false
	}
	if t.Insecure != other.Insecure || t.InsecureStated != other.InsecureStated {
		return false
	}
	if t.Fingerprint != other.Fingerprint || t.PublicKey != other.PublicKey || t.ShortID != other.ShortID {
		return false
	}
	return slices.Equal(t.ALPN, other.ALPN)
}

// ToURI renders the spec back to a link.
//
// Rendering is canonical: it emits the resolved semantics under the canonical
// spelling, so FromURI(ToURI(spec)) describes the same connection. That is
// strictly stronger than the old Clash → URI conversion, which silently dropped
// every parameter its hand-written formatter did not know about (vmess alterId
// and cipher among them).
func (s Spec) ToURI() string {
	u := &url.URL{
		Scheme:   s.Protocol,
		Host:     net.JoinHostPort(s.Server, strconv.Itoa(int(s.Port))),
		RawQuery: s.queryValues().Encode(),
	}
	if s.Name != "" {
		u.Fragment = s.Name
	}
	switch s.Protocol {
	case "vmess", "vless":
		u.User = url.User(s.Auth.UUID)
	case "ss":
		u.User = url.User(base64.RawURLEncoding.EncodeToString([]byte(s.Cipher + ":" + s.Auth.Password)))
	case "trojan", "hysteria2", "anytls":
		if s.Auth.PasswordSet {
			u.User = url.UserPassword(s.Auth.Username, s.Auth.Password)
		} else {
			u.User = url.User(s.Auth.Username)
		}
	case "http", "socks5":
		if s.Auth.Username != "" || s.Auth.PasswordSet {
			u.User = url.UserPassword(s.Auth.Username, s.Auth.Password)
		}
		if s.Protocol == "http" {
			u.Path = s.Transport.Path
		}
	}
	return u.String()
}

func (s Spec) queryValues() url.Values {
	q := url.Values{}
	if s.TLS.Security != "" && s.TLS.Security != "none" {
		q.Set("security", s.TLS.Security)
	}
	if s.TLS.ServerName != "" {
		q.Set("sni", s.TLS.ServerName)
	}
	if len(s.TLS.ALPN) > 0 {
		q.Set("alpn", strings.Join(s.TLS.ALPN, ","))
	}
	if s.TLS.Fingerprint != "" {
		q.Set("fp", s.TLS.Fingerprint)
	}
	if s.TLS.InsecureStated {
		q.Set("insecure", boolFlag(s.TLS.Insecure))
	}
	if s.TLS.PublicKey != "" {
		q.Set("pbk", s.TLS.PublicKey)
	}
	if s.TLS.ShortID != "" {
		q.Set("sid", s.TLS.ShortID)
	}
	if s.ParameterIsV2RayTransport() {
		if s.Transport.Type != "" {
			q.Set("type", s.Transport.Type)
		}
		if s.Transport.Path != "" {
			q.Set("path", s.Transport.Path)
		}
		if s.Transport.Host != "" {
			q.Set("host", s.Transport.Host)
		}
		if s.Transport.ServiceName != "" {
			q.Set("serviceName", s.Transport.ServiceName)
		}
	}
	switch s.Protocol {
	case "vmess":
		q.Set("alterId", strconv.Itoa(s.AlterID))
		q.Set("encryption", DefaultString(s.Cipher, "auto"))
	case "vless":
		if s.Flow != "" {
			q.Set("flow", s.Flow)
		}
		if s.PacketEncoding != "" {
			q.Set("packetencoding", s.PacketEncoding)
		}
	case "hysteria2":
		if s.Obfs != "" {
			q.Set("obfs", s.Obfs)
			q.Set("obfs-password", s.ObfsPassword)
		}
		if s.UpMbps > 0 {
			q.Set("upMbps", strconv.Itoa(s.UpMbps))
		}
		if s.DownMbps > 0 {
			q.Set("downMbps", strconv.Itoa(s.DownMbps))
		}
	case "ss":
		if s.Plugin.Name != "" {
			q.Set("plugin", s.Plugin.Name)
			if s.Plugin.Options != "" {
				q.Set("plugin-opts", s.Plugin.Options)
			}
		}
	case "socks5":
		if s.Network != "" {
			q.Set("network", s.Network)
		}
	}
	return q
}

func boolFlag(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
