// Package nodecodec provides protocol-aware proxy node identity parsing.
// It deliberately keeps display metadata (notably URI fragments) out of the
// identity while retaining every connection-affecting option it understands.
package nodecodec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
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
//
// It is a thin wrapper over FromURI so that the identity and the behaviour view
// can never be produced by two different parsers.
func ParseURI(rawURI string) (Result, error) {
	spec, err := FromURI(rawURI)
	if err != nil {
		return Result{}, err
	}
	return spec.Result()
}

// NormalizeProtocol folds the protocol aliases clients emit (shadowsocks, hy2,
// socks) onto the canonical name the rest of the codebase switches on.
func NormalizeProtocol(value string) string {
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

// DefaultPort returns the port a link implies when it omits one. Every codec
// path must use it: subrender used to reject a portless link outright while the
// identity happily assigned it a default, so the same node existed and could not
// be exported.
func DefaultPort(protocol string) uint16 {
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
		// Canonical JSON needs a stable, duplicate-free ordering: sort each
		// option list and collapse the runs that sorting creates.
		slices.Sort(result[key])
		result[key] = slices.Compact(result[key])
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
	if security := FirstOption(result, "security"); security == "tls" || security == "reality" {
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

func decodeSSUserInfo(value string) (string, string, error) {
	if decoded, ok := DecodeBase64Loose(value); ok {
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

func splitAndSort(value string) []string {
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
	}
	slices.Sort(parts)
	return slices.Compact(parts)
}

func normalizeBool(value string) string {
	if Truthy(value) {
		return "true"
	}
	return "false"
}

func setDefault(values map[string][]string, key, value string) {
	if len(values[key]) == 0 || values[key][0] == "" {
		values[key] = []string{value}
	}
}
