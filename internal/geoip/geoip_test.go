package geoip

import (
	"testing"

	"github.com/oschwald/geoip2-golang/v2"
)

func TestParseLookupAddress(t *testing.T) {
	for _, input := range []string{"203.0.113.10", "2001:db8::10"} {
		if _, ok := parseLookupAddress(input); !ok {
			t.Fatalf("parseLookupAddress(%q) rejected a valid address", input)
		}
	}
	if _, ok := parseLookupAddress("not-an-ip"); ok {
		t.Fatal("parseLookupAddress accepted an invalid address")
	}
}

func TestRegionInfoFromCountry(t *testing.T) {
	tests := []struct {
		name   string
		record *geoip2.Country
		want   RegionInfo
	}{
		{
			name: "localized country name",
			record: &geoip2.Country{Country: geoip2.CountryRecord{
				ISOCode: "JP",
				Names:   geoip2.Names{English: "Japan"},
			}},
			want: RegionInfo{Code: RegionJP, Country: "Japan", ISOCode: "JP"},
		},
		{
			name: "ISO code fallback",
			record: &geoip2.Country{Country: geoip2.CountryRecord{
				ISOCode: "SG",
			}},
			want: RegionInfo{Code: "sg", Country: "SG", ISOCode: "SG"},
		},
		{name: "nil record", want: unknownRegionInfo()},
		{name: "empty record", record: &geoip2.Country{}, want: unknownRegionInfo()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := regionInfoFromCountry(tt.record); got != tt.want {
				t.Fatalf("regionInfoFromCountry() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExtractHostFromURISupportsHTTPAndSOCKS5(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{name: "http", uri: "http://alice:secret@example.com:8080", want: "example.com"},
		{name: "socks5", uri: "socks5://alice:secret@99.144.123.135:30350", want: "99.144.123.135"},
	}

	for _, tt := range tests {
		if got := extractHostFromURI(tt.uri); got != tt.want {
			t.Fatalf("%s: extractHostFromURI(%q) = %q, want %q", tt.name, tt.uri, got, tt.want)
		}
	}
}

func TestIsoCodeToRegionIsCountryCode(t *testing.T) {
	// Well-known regions keep their codes.
	if got := isoCodeToRegion("JP"); got != "jp" {
		t.Errorf("isoCodeToRegion(JP) = %q, want jp", got)
	}
	if got := isoCodeToRegion("SG"); got != "sg" {
		t.Errorf("isoCodeToRegion(SG) = %q, want sg", got)
	}
	if got := isoCodeToRegion("DE"); got != "de" {
		t.Errorf("isoCodeToRegion(DE) = %q, want de", got)
	}
	// Unknown/empty falls back to "other".
	if got := isoCodeToRegion(""); got != RegionOther {
		t.Errorf("isoCodeToRegion(\"\") = %q, want %q", got, RegionOther)
	}
}

func TestRegionNameShowsCountryCode(t *testing.T) {
	// Well-known regions show friendly names.
	if got := RegionName(RegionJP); got != "Japan" {
		t.Errorf("RegionName(jp) = %q, want Japan", got)
	}
	// Any 2-letter code shows the uppercased code (not "Unknown").
	if got := RegionName("sg"); got != "SG" {
		t.Errorf("RegionName(sg) = %q, want SG", got)
	}
	if got := RegionName("de"); got != "DE" {
		t.Errorf("RegionName(de) = %q, want DE", got)
	}
}

func TestRegionEmojiFromAnyCountryCode(t *testing.T) {
	// "other" keeps the globe.
	if got := RegionEmoji(RegionOther); got != "🌍" {
		t.Errorf("RegionEmoji(other) = %q, want 🌍", got)
	}
	// Any 2-letter ISO code yields the correct flag via regional indicators.
	cases := map[string]string{
		"jp": "🇯🇵", "us": "🇺🇸", "sg": "🇸🇬", "de": "🇩🇪", "gb": "🇬🇧",
	}
	for code, want := range cases {
		if got := RegionEmoji(code); got != want {
			t.Errorf("RegionEmoji(%q) = %q, want %q", code, got, want)
		}
	}
	// Garbage / wrong-length inputs return the question flag.
	if got := RegionEmoji(""); got != "❓" {
		t.Errorf("RegionEmoji(\"\") = %q, want ❓", got)
	}
	if got := RegionEmoji("usa"); got != "❓" {
		t.Errorf("RegionEmoji(usa) = %q, want ❓", got)
	}
}
