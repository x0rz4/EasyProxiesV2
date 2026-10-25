package config

import (
	"errors"
	"io"
)

const MaxSubscriptionBodyBytes = 10 * 1024 * 1024

// ReadSubscriptionBody applies the same limit to startup and managed fetches,
// including chunked or decompressed responses without a useful Content-Length.
func ReadSubscriptionBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, MaxSubscriptionBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxSubscriptionBodyBytes {
		return nil, errors.New("subscription response exceeds 10MB")
	}
	return body, nil
}
