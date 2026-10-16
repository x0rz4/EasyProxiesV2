// Package subrender converts EasyProxies node URIs into compact Clash/Mihomo
// subscription documents. It intentionally has no runtime dependencies so the
// public subscription handler can reuse it without creating package cycles.
package subrender

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"easy_proxies/internal/nodecodec"

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
//
// It parses through nodecodec so the proxy it emits and the outbound the data
// plane builds can never disagree; rendering itself lives in ClashSpec, which
// consumes the parsed Spec directly.
func ClashProxy(name, rawURI string) (map[string]any, error) {
	spec, err := nodecodec.FromURI(rawURI)
	if err != nil {
		return nil, err
	}
	return ClashSpec(name, spec)
}

// ClashSpec renders one parsed node into a Clash/Mihomo proxy entry.
//
// Everything it reads comes from the shared Spec, which was resolved once from
// the link. Subrender used to carry its own vmess and shadowsocks parsers and
// its own option lookups, which is how an exported subscription could advertise
// a websocket while the running outbound dialled raw TCP.
func ClashSpec(name string, spec nodecodec.Spec) (map[string]any, error) {
	proxy := map[string]any{
		"name": name, "server": spec.Server, "port": spec.Port, "udp": true,
	}
	switch spec.Protocol {
	case "vmess":
		proxy["type"] = "vmess"
		proxy["uuid"] = spec.Auth.UUID
		proxy["alterId"] = spec.AlterID
		proxy["cipher"] = nodecodec.DefaultString(spec.Cipher, "auto")
		applyTransport(proxy, spec)
		applyTLS(proxy, spec)
	case "vless":
		proxy["type"] = "vless"
		proxy["uuid"] = spec.Auth.UUID
		copyIf(proxy, "flow", spec.Flow)
		applyTransport(proxy, spec)
		applyTLS(proxy, spec)
	case "trojan":
		proxy["type"] = "trojan"
		proxy["password"] = spec.AuthString()
		applyTransport(proxy, spec)
		applyTLS(proxy, spec)
	case "ss":
		proxy["type"] = "ss"
		proxy["cipher"] = spec.Cipher
		proxy["password"] = spec.Auth.Password
		if spec.Plugin.Name != "" {
			proxy["plugin"] = spec.Plugin.Name
			if options := clashPluginOptions(spec.Plugin.Options); len(options) > 0 {
				proxy["plugin-opts"] = options
			}
		}
	case "hysteria2":
		proxy["type"] = "hysteria2"
		proxy["password"] = spec.AuthString()
		copyIf(proxy, "sni", spec.TLS.ServerName)
		copyIf(proxy, "obfs", spec.Obfs)
		copyIf(proxy, "obfs-password", spec.ObfsPassword)
		if spec.TLS.Insecure {
			proxy["skip-cert-verify"] = true
		}
	case "anytls":
		proxy["type"] = "anytls"
		proxy["password"] = spec.AuthString()
		copyIf(proxy, "sni", spec.TLS.ServerName)
		copyIf(proxy, "client-fingerprint", spec.TLS.Fingerprint)
		if spec.TLS.Insecure {
			proxy["skip-cert-verify"] = true
		}
	case "http":
		proxy["type"] = "http"
		copyIf(proxy, "username", spec.Auth.Username)
		copyIf(proxy, "password", spec.Auth.Password)
		if spec.TLS.Enabled {
			proxy["tls"] = true
		}
	case "socks5":
		proxy["type"] = "socks5"
		copyIf(proxy, "username", spec.Auth.Username)
		copyIf(proxy, "password", spec.Auth.Password)
	default:
		return nil, fmt.Errorf("unsupported clash protocol %q", spec.Protocol)
	}
	return proxy, nil
}

func applyTLS(proxy map[string]any, spec nodecodec.Spec) {
	if !spec.TLS.Enabled {
		return
	}
	proxy["tls"] = true
	copyIf(proxy, "servername", spec.TLS.ServerName)
	copyIf(proxy, "client-fingerprint", spec.TLS.Fingerprint)
	if spec.TLS.Insecure {
		proxy["skip-cert-verify"] = true
	}
	if len(spec.TLS.ALPN) > 0 {
		proxy["alpn"] = spec.TLS.ALPN
	}
	if spec.TLS.Security == "reality" {
		reality := map[string]any{}
		copyIf(reality, "public-key", spec.TLS.PublicKey)
		copyIf(reality, "short-id", spec.TLS.ShortID)
		proxy["reality-opts"] = reality
	}
}

func applyTransport(proxy map[string]any, spec nodecodec.Spec) {
	switch spec.Transport.Type {
	case "", "tcp":
		return
	}
	proxy["network"] = spec.Transport.Type
	switch spec.Transport.Type {
	case "ws":
		options := map[string]any{}
		copyIf(options, "path", spec.Transport.Path)
		if spec.Transport.Host != "" {
			options["headers"] = map[string]any{"Host": spec.Transport.Host}
		}
		proxy["ws-opts"] = options
	case "grpc":
		proxy["grpc-opts"] = map[string]any{"grpc-service-name": spec.Transport.ServiceName}
	case "h2":
		options := map[string]any{}
		copyIf(options, "path", spec.Transport.Path)
		if spec.Transport.Host != "" {
			options["host"] = []string{spec.Transport.Host}
		}
		proxy["h2-opts"] = options
	}
}

// clashPluginOptions renders a shadowsocks plugin option string ("k=v;k=v") as
// the map Mihomo expects.
//
// The previous implementation wrote the joined string back into the proxy, so an
// ss node with a plugin was exported as an entry Clash cannot read, and the
// Clash → URI → Clash round trip silently changed the field's type.
func clashPluginOptions(value string) map[string]any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	options := map[string]any{}
	for _, segment := range strings.Split(value, ";") {
		key, raw, found := strings.Cut(segment, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "true" || raw == "false" {
			options[key] = raw == "true"
			continue
		}
		options[key] = raw
	}
	return options
}

func copyIf(target map[string]any, key, value string) {
	if value != "" {
		target[key] = value
	}
}
