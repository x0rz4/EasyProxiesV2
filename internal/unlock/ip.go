package unlock

import (
	"strings"

	"easy_proxies/internal/geoip"
)

func probeExitIP(runtime Runtime, geo *geoip.Lookup) IPInfo {
	info := IPInfo{}
	resp, err := fetchProbeWithLimit(runtime, "https://www.cloudflare.com/cdn-cgi/trace", nil, 4096)
	if err != nil || resp.StatusCode != 200 {
		return info
	}
	for _, line := range strings.Split(resp.RawBody, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			info.IP = strings.TrimSpace(value)
		case "loc":
			info.ISOCode = strings.ToUpper(strings.TrimSpace(value))
		}
	}
	if info.IP != "" && geo != nil && geo.IsEnabled() {
		region := geo.LookupIP(info.IP)
		info.Country = region.Country
		if info.ISOCode == "" {
			info.ISOCode = strings.ToUpper(region.ISOCode)
		}
		info.Region = region.Code
	} else if info.ISOCode != "" {
		info.Region = strings.ToLower(info.ISOCode)
	}
	if info.IP == "" {
		return info
	}
	info = mergeIPMetadata(info, fetchIPMetadata(runtime, info.IP))
	// IP quality is intentionally handled by the independent node diagnostics
	// providers. Reachability and geography alone cannot prove purity or assign
	// a fraud score, so legacy fields remain unknown here.
	return info
}
