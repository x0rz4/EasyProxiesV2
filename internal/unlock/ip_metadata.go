package unlock

import (
	"fmt"
	"net/url"
	"strings"

	json "easy_proxies/internal/jsonx"
)

// fetchIPMetadata restores the lightweight metadata lookup historically used
// by quick unlock checks. It deliberately returns geography and network owner
// data only; purity and risk classifications belong to independent providers.
func fetchIPMetadata(runtime Runtime, ip string) IPInfo {
	info := IPInfo{IP: strings.TrimSpace(ip)}
	if info.IP == "" {
		return info
	}

	target := fmt.Sprintf("http://ip-api.com/json/%s?fields=status,message,country,countryCode,regionName,isp,org,as", url.PathEscape(info.IP))
	response, err := fetchProbeWithLimit(runtime, target, nil, 16*1024)
	if err != nil || response.StatusCode != 200 {
		return info
	}

	var payload struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		RegionName  string `json:"regionName"`
		ISP         string `json:"isp"`
		Org         string `json:"org"`
		AS          string `json:"as"`
	}
	if err := json.Unmarshal([]byte(response.RawBody), &payload); err != nil || payload.Status != "success" {
		return info
	}

	info.Country = strings.TrimSpace(payload.Country)
	info.ISOCode = strings.ToUpper(strings.TrimSpace(payload.CountryCode))
	info.Region = strings.TrimSpace(payload.RegionName)
	info.ASN = strings.TrimSpace(payload.AS)
	info.Org = strings.TrimSpace(payload.Org)
	if info.Org == "" {
		info.Org = strings.TrimSpace(payload.ISP)
	}
	return info
}

func mergeIPMetadata(info, metadata IPInfo) IPInfo {
	if info.Country == "" {
		info.Country = metadata.Country
	}
	if info.ISOCode == "" {
		info.ISOCode = metadata.ISOCode
	}
	if info.Region == "" {
		info.Region = metadata.Region
	}
	if info.ASN == "" {
		info.ASN = metadata.ASN
	}
	if info.Org == "" {
		info.Org = metadata.Org
	}
	return info
}
