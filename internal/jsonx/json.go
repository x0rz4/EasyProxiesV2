// Package jsonx centralizes JSON encoding and decoding for EasyProxies.
//
// The default API favors Sonic's performance-oriented configuration. Callers
// that require deterministic map ordering (for example hashes or signatures)
// must use MarshalCanonical instead.
package jsonx

import (
	"io"

	"github.com/bytedance/sonic"
)

func Marshal(v any) ([]byte, error) {
	return sonic.ConfigDefault.Marshal(v)
}

func Unmarshal(data []byte, v any) error {
	return sonic.ConfigDefault.Unmarshal(data, v)
}

func NewEncoder(w io.Writer) sonic.Encoder {
	return sonic.ConfigDefault.NewEncoder(w)
}

func NewDecoder(r io.Reader) sonic.Decoder {
	return sonic.ConfigDefault.NewDecoder(r)
}

// MarshalCanonical uses Sonic's encoding/json-compatible configuration. Its
// sorted map keys make the output suitable for stable hashes and identities.
func MarshalCanonical(v any) ([]byte, error) {
	return sonic.ConfigStd.Marshal(v)
}
