package nodeloc

import (
	"testing"
	"time"
)

func TestLocationEvidenceSequence(t *testing.T) {
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	state := State{Token: Token{Identity: "node", Revision: 1}}
	apply := func(id, iso string, offset int, source Source, success bool) Change {
		t.Helper()
		obs := Observation{Token: state.Token, ProbeID: id, ISO: iso, IP: "203.0.113.1", Country: iso,
			StartedAt: base.Add(time.Duration(offset) * time.Second), CheckedAt: base.Add(time.Duration(offset+1) * time.Second), Source: source, Succeeded: success}
		var change Change
		state, change = Decide(state, obs)
		return change
	}
	if c := apply("first", "HK", 0, SourceGeoIPExit, true); !c.Changed || c.Location.ISO != "hk" {
		t.Fatalf("first success: %+v", c)
	}
	if c := apply("second", "JP", 2, SourceGeoIPExit, true); c.Changed || state.Confirmations != 1 {
		t.Fatalf("first relocation: %+v", c)
	}
	if c := apply("second", "JP", 2, SourceExitProbe, true); c.Changed || state.Confirmations != 1 {
		t.Fatalf("second classifier counted twice: %+v", c)
	}
	if c := apply("second", "JP", 2, SourceExitProbe, true); c.Applied {
		t.Fatalf("duplicate accepted: %+v", c)
	}
	if c := apply("old", "US", 1, SourceExitProbe, true); c.Applied {
		t.Fatalf("old probe accepted: %+v", c)
	}
	if c := apply("failed", "JP", 4, SourceGeoIPExit, false); c.Changed || state.Confirmations != 0 {
		t.Fatalf("failure changed location or retained pending: %+v", c)
	}
	apply("third", "JP", 6, SourceGeoIPExit, true)
	if c := apply("fourth", "JP", 8, SourceGeoIPExit, true); !c.MembershipChanged || c.Location.ISO != "jp" {
		t.Fatalf("confirmation: %+v", c)
	}
	apply("manual", "GB", 10, SourceManual, true)
	if c := apply("automatic", "US", 12, SourceExitProbe, true); c.Location.ISO != "gb" || c.Changed {
		t.Fatalf("manual override lost: %+v", c)
	}
}

func TestLocationEvidenceRejectsWrongRevisionAndInvalidResults(t *testing.T) {
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	state := State{Token: Token{Identity: "node", Revision: 3}, Evidence: Evidence{Current: Location{ISO: "hk", Known: true, Source: SourceExitProbe}}}
	for _, tc := range []struct {
		name, iso, ip string
		revision      int64
		source        Source
	}{
		{"old identity revision", "jp", "203.0.113.1", 1, SourceExitProbe},
		{"invalid IP", "jp", "example.com", 3, SourceExitProbe},
		{"empty ISO", "", "203.0.113.1", 3, SourceExitProbe},
		{"bucket is not country", "other", "203.0.113.1", 3, SourceExitProbe},
		{"entry geography rejected", "jp", "203.0.113.1", 3, Source("geoip_entry")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, c := Decide(state, Observation{Token: Token{Identity: "node", Revision: tc.revision}, ProbeID: "probe", StartedAt: base, CheckedAt: base, Source: tc.source, Succeeded: true, ISO: tc.iso, IP: tc.ip})
			if c.Changed || next.Current.ISO != "hk" || next.Confirmations != 0 {
				t.Fatalf("invalid evidence changed state: %+v %+v", next, c)
			}
		})
	}
}

func TestHigherPriorityClassifierReplacesSameProbeContribution(t *testing.T) {
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	state := State{Token: Token{Identity: "node", Revision: 1}, Evidence: Evidence{Current: Location{ISO: "hk", Source: SourceExitProbe, Known: true}, Candidate: Location{ISO: "jp", Known: true}, Confirmations: 1}}
	obs := Observation{Token: state.Token, ProbeID: "probe", StartedAt: base, CheckedAt: base, Source: SourceGeoIPExit, Succeeded: true, ISO: "jp", IP: "203.0.113.1"}
	state, _ = Decide(state, obs)
	if state.Current.ISO != "jp" {
		t.Fatal("second independent probe did not confirm")
	}
	obs.Source, obs.ISO = SourceExitProbe, "us"
	state, change := Decide(state, obs)
	if state.Current.ISO != "hk" || state.Candidate.ISO != "us" || state.Confirmations != 1 || !change.MembershipChanged {
		t.Fatalf("classifier did not replace contribution: %+v %+v", state, change)
	}
}
