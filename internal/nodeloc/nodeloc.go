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

import "strings"

// Observation is the outcome of one landing-IP detection for a node.
type Observation struct {
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

// Resolve returns the location the node should carry after obs, and whether it
// differs from the current one. The current location is returned unchanged for
// any observation that is not a success naming a country.
func Resolve(currentRegion, currentCountry string, obs Observation) (region, country string, changed bool) {
	region, country = currentRegion, currentCountry
	if !obs.Succeeded {
		return region, country, false
	}
	iso := Normalize(obs.ISO)
	if iso == "" {
		return region, country, false
	}
	name := strings.TrimSpace(obs.Country)
	if currentRegion == iso && currentCountry == name {
		return region, country, false
	}
	return iso, name, true
}
