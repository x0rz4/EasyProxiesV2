package noderegion

import "testing"

func TestNormalizeCountryAliases(t *testing.T) {
	for input, want := range map[string]string{" UK ": "gb", "英国": "gb", "🇬🇧": "gb", "香港": "hk", "台灣": "tw", "中国": "cn", "Singapore": "sg", "other": "", "unknown": "", "ZZ": "", "": ""} {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q)=%q want %q", input, got, want)
		}
	}
}

func TestVersionedOtherAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		region        string
		version       int
		unknown, want bool
	}{
		{"", 1, false, true}, {"sg", 1, false, false},
		{"", 2, false, false}, {"", 2, true, true},
		{"sg", 2, false, true}, {"hk", 2, false, false},
	} {
		if got := Matches(tc.region, []string{Other}, tc.unknown, tc.version); got != tc.want {
			t.Errorf("Matches(%+v)=%v", tc, got)
		}
	}
}
