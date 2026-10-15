package nodeloc

import "testing"

func TestResolve(t *testing.T) {
	tests := []struct {
		name                string
		curRegion, curCtry  string
		obs                 Observation
		wantRegion, wantCtr string
		wantChanged         bool
	}{
		{"failed observation keeps known location", "hk", "Hong Kong", Observation{}, "hk", "Hong Kong", false},
		{"failed observation with stale ISO still keeps location", "hk", "Hong Kong", Observation{Succeeded: false, ISO: "US", Country: "United States"}, "hk", "Hong Kong", false},
		{"success without a country keeps location", "jp", "Japan", Observation{Succeeded: true, ISO: "  "}, "jp", "Japan", false},
		{"success with a new country replaces it", "hk", "Hong Kong", Observation{Succeeded: true, ISO: " JP ", Country: " Japan "}, "jp", "Japan", true},
		{"success on an unclassified node sets it", "", "", Observation{Succeeded: true, ISO: "SG", Country: "Singapore"}, "sg", "Singapore", true},
		{"identical success is not a change", "us", "United States", Observation{Succeeded: true, ISO: "US", Country: "United States"}, "us", "United States", false},
		{"country name only change is a change", "us", "USA", Observation{Succeeded: true, ISO: "us", Country: "United States"}, "us", "United States", true},
		{"failure on an unclassified node stays unclassified", "", "", Observation{}, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			region, country, changed := Resolve(tt.curRegion, tt.curCtry, tt.obs)
			if region != tt.wantRegion || country != tt.wantCtr || changed != tt.wantChanged {
				t.Fatalf("Resolve() = (%q, %q, %v), want (%q, %q, %v)", region, country, changed, tt.wantRegion, tt.wantCtr, tt.wantChanged)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("  HK "); got != "hk" {
		t.Fatalf("Normalize = %q", got)
	}
}
