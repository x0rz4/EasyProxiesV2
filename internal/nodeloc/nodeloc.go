// Package nodeloc decides what landing location a node should carry after a
// detection pass.
//
// The rule it exists to enforce: a failed or empty observation never erases a
// location that is already known. Group membership is driven by the node's
// region, so wiping it on one failed probe moved the node out of its regional
// group (and into the "other" bucket, since an empty region normalizes to
// "other") until some later probe succeeded. Only a successful observation that
// names a country may change the location.
//
// The package has no dependencies so every write path can share the one rule.
package nodeloc

import (
	"strings"
	"time"
)

// Observation is the outcome of one landing-IP detection for a node.
type Observation struct {
	Token
	ProbeID   string    `json:"probe_id"`
	StartedAt time.Time `json:"started_at"`
	CheckedAt time.Time `json:"checked_at"`
	IP        string    `json:"ip"`
	Source    Source    `json:"source"`
	// Succeeded is true only when the exit IP was obtained through the node's
	// own tunnel. A lookup that did not go through the node says nothing about
	// where the node lands.
	Succeeded bool
	// ISO is the exit country's ISO 3166-1 alpha-2 code, any case.
	ISO     string
	Country string
}

// Normalize returns the stored form of a region code: trimmed and lowercase.
func Normalize(region string) string {
	return strings.ToLower(strings.TrimSpace(region))
}
