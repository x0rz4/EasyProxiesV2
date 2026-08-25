// Package nodecodec provides protocol-aware proxy node identity parsing.
// It deliberately keeps display metadata (notably URI fragments) out of the
// identity while retaining every connection-affecting option it understands.
package nodecodec

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"

	json "easy_proxies/internal/jsonx"

	"golang.org/x/net/idna"
)

const IdentityVersion = 1

var ErrUnsupportedProtocol = errors.New("unsupported proxy protocol")

// Identity is the stable, protocol-aware connection identity persisted as JSON.
// Credentials are included because two accounts on one endpoint are different
// nodes. Callers must never expose CanonicalJSON in an API response or log it.
type Identity struct {
	Version  int                 `json:"version"`
	Protocol string              `json:"protocol"`
	Server   string              `json:"server"`
	Port     uint16              `json:"port"`
	Auth     map[string]string   `json:"auth,omitempty"`
	Options  map[string][]string `json:"options,omitempty"`
}

type Result struct {
	Identity      Identity
	CanonicalJSON string
	Hash          string
	EndpointKey   string
}

// ParseURI validates rawURI and returns its canonical semantic identity.
func ParseURI(rawURI string) (Result, error) {
	rawURI = strings.TrimSpace(strings.TrimPrefix(rawURI, "\ufeff"))
	if rawURI == "" {
		return Result{}, errors.New("empty proxy URI")
	}
	schemeEnd := strings.Index(rawURI, "://")
	if schemeEnd <= 0 {
		return Result{}, errors.New("proxy URI missing scheme")
	}
	scheme := normalizeProtocol(rawURI[:schemeEnd])
	switch scheme {
	case "vmess":
		return parseVMess(rawURI)
	case "ss":
		return parseShadowsocks(rawURI)
	case "vless", "trojan", "hysteria2", "anytls", "http", "socks5":
		return parseURLIdentity(rawURI, scheme)
	case "ssr", "hysteria":
		return Result{}, fmt.Errorf("%w: %s is accepted by some clients but is not runnable by EasyProxies", ErrUnsupportedProtocol, scheme)
	default:
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupportedProtocol, scheme)
	}
}

func normalizeProtocol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "shadowsocks":
		return "ss"
	case "hy2":
		return "hysteria2"
	case "socks":
		return "socks5"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func defaultPort(protocol string) uint16 {
	switch protocol {
	case "http":
		return 80
	case "socks5":
		return 1080
	case "ss":
		return 8388
	default:
		return 443
	}
}

func parseURLIdentity(rawURI, protocol string) (Result, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return Result{}, fmt.Errorf("parse %s URI: %w", protocol, err)
	}
	server, err := normalizeHost(u.Hostname())
	if err != nil || server == "" {
		return Result{}, fmt.Errorf("%s URI missing or invalid server", protocol)
	}
	port := defaultPort(protocol)
	if protocol == "http" && strings.EqualFold(u.Query().Get("security"), "tls") {
		port = 443
	}
	if value := u.Port(); value != "" {
		parsed, parseErr := strconv.ParseUint(value, 10, 16)
		if parseErr != nil || parsed == 0 {
			return Result{}, fmt.Errorf("%s URI has invalid port %q", protocol, value)
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
			return Result{}, fmt.Errorf("%s URI missing UUID", protocol)
		}
		auth["username"] = strings.ToLower(auth["username"])
	case "trojan", "hysteria2", "anytls":
		if auth["username"] == "" {
			return Result{}, fmt.Errorf("%s URI missing password", protocol)
		}
	}

	options := normalizeQuery(protocol, u.Query())
	if security := firstOption(options, "security"); security == "tls" || security == "reality" {
		setDefault(options, "sni", server)
		setDefault(options, "insecure", "false")
	}
	if protocol == "http" && u.EscapedPath() != "" && u.EscapedPath() != "/" {
		options["path"] = []string{u.EscapedPath()}
	}
	identity := Identity{Version: IdentityVersion, Protocol: protocol, Server: server, Port: port, Auth: auth, Options: options}
	return finish(identity)
}

func normalizeQuery(protocol string, values url.Values) map[string][]string {
	result := make(map[string][]string)
	for key, list := range values {
		normalizedKey := normalizeQueryKey(key)
		if isCosmeticQueryKey(normalizedKey) {
			continue
		}
		for _, value := range list {
			value = strings.TrimSpace(value)
			switch normalizedKey {
			case "insecure":
				value = normalizeBool(value)
			case "type", "security", "encryption", "network", "flow", "fp", "packetencoding":
				value = strings.ToLower(value)
			case "alpn":
				parts := splitAndSort(value)
				value = strings.Join(parts, ",")
			}
			result[normalizedKey] = append(result[normalizedKey], value)
		}
	}
	for key := range result {
		sort.Strings(result[key])
		result[key] = uniqueStrings(result[key])
	}

	// Defaults and aliases must canonicalize to the same identity.
	switch protocol {
	case "vless":
		setDefault(result, "encryption", "none")
		setDefault(result, "type", "tcp")
		setDefault(result, "security", "none")
	case "vmess":
		setDefault(result, "encryption", "auto")
		setDefault(result, "alterid", "0")
		setDefault(result, "type", "tcp")
		setDefault(result, "security", "none")
	case "trojan":
		setDefault(result, "type", "tcp")
		setDefault(result, "security", "tls")
	case "hysteria2", "anytls":
		setDefault(result, "security", "tls")
	}
	if security := firstOption(result, "security"); security == "tls" || security == "reality" {
		setDefault(result, "insecure", "false")
	}
	if result["type"] != nil && result["type"][0] == "tcp" {
		delete(result, "path")
		delete(result, "host")
		delete(result, "servicename")
	}
	return result
}

func normalizeQueryKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "allowinsecure", "skip-cert-verify", "skip_cert_verify":
		return "insecure"
	case "peer", "servername", "server_name":
		return "sni"
	case "servicename", "service_name", "grpc-service-name":
		return "servicename"
	case "packetencoding", "packet_encoding":
		return "packetencoding"
	case "alterid", "aid":
		return "alterid"
	case "pluginopts", "plugin_opts":
		return "plugin-opts"
	default:
		return key
	}
}

func isCosmeticQueryKey(key string) bool {
	switch key {
	case "name", "remark", "remarks", "ps":
		return true
	default:
		return false
	}
}

func parseVMess(rawURI string) (Result, error) {
	payload := strings.TrimPrefix(rawURI, rawURI[:strings.Index(rawURI, "://")+3])
	if fragment := strings.IndexByte(payload, '#'); fragment >= 0 {
		payload = payload[:fragment]
	}
	if decoded, ok := decodeBase64(payload); ok {
		var data map[string]any
		if json.Unmarshal(decoded, &data) == nil && stringValue(data["add"]) != "" {
			server, err := normalizeHost(stringValue(data["add"]))
			if err != nil {
				return Result{}, err
			}
			port64, _ := strconv.ParseUint(stringValue(data["port"]), 10, 16)
			if port64 == 0 {
				port64 = 443
			}
			uuid := strings.ToLower(stringValue(data["id"]))
			if uuid == "" {
				return Result{}, errors.New("vmess URI missing UUID")
			}
			q := url.Values{}
			q.Set("alterId", defaultString(stringValue(data["aid"]), "0"))
			q.Set("encryption", defaultString(stringValue(data["scy"]), "auto"))
			q.Set("type", defaultString(stringValue(data["net"]), "tcp"))
			if value := stringValue(data["tls"]); value != "" {
				q.Set("security", value)
			}
			for jsonKey, queryKey := range map[string]string{"host": "host", "path": "path", "sni": "sni", "alpn": "alpn", "fp": "fp"} {
				if value := stringValue(data[jsonKey]); value != "" {
					q.Set(queryKey, value)
				}
			}
			identity := Identity{Version: IdentityVersion, Protocol: "vmess", Server: server, Port: uint16(port64),
				Auth: map[string]string{"username": uuid}, Options: normalizeQuery("vmess", q)}
			if security := firstOption(identity.Options, "security"); security == "tls" || security == "reality" {
				setDefault(identity.Options, "sni", server)
			}
			return finish(identity)
		}
	}
	return parseURLIdentity(rawURI, "vmess")
}

func parseShadowsocks(rawURI string) (Result, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return Result{}, fmt.Errorf("parse shadowsocks URI: %w", err)
	}
	var method, password, server string
	var port uint16
	if u.Hostname() != "" && u.User != nil {
		server, err = normalizeHost(u.Hostname())
		if err != nil {
			return Result{}, err
		}
		port = defaultPort("ss")
		if u.Port() != "" {
			value, parseErr := strconv.ParseUint(u.Port(), 10, 16)
			if parseErr != nil || value == 0 {
				return Result{}, errors.New("shadowsocks URI has invalid port")
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
		decoded, ok := decodeBase64(payload)
		if !ok {
			return Result{}, errors.New("decode shadowsocks payload")
		}
		text := string(decoded)
		at := strings.LastIndex(text, "@")
		if at < 0 {
			return Result{}, errors.New("shadowsocks payload missing endpoint")
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
		return Result{}, err
	}
	options := normalizeQuery("ss", u.Query())
	identity := Identity{Version: IdentityVersion, Protocol: "ss", Server: server, Port: port,
		Auth: map[string]string{"password": password}, Options: options}
	identity.Options["method"] = []string{strings.ToLower(method)}
	return finish(identity)
}

func decodeSSUserInfo(value string) (string, string, error) {
	if decoded, ok := decodeBase64(value); ok {
		return splitMethodPassword(string(decoded))
	}
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return "", "", err
	}
	return splitMethodPassword(decoded)
}

func splitMethodPassword(value string) (string, string, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("shadowsocks userinfo must be method:password")
	}
	return parts[0], parts[1], nil
}

func finish(identity Identity) (Result, error) {
	if len(identity.Auth) == 0 {
		identity.Auth = nil
	}
	if len(identity.Options) == 0 {
		identity.Options = nil
	}
	canonical, err := json.MarshalCanonical(identity)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(canonical)
	return Result{Identity: identity, CanonicalJSON: string(canonical), Hash: "v1:" + hex.EncodeToString(sum[:]),
		EndpointKey: identity.Protocol + "|" + identity.Server + "|" + strconv.Itoa(int(identity.Port))}, nil
}

// FallbackHash preserves an unparsable historical URI without allowing it to
// collide with a semantic identity.
func FallbackHash(rawURI string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawURI)))
	return "raw-v1:" + hex.EncodeToString(sum[:])
}

func normalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return "", nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("invalid server %q: %w", host, err)
	}
	return strings.ToLower(ascii), nil
}

func decodeBase64(value string) ([]byte, bool) {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func normalizeBool(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return "true"
	default:
		return "false"
	}
}

func splitAndSort(value string) []string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
	}
	sort.Strings(parts)
	return uniqueStrings(parts)
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func setDefault(values map[string][]string, key, value string) {
	if len(values[key]) == 0 || values[key][0] == "" {
		values[key] = []string{value}
	}
}

func firstOption(values map[string][]string, key string) string {
	if len(values[key]) == 0 {
		return ""
	}
	return values[key][0]
}
