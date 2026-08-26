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

// ServiceResult is the stable API representation of one detector result.
type ServiceResult struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Region      string `json:"region,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// IPInfo describes the observed exit IP and its best-effort classification.
type IPInfo struct {
	IP         string `json:"ip"`
	Country    string `json:"country,omitempty"`
	ISOCode    string `json:"iso_code,omitempty"`
	Region     string `json:"region,omitempty"`
	Pure       bool   `json:"pure"`
	ASN        string `json:"asn,omitempty"`
	Org        string `json:"org,omitempty"`
	IPType     string `json:"ip_type,omitempty"`
	UsageType  string `json:"usage_type,omitempty"`
	FraudScore int    `json:"fraud_score,omitempty"`
	RiskLevel  string `json:"risk_level,omitempty"`
}

// Result is the full unlock report for one node.
type Result struct {
	Tag      string          `json:"tag"`
	Name     string          `json:"name"`
	Services []ServiceResult `json:"services"`
	IP       IPInfo          `json:"ip"`
	Error    string          `json:"error,omitempty"`
	Duration int64           `json:"duration_ms"`
}

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
