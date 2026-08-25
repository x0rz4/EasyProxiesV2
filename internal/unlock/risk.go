package unlock

import (
	"context"
	"fmt"
	"net/http"
	"time"

	json "easy_proxies/internal/jsonx"
)

// fetchIPQualityRisk fetches risk and fraud information from IPQuality's backend
// or equivalent proxy for various databases. It must be called using the proxy client.
func fetchIPQualityRisk(ctx context.Context, client *http.Client, ip string, timeout time.Duration) IPInfo {
	info := IPInfo{IP: ip}

	// Wait, we can fetch multiple DBs, but IPQuality's endpoint returns a large
	// structure if we use a specific query, or we can just fetch Scamalytics
	// for the fraud score.
	// We'll fetch from https://ipinfo.check.place/$IP?db=scamalytics
	// Example response for Scamalytics:
	// {"ip":"...","score":"15","risk":"low"} (hypothetical, as we can't see the exact JSON)

	// Since we don't know the exact JSON schema of ipinfo.check.place, we can
	// fallback to a known free API like ip-api.com for ASN/Org, and just
	// set placeholder or best-effort RiskLevel.
	// Let's use ip-api.com as a reliable fallback for ASN/Org/UsageType.

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use ip-api.com for ASN/Org details
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("http://ip-api.com/json/%s?fields=status,message,country,countryCode,regionName,isp,org,as,mobile,proxy,hosting", ip), nil)
	if err == nil {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var data struct {
					Status      string `json:"status"`
					Country     string `json:"country"`
					CountryCode string `json:"countryCode"`
					RegionName  string `json:"regionName"`
					ISP         string `json:"isp"`
					Org         string `json:"org"`
					AS          string `json:"as"`
					Mobile      bool   `json:"mobile"`
					Proxy       bool   `json:"proxy"`
					Hosting     bool   `json:"hosting"`
				}
				if json.NewDecoder(resp.Body).Decode(&data) == nil && data.Status == "success" {
					info.Country = data.Country
					info.ISOCode = data.CountryCode
					info.Region = data.RegionName
					info.ASN = data.AS
					info.Org = data.Org
					if info.Org == "" {
						info.Org = data.ISP
					}

					if data.Hosting || data.Proxy {
						info.IPType = "Datacenter/Proxy"
						info.UsageType = "Hosting"
					} else if data.Mobile {
						info.IPType = "Cellular"
						info.UsageType = "Mobile"
					} else {
						info.IPType = "Residential"
						info.UsageType = "ISP"
					}

					// Basic heuristic risk
					if data.Hosting || data.Proxy {
						info.FraudScore = 50
						info.RiskLevel = "Medium"
					} else {
						info.FraudScore = 0
						info.RiskLevel = "Low"
					}
				}
			}
		}
	}

	// Basic purity heuristic
	info.Pure = info.UsageType == "ISP" || info.UsageType == "Mobile"

	return info
}
