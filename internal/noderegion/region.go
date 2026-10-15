// Package noderegion defines country input, display and versioned bucket
// semantics. Unknown is empty; Other is a query bucket, never a country code.
package noderegion

import (
	"sort"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

const Other = "other"
const CurrentVersion = 2

var names = make(map[string]string)
var aliases = make(map[string]string)

func init() {
	for _, r := range language.Supported.Regions() {
		code := strings.ToLower(r.String())
		if len(code) != 2 || !r.IsCountry() {
			continue
		}
		name := display.English.Regions().Name(r)
		if strings.TrimSpace(name) == "" {
			continue
		}
		names[code] = name
		aliases[code] = code
		aliases[strings.ToLower(name)] = code
		for _, localized := range []string{display.SimplifiedChinese.Regions().Name(r), display.TraditionalChinese.Regions().Name(r)} {
			if localized != "" {
				aliases[localized] = code
			}
		}
	}
	for alias, code := range map[string]string{"uk": "gb", "usa": "us", "中国": "cn", "中國": "cn", "香港": "hk", "台湾": "tw", "台灣": "tw", "澳门": "mo", "澳門": "mo", "韩国": "kr", "韓國": "kr", "英国": "gb", "英國": "gb"} {
		aliases[alias] = code
	}
}

func Normalize(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) == 2 && runes[0] >= 0x1f1e6 && runes[0] <= 0x1f1ff && runes[1] >= 0x1f1e6 && runes[1] <= 0x1f1ff {
		value = string([]rune{'a' + runes[0] - 0x1f1e6, 'a' + runes[1] - 0x1f1e6})
	}
	return aliases[value]
}

func Valid(code string) bool { _, ok := names[code]; return ok }

func Name(code string) string {
	if code == Other {
		return "Other"
	}
	if name := names[Normalize(code)]; name != "" {
		return name
	}
	return "Unknown"
}

func Codes() []string {
	result := make([]string, 0, len(names))
	for code := range names {
		result = append(result, code)
	}
	sort.Strings(result)
	return result
}

// IsOther uses the fixed v2 common-country set; display popularity must never
// change membership. Introducing another set requires another rule version.
func IsOther(code string) bool {
	code = Normalize(code)
	if code == "" {
		return false
	}
	switch code {
	case "jp", "kr", "us", "hk", "tw":
		return false
	}
	return true
}

func Legacy(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return Other
	}
	return value
}

func Matches(value string, regions []string, includeUnknown bool, version int) bool {
	if version < CurrentVersion {
		for _, candidate := range regions {
			if Legacy(candidate) == Legacy(value) {
				return true
			}
		}
		return false
	}
	code := Normalize(value)
	if code == "" {
		return includeUnknown
	}
	for _, candidate := range regions {
		if strings.EqualFold(strings.TrimSpace(candidate), Other) && IsOther(code) {
			return true
		}
		if Normalize(candidate) == code {
			return true
		}
	}
	return false
}
