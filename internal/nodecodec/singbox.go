package nodecodec

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Sentinel causes returned by the non-URI importers.
//
// The importers own the user-facing prose and its language, so the codec reports
// a cause rather than a message. Callers match with errors.Is.
var (
	// ErrLossyConversion means the entry cannot be represented without dropping
	// connection-affecting information.
	ErrLossyConversion = errors.New("cannot be converted losslessly")
	// ErrMissingEndpoint means the entry has no usable server/port pair.
	ErrMissingEndpoint = errors.New("missing server endpoint")
	// ErrMissingCredentials means the entry has no credential for its protocol.
	ErrMissingCredentials = errors.New("missing credentials")
)

// MapString reads a string-ish field out of a decoded JSON object. The codec's
// import formats are untyped maps, and hand-written configs routinely use a
// number where a string is expected, so reads must be total.
func MapString(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	if list, ok := value.([]any); ok && len(list) > 0 {
		return CoerceString(list[0])
	}
	if list, ok := value.([]string); ok && len(list) > 0 {
		return list[0]
	}
	return CoerceString(value)
}

// MapInt reads an integer field out of a decoded JSON object.
func MapInt(values map[string]any, key string) int {
	value := MapString(values, key)
	parsed, _ := strconv.Atoi(strings.TrimSuffix(value, ".0"))
	return parsed
}

// MapBool reads a boolean field out of a decoded JSON object.
func MapBool(values map[string]any, key string) bool {
	value, ok := values[key]
	if !ok {
		return false
	}
	if flag, ok := value.(bool); ok {
		return flag
	}
	return Truthy(fmt.Sprint(value))
}

// MapMap reads a nested object out of a decoded JSON object.
func MapMap(values map[string]any, key string) map[string]any {
	nested, _ := values[key].(map[string]any)
	return nested
}

// MapStringSlice reads a list of strings out of a decoded JSON object.
func MapStringSlice(values map[string]any, key string) []string {
	if list, ok := values[key].([]string); ok {
		return append([]string(nil), list...)
	}
	raw, _ := values[key].([]any)
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		result = append(result, CoerceString(value))
	}
	return result
}

// FromSingBoxOutbound converts one sing-box `outbounds` entry into a Spec.
//
// Like FromClashProxy it renders to a link and parses it back, so the Spec is
// exactly the spec of the link the importer stores; the defaults therefore live
// in one place instead of being restated per importer.
func FromSingBoxOutbound(outbound map[string]any) (Spec, error) {
	draft, err := draftFromSingBoxOutbound(outbound)
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

func draftFromSingBoxOutbound(outbound map[string]any) (Spec, error) {
	rawType := MapString(outbound, "type")
	protocol := NormalizeProtocol(rawType)
	switch protocol {
	case "vmess", "vless", "trojan", "ss", "hysteria2", "anytls", "http", "socks5":
	default:
		return Spec{}, fmt.Errorf("%w: sing-box outbound type %q", ErrUnsupportedProtocol, rawType)
	}
	if MapString(outbound, "detour") != "" {
		return Spec{}, fmt.Errorf("%w: outbound carries detour", ErrLossyConversion)
	}
	server, err := normalizeHost(MapString(outbound, "server"))
	if err != nil {
		return Spec{}, err
	}
	port := MapInt(outbound, "server_port")
	if server == "" || port <= 0 || port > 65535 {
		return Spec{}, ErrMissingEndpoint
	}

	spec := Spec{Protocol: protocol, Server: server, Port: uint16(port), Name: MapString(outbound, "tag")}
	switch protocol {
	case "vmess", "vless":
		spec.Auth.UUID = strings.ToLower(strings.TrimSpace(MapString(outbound, "uuid")))
		if spec.Auth.UUID == "" {
			return Spec{}, fmt.Errorf("%w: %s outbound has no uuid", ErrMissingCredentials, protocol)
		}
		spec.Flow = MapString(outbound, "flow")
		spec.PacketEncoding = MapString(outbound, "packet_encoding")
	case "ss":
		method, password := MapString(outbound, "method"), MapString(outbound, "password")
		if method == "" || password == "" {
			return Spec{}, fmt.Errorf("%w: ss outbound has no method/password", ErrMissingCredentials)
		}
		spec.Cipher = strings.ToLower(method)
		spec.Auth = Auth{Password: password, PasswordSet: true}
		spec.Plugin = Plugin{
			Name:    MapString(outbound, "plugin"),
			Options: pluginOptionsString(outbound["plugin_opts"]),
		}
	case "trojan", "hysteria2", "anytls":
		password := MapString(outbound, "password")
		if password == "" {
			return Spec{}, fmt.Errorf("%w: %s outbound has no password", ErrMissingCredentials, protocol)
		}
		spec.Auth = Auth{Username: password}
	case "http", "socks5":
		spec.Auth = Auth{Username: MapString(outbound, "username"), Password: MapString(outbound, "password")}
		spec.Auth.PasswordSet = spec.Auth.Password != ""
		spec.Network = MapString(outbound, "network")
	}
	if protocol == "vmess" {
		spec.AlterID = MapInt(outbound, "alter_id")
		if security := MapString(outbound, "security"); security != "" {
			spec.Cipher = security
		}
	}

	if tls := MapMap(outbound, "tls"); tls != nil && MapBool(tls, "enabled") {
		spec.TLS.Enabled = true
		spec.TLS.Security = "tls"
		spec.TLS.ServerName = MapString(tls, "server_name")
		spec.TLS.Insecure = MapBool(tls, "insecure")
		spec.TLS.InsecureStated = spec.TLS.Insecure
		spec.TLS.ALPN = MapStringSlice(tls, "alpn")
		if utls := MapMap(tls, "utls"); utls != nil && MapBool(utls, "enabled") {
			spec.TLS.Fingerprint = MapString(utls, "fingerprint")
		}
		if reality := MapMap(tls, "reality"); reality != nil && MapBool(reality, "enabled") {
			spec.TLS.Security = "reality"
			spec.TLS.PublicKey = MapString(reality, "public_key")
			spec.TLS.ShortID = MapString(reality, "short_id")
		}
	}

	if transport := MapMap(outbound, "transport"); transport != nil {
		if transportType := MapString(transport, "type"); transportType != "" && spec.ParameterIsV2RayTransport() {
			spec.Transport.Type = transportType
			spec.Transport.Path = MapString(transport, "path")
			spec.Transport.ServiceName = MapString(transport, "service_name")
			if headers := MapMap(transport, "headers"); headers != nil {
				spec.Transport.Host = DefaultString(MapString(headers, "Host"), MapString(headers, "host"))
			}
		}
	}

	switch protocol {
	case "hysteria2":
		spec.UpMbps = MapInt(outbound, "up_mbps")
		spec.DownMbps = MapInt(outbound, "down_mbps")
		if obfs := MapMap(outbound, "obfs"); obfs != nil {
			spec.Obfs = MapString(obfs, "type")
			spec.ObfsPassword = MapString(obfs, "password")
		}
	case "http":
		spec.Transport.Path = MapString(outbound, "path")
	}
	return spec, nil
}

// pluginOptionsString renders sing-box's plugin_opts back into the wire form.
// sing-box accepts both a string and an object for this field; the previous
// implementation passed an object through fmt.Sprint, producing the Go map
// dump ("map[mode:websocket]") as the plugin configuration.
func pluginOptionsString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case map[string]any:
		if len(typed) == 0 {
			return ""
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+fmt.Sprint(typed[key]))
		}
		return strings.Join(parts, ";")
	default:
		return fmt.Sprint(typed)
	}
}
