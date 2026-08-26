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
	risk := fetchIPQualityRisk(runtime.Context, runtime.Client, info.IP, runtime.Timeout)
	info.ASN, info.Org = risk.ASN, risk.Org
	info.IPType, info.UsageType = risk.IPType, risk.UsageType
	info.FraudScore, info.RiskLevel = risk.FraudScore, risk.RiskLevel
	if risk.Country != "" && info.Country == "" {
		info.Country = risk.Country
	}
	// A valid IP and country only prove that the exit is reachable. Mark it as
	// pure exclusively when the risk source identifies an ISP/mobile network;
	// otherwise datacenter and VPN exits would all become false positives.
	info.Pure = risk.Pure
	return info
}
