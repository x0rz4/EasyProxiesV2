// Package subrender converts EasyProxies node URIs into compact Clash/Mihomo
// subscription documents. It intentionally has no runtime dependencies so the
// public subscription handler can reuse it without creating package cycles.
package subrender

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type ClashDocument struct {
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []map[string]any `yaml:"proxy-groups"`
	Rules       []string         `yaml:"rules"`
}

// RenderClash renders a minimal complete configuration accepted by Clash and
// Mihomo. Fixed groups use fallback; random groups use load-balance.
func RenderClash(groupName, dispatchMode string, proxies []map[string]any) ([]byte, error) {
	if len(proxies) == 0 {
		return nil, errors.New("no proxies to render")
	}
	names := make([]string, 0, len(proxies))
	for _, proxy := range proxies {
		if name, ok := proxy["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	groupType := "fallback"
	group := map[string]any{"name": groupName, "type": groupType, "proxies": names,
		"url": "http://www.gstatic.com/generate_204", "interval": 300}
	if dispatchMode == "random" {
		group["type"] = "load-balance"
		group["strategy"] = "round-robin"
	}
	return yaml.Marshal(ClashDocument{Proxies: proxies, ProxyGroups: []map[string]any{group}, Rules: []string{"MATCH," + groupName}})
}

// EntryProxy creates a single proxy pointing at the public group listener.
func EntryProxy(name, protocol, host string, port uint16, username, password string) map[string]any {
	proxyType := "socks5"
	if protocol == "http" {
		proxyType = "http"
	}
	proxy := map[string]any{"name": name, "type": proxyType, "server": host, "port": port, "udp": true}
	if username != "" {
		proxy["username"] = username
		proxy["password"] = password
	}
	return proxy
}

// EntryURI returns a standard URI for the public group listener. Mixed
// listeners are represented as SOCKS5 because that is broadly importable.
func EntryURI(name, protocol, host string, port uint16, username, password string) string {
	scheme := "socks5"
	if protocol == "http" {
		scheme = "http"
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(int(port))), Fragment: name}
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u.String()
}

// ClashProxy converts one supported upstream URI into a Clash/Mihomo proxy.
func ClashProxy(name, rawURI string) (map[string]any, error) {
	lower := strings.ToLower(rawURI)
	if strings.HasPrefix(lower, "vmess://") {
		return vmessProxy(name, rawURI)
	}
	if strings.HasPrefix(lower, "ss://") {
		return shadowsocksProxy(name, rawURI)
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return nil, err
	}
	host, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	base := map[string]any{"name": name, "server": host, "port": port, "udp": true}
	switch strings.ToLower(u.Scheme) {
	case "vless":
		base["type"] = "vless"
		base["uuid"] = username(u)
		copyIf(base, "flow", q.Get("flow"))
		applyTLS(base, q)
		applyTransport(base, q)
	case "trojan":
		base["type"] = "trojan"
		base["password"] = username(u)
		applyTLS(base, q)
		applyTransport(base, q)
	case "hysteria2", "hy2":
		base["type"] = "hysteria2"
		base["password"] = username(u)
		copyIf(base, "sni", first(q.Get("sni"), q.Get("peer")))
		copyBool(base, "skip-cert-verify", q.Get("insecure"))
		copyIf(base, "obfs", q.Get("obfs"))
		copyIf(base, "obfs-password", first(q.Get("obfs-password"), q.Get("obfsPassword")))
	case "hysteria":
		base["type"] = "hysteria"
		copyIf(base, "auth-str", first(username(u), q.Get("auth")))
		copyIf(base, "sni", first(q.Get("sni"), q.Get("peer")))
		copyIf(base, "protocol", q.Get("protocol"))
		copyIf(base, "up", q.Get("up"))
		copyIf(base, "down", q.Get("down"))
		copyBool(base, "skip-cert-verify", q.Get("insecure"))
	case "anytls":
		base["type"] = "anytls"
		base["password"] = username(u)
		copyIf(base, "sni", q.Get("sni"))
		copyIf(base, "client-fingerprint", q.Get("fp"))
		copyBool(base, "skip-cert-verify", q.Get("insecure"))
	case "http", "https":
		base["type"] = "http"
		applyUser(base, u)
		if strings.EqualFold(u.Scheme, "https") {
			base["tls"] = true
		}
	case "socks5", "socks":
		base["type"] = "socks5"
		applyUser(base, u)
	default:
		return nil, fmt.Errorf("unsupported clash URI scheme %q", u.Scheme)
	}
	return base, nil
}

func vmessProxy(name, rawURI string) (map[string]any, error) {
	decoded, err := decodeBase64(strings.TrimPrefix(rawURI, "vmess://"))
	if err != nil {
		return nil, fmt.Errorf("decode vmess: %w", err)
	}
	var value map[string]any
	if err := json.Unmarshal(decoded, &value); err != nil {
		return nil, fmt.Errorf("decode vmess json: %w", err)
	}
	server := stringValue(value["add"])
	port, _ := strconv.Atoi(stringValue(value["port"]))
	if server == "" || port == 0 {
		return nil, errors.New("vmess server or port missing")
	}
	proxy := map[string]any{"name": name, "type": "vmess", "server": server, "port": port,
		"uuid": stringValue(value["id"]), "alterId": intValue(value["aid"]), "cipher": first(stringValue(value["scy"]), "auto"), "udp": true}
	network := stringValue(value["net"])
	if network != "" && network != "tcp" {
		proxy["network"] = network
	}
	if strings.EqualFold(stringValue(value["tls"]), "tls") {
		proxy["tls"] = true
		copyIf(proxy, "servername", first(stringValue(value["sni"]), stringValue(value["host"])))
	}
	if network == "ws" {
		ws := map[string]any{}
		copyIf(ws, "path", stringValue(value["path"]))
		if host := stringValue(value["host"]); host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		proxy["ws-opts"] = ws
	} else if network == "grpc" {
		proxy["grpc-opts"] = map[string]any{"grpc-service-name": stringValue(value["path"])}
	}
	return proxy, nil
}

func shadowsocksProxy(name, rawURI string) (map[string]any, error) {
	body := strings.TrimPrefix(rawURI, "ss://")
	if hash := strings.IndexByte(body, '#'); hash >= 0 {
		body = body[:hash]
	}
	var plugin string
	if query := strings.IndexByte(body, '?'); query >= 0 {
		values, _ := url.ParseQuery(body[query+1:])
		plugin = values.Get("plugin")
		body = body[:query]
	}
	if !strings.Contains(body, "@") {
		decoded, err := decodeBase64(body)
		if err != nil {
			return nil, err
		}
		body = string(decoded)
	}
	at := strings.LastIndex(body, "@")
	if at < 0 {
		return nil, errors.New("invalid shadowsocks URI")
	}
	credentials, endpoint := body[:at], body[at+1:]
	if decoded, err := decodeBase64(credentials); err == nil {
		credentials = string(decoded)
	}
	credentials, _ = url.PathUnescape(credentials)
	colon := strings.IndexByte(credentials, ':')
	if colon < 1 {
		return nil, errors.New("invalid shadowsocks credentials")
	}
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(portText)
	proxy := map[string]any{"name": name, "type": "ss", "server": host, "port": port,
		"cipher": credentials[:colon], "password": credentials[colon+1:], "udp": true}
	if plugin != "" {
		parts := strings.Split(plugin, ";")
		proxy["plugin"] = parts[0]
		if len(parts) > 1 {
			proxy["plugin-opts"] = strings.Join(parts[1:], ";")
		}
	}
	return proxy, nil
}

func hostPort(u *url.URL) (string, int, error) {
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if host == "" || port == 0 {
		return "", 0, errors.New("server or port missing")
	}
	return host, port, nil
}

func applyTLS(proxy map[string]any, q url.Values) {
	security := strings.ToLower(q.Get("security"))
	if security == "tls" || security == "reality" {
		proxy["tls"] = true
		copyIf(proxy, "servername", first(q.Get("sni"), q.Get("peer")))
		copyIf(proxy, "client-fingerprint", q.Get("fp"))
		copyBool(proxy, "skip-cert-verify", q.Get("insecure"))
	}
	if security == "reality" {
		reality := map[string]any{}
		copyIf(reality, "public-key", q.Get("pbk"))
		copyIf(reality, "short-id", q.Get("sid"))
		proxy["reality-opts"] = reality
	}
}

func applyTransport(proxy map[string]any, q url.Values) {
	network := strings.ToLower(first(q.Get("type"), q.Get("network")))
	if network == "" || network == "tcp" {
		return
	}
	proxy["network"] = network
	switch network {
	case "ws":
		opts := map[string]any{}
		copyIf(opts, "path", q.Get("path"))
		if host := q.Get("host"); host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		proxy["ws-opts"] = opts
	case "grpc":
		proxy["grpc-opts"] = map[string]any{"grpc-service-name": first(q.Get("serviceName"), q.Get("service_name"))}
	}
}

func applyUser(proxy map[string]any, u *url.URL) {
	if u.User == nil {
		return
	}
	proxy["username"] = u.User.Username()
	if password, ok := u.User.Password(); ok {
		proxy["password"] = password
	}
}

func username(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	return u.User.Username()
}
func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func copyIf(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}
func copyBool(target map[string]any, key, value string) {
	if value == "1" || strings.EqualFold(value, "true") {
		target[key] = true
	}
}
func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}
func intValue(value any) int { result, _ := strconv.Atoi(stringValue(value)); return result }
func decodeBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64")
}
