// Package noderesult holds the detection result payloads that are produced by
// the detectors, persisted by the store and served by the monitor API. It is a
// leaf: it imports nothing from the project, so the persistence layer, the
// provider adapters and the API can all share one definition instead of
// mirroring each other's structs and hand-copying fields between them.
//
// The JSON tags here are the wire contract. They match what the store persists
// and what the WebUI consumes, and the store embeds these types in place, so
// embedding must not shift field order — that would change the canonical bytes
// of a stored probe event and invalidate pending receipts across an upgrade.
package noderesult

import "time"

// Quality is one IP-quality provider's verdict for a node's exit IP. Providers
// stay independent: this carries each provider's own answer and never a
// synthesized combined score.
type Quality struct {
	Provider      string    `json:"provider"`
	Status        string    `json:"status"`
	IP            string    `json:"ip,omitempty"`
	Family        string    `json:"family,omitempty"`
	Country       string    `json:"country,omitempty"`
	CountryCode   string    `json:"country_code,omitempty"`
	ASN           string    `json:"asn,omitempty"`
	Org           string    `json:"org,omitempty"`
	ISP           string    `json:"isp,omitempty"`
	IsBroadcast   *bool     `json:"is_broadcast"`
	IsResidential *bool     `json:"is_residential"`
	FraudScore    *int      `json:"fraud_score"`
	Proxy         *bool     `json:"proxy"`
	Hosting       *bool     `json:"hosting"`
	Mobile        *bool     `json:"mobile"`
	Reason        string    `json:"reason,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

// Service is the outcome of one streaming/AI service check.
type Service struct {
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

// Unlock is the full unlock report for one node.
type Unlock struct {
	Tag      string    `json:"tag"`
	Name     string    `json:"name"`
	Services []Service `json:"services"`
	IP       IPInfo    `json:"ip"`
	Error    string    `json:"error,omitempty"`
	Duration int64     `json:"duration_ms"`
}
