package nodeloc

import (
	"net/netip"
	"strings"
	"time"
)

type Source string

const (
	SourceLegacy    Source = "legacy_unverified"
	SourceGeoIPExit Source = "geoip_exit"
	SourceExitProbe Source = "exit_probe"
	SourceManual    Source = "manual"
)

// Location is the last accepted location, independent of the latest probe's
// success or failure. Entry-server geography is never a landing location.
type Location struct {
	ISO       string    `json:"iso"`
	Country   string    `json:"country"`
	Source    Source    `json:"source"`
	Known     bool      `json:"known"`
	CheckedAt time.Time `json:"checked_at"`
}

// Token is captured before starting network work. Revision also rejects an
// in-flight result when a node changes from identity A to B and back to A.
type Token struct {
	Identity string `json:"identity"`
	Revision int64  `json:"revision"`
}

type Evidence struct {
	Current       Location `json:"current"`
	Candidate     Location `json:"candidate"`
	Confirmations int      `json:"confirmations"`
}

// State persists the debounce window. BeforeProbe permits a higher-priority
// classifier of the same exit probe to replace, rather than count twice, its
// contribution to the confirmation sequence.
type State struct {
	Token
	Evidence
	BeforeProbe   Evidence  `json:"before_probe"`
	LastProbeID   string    `json:"last_probe_id"`
	LastStartedAt time.Time `json:"last_started_at"`
	LastSource    Source    `json:"last_source"`
	LastSucceeded bool      `json:"last_succeeded"`
}

type Change struct {
	Location          Location `json:"location"`
	Changed           bool     `json:"changed"`
	MembershipChanged bool     `json:"membership_changed"`
	Applied           bool     `json:"applied"`
	Reason            string   `json:"reason"`
}

func sourcePriority(source Source) int {
	switch source {
	case SourceManual:
		return 3
	case SourceExitProbe:
		return 2
	case SourceGeoIPExit:
		return 1
	default:
		return 0
	}
}

// ValidISO validates the lowercase ISO storage shape, excluding bucket names.
func ValidISO(iso string) bool {
	return len(iso) == 2 && iso[0] >= 'a' && iso[0] <= 'z' && iso[1] >= 'a' && iso[1] <= 'z'
}

// Decide is pure; the repository must read state and commit its result in one
// transaction. A failed, duplicate, older or wrong-identity probe cannot erase
// the last accepted location.
func Decide(state State, obs Observation) (State, Change) {
	before := state.Current
	change := Change{Location: before}
	reject := func(reason string) (State, Change) { change.Reason = reason; return state, change }
	if obs.Identity == "" || obs.Identity != state.Identity || obs.Revision != state.Revision {
		return reject("identity_changed")
	}
	if obs.ProbeID == "" || obs.StartedAt.IsZero() || obs.CheckedAt.Before(obs.StartedAt) {
		return reject("invalid_observation")
	}
	if sourcePriority(obs.Source) == 0 {
		return reject("unsupported_source")
	}
	if obs.StartedAt.Before(state.LastStartedAt) {
		return reject("older_observation")
	}
	iso := Normalize(obs.ISO)
	_, ipErr := netip.ParseAddr(strings.TrimSpace(obs.IP))
	success := obs.Succeeded && ValidISO(iso) && (obs.Source == SourceManual || ipErr == nil)
	if obs.ProbeID == state.LastProbeID {
		if !obs.StartedAt.Equal(state.LastStartedAt) {
			return reject("invalid_replay")
		}
		if sourcePriority(obs.Source) < sourcePriority(state.LastSource) ||
			(sourcePriority(obs.Source) == sourcePriority(state.LastSource) && (state.LastSucceeded || !success)) {
			return reject("duplicate_observation")
		}
		state.Evidence = state.BeforeProbe
	} else {
		if obs.StartedAt.Equal(state.LastStartedAt) {
			return reject("ambiguous_order")
		}
		state.BeforeProbe = state.Evidence
	}
	state.LastProbeID, state.LastStartedAt = obs.ProbeID, obs.StartedAt
	state.LastSource, state.LastSucceeded = obs.Source, success
	change.Applied = true
	change.Reason = "failed_or_unclassified"
	if !success {
		state.Candidate, state.Confirmations = Location{}, 0
	} else if state.Current.Source == SourceManual && obs.Source != SourceManual {
		state.Candidate, state.Confirmations = Location{}, 0
		change.Reason = "manual_override"
	} else {
		next := Location{ISO: iso, Country: strings.TrimSpace(obs.Country), Source: obs.Source, Known: true, CheckedAt: obs.CheckedAt}
		if next.ISO == state.Current.ISO && next.Country == "" {
			next.Country = state.Current.Country
		}
		if !state.Current.Known || state.Current.Source == SourceLegacy || next.ISO == state.Current.ISO || obs.Source == SourceManual {
			state.Current, state.Candidate, state.Confirmations = next, Location{}, 0
			change.Reason = "accepted"
		} else {
			if next.ISO != state.Candidate.ISO {
				state.Confirmations = 0
			}
			state.Candidate = next
			state.Confirmations++
			change.Reason = "awaiting_confirmation"
			if state.Confirmations >= 2 {
				state.Current, state.Candidate, state.Confirmations = next, Location{}, 0
				change.Reason = "confirmed"
			}
		}
	}
	change.Location = state.Current
	change.Changed = before.ISO != state.Current.ISO || before.Country != state.Current.Country || before.Known != state.Current.Known
	change.MembershipChanged = before.ISO != state.Current.ISO || before.Known != state.Current.Known
	return state, change
}
