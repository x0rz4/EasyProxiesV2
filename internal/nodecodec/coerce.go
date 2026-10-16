// Shared coercion primitives for every node-link entry and exit point.
//
// Before this file existed each of the four link paths — config import,
// nodecodec identity, builder outbound and subrender clash — carried its own
// copy of base64 decoding, scalar coercion, truthiness and option lookup. They
// had drifted, so the same node could parse for identity and fail to build an
// outbound, or carry a setting in the identity while the data plane dropped it.
// Everything that interprets a link now goes through these functions.
//
// Two rules keep that safe:
//
//   - Decoding and coercion must be *permissive*: accept every spelling the
//     ecosystem emits, because rejecting one turns a working node into a
//     permanently unavailable one.
//   - Lookup must not feed the identity. CanonicalQueryKey is frozen: adding an
//     alias there re-hashes already-stored nodes. Use LookupOption instead,
//     which resolves historical spellings on the read side and is hash-neutral
//     by construction.
package nodecodec

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// DecodeBase64Loose decodes value under every base64 alphabet the ecosystem
// emits, ignoring embedded whitespace and line wrapping.
//
// Subscriptions routinely wrap payloads at 76 columns, mix padded and unpadded
// forms and use URL-safe alphabets. A strict decoder rejects those, which is
// exactly how a vmess link ends up accepted by the identity codec and rejected
// by the outbound builder.
func DecodeBase64Loose(value string) ([]byte, bool) {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// Truthy reports whether value means "on". It accepts the union of spellings
// the ecosystem writes for an already-boolean field: configurations disagree on
// 1 / true / yes / on, and a node whose certificate check silently stays on
// because the link said "yes" is a support ticket.
func Truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CoerceString renders a scalar from decoded JSON as a string. Link payloads
// are hand-written and routinely use a number where a string is expected (port,
// alterId), so coercion must be total rather than type-asserted.
func CoerceString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

// DefaultString returns value unless it is blank, otherwise fallback.
func DefaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// LookupOption reads one query parameter, accepting every historical spelling
// the identity key mapping deliberately does not fold.
//
// CanonicalQueryKey lower-cases keys and folds a known alias set, so the
// identity calls the gRPC service name "servicename" while the data plane has
// always read "serviceName". That mismatch let a node keep a setting in its
// identity and drop it at dial time. LookupOption resolves all spellings on the
// read side, which is hash-neutral: it changes no identity.
//
// key wins over aliases regardless of query order; within one spelling the
// first non-blank value wins and keys are visited in sorted order so the result
// never depends on map iteration.
func LookupOption(values url.Values, key string, aliases ...string) string {
	for _, wanted := range append([]string{key}, aliases...) {
		for _, raw := range slices.Sorted(maps.Keys(values)) {
			if !strings.EqualFold(raw, wanted) {
				continue
			}
			for _, value := range values[raw] {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

// TLSInsecureOverride returns the certificate-verification verdict stated by
// the link, and whether the link stated one at all.
//
// The two answers differ: an explicit value overrides the global
// SkipCertVerify setting, while an absent one must leave it untouched. Collapsing
// them would silently re-enable verification for nodes whose link says
// "insecure=0" while the global setting says otherwise.
//
// builder previously resolved this in *opposite* orders depending on the
// protocol branch — allowInsecure first for generic TLS, insecure first for
// AnyTLS and Hysteria2 — so one node could verify its certificate on one code
// path and not on another. "insecure" is the canonical spelling and wins.
func TLSInsecureOverride(values url.Values) (verdict bool, stated bool) {
	for _, key := range []string{"insecure", "allowInsecure", "skip-cert-verify", "skip_cert_verify"} {
		if value := LookupOption(values, key); value != "" {
			return Truthy(value), true
		}
	}
	return false, false
}

// TrimLink removes the transport junk that survives copy-paste — a UTF-8 BOM
// written by Excel and some Windows editors, plus surrounding whitespace —
// before a link is parsed.
//
// ParseURI does this internally, which is why a BOM-prefixed link imports and
// identifies cleanly. Every other entry point has to do the same, or that link
// becomes unbuildable at dial time.
func TrimLink(raw string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff"))
}

// URIScheme returns the canonical scheme of a raw link without fully parsing it.
//
// Full parsing rejects payloads that legitimately contain whitespace — a vmess
// base64 blob wrapped across lines is the common case, and callers must dispatch
// on the prefix before handing the link to url.Parse. Returns "" when the link
// carries no scheme.
func URIScheme(raw string) string {
	raw = TrimLink(raw)
	index := strings.Index(raw, "://")
	if index <= 0 {
		return ""
	}
	return NormalizeProtocol(raw[:index])
}

// TLSServerName resolves the TLS server name from a link, accepting every
// spelling the ecosystem uses for it. The identity key mapping already folds
// peer / servername / server_name into sni; the data plane used to read only
// "sni", so a trojan link written with "peer" had its certificate checked
// against the dialled address instead of the configured name.
func TLSServerName(values url.Values) string {
	return LookupOption(values, "sni", "peer", "servername", "server_name")
}

// FirstOption returns the first value recorded for key in a canonical option
// map, or "" when the key is absent.
func FirstOption(values map[string][]string, key string) string {
	if len(values[key]) == 0 {
		return ""
	}
	return values[key][0]
}
