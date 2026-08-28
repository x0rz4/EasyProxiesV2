// Package runtimetag defines the one tag namespace shared by statically built
// and dynamically created node outbounds.
package runtimetag

import (
	"errors"
	"fmt"
	"strings"
)

const InitialVersion uint64 = 1

// Format returns the tag for one concrete runtime generation. NodeKey may
// carry a persisted identity prefix (for example "v1:"); only the hexadecimal
// digest participates in the tag.
func Format(nodeKey string, version uint64) (string, error) {
	if version == 0 {
		return "", errors.New("runtime tag version must be greater than zero")
	}
	digest := nodeKey
	if separator := strings.LastIndexByte(digest, ':'); separator >= 0 {
		digest = digest[separator+1:]
	}
	if len(digest) < 16 {
		return "", errors.New("node key does not contain 16 hexadecimal characters")
	}
	digest = strings.ToLower(digest[:16])
	for _, character := range digest {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return "", fmt.Errorf("node key prefix %q is not hexadecimal", digest)
		}
	}
	return fmt.Sprintf("%s@v%d", digest, version), nil
}
