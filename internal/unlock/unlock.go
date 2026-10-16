// Package unlock detects streaming and AI service availability through a
// node-specific outbound. Detectors are independent modules registered in a
// concurrency-safe registry and executed through a shared proxy HTTP runtime.
package unlock

import (
	"context"
	"errors"
	"net"
	"time"

	"easy_proxies/internal/geoip"
	"easy_proxies/internal/noderesult"
)

// DialFunc opens a raw connection through one node's outbound.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Status values are kept compatible with persisted results and the Web UI.
const (
	StatusUnlocked      = "unlocked"
	StatusPartial       = "partial"
	StatusOriginalsOnly = "originals_only"
	StatusLocked        = "locked"
	StatusFailed        = "failed"
)

// ServiceResult is the stable API representation of one detector result. It is
// an alias for the shared persisted shape, so a detector result is stored and
// served verbatim.
type ServiceResult = noderesult.Service

// IPInfo describes the observed exit IP and its best-effort classification.
type IPInfo = noderesult.IPInfo

// Result is the full unlock report for one node.
type Result = noderesult.Unlock

// Check runs every registered default detector. The public response remains
// compatible with the monitor API while detector implementation is modular.
func Check(ctx context.Context, dialer DialFunc, tag, name string, geoLookup *geoip.Lookup, timeout time.Duration) (*Result, error) {
	if dialer == nil {
		return nil, errors.New("dialer not available for this node")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if name == "" {
		name = tag
	}
	return checkRegistered(ctx, dialer, tag, name, geoLookup, timeout), nil
}
