package unlock

import (
	"context"
	"fmt"
	"net/http"
	"time"

	json "easy_proxies/internal/jsonx"
)

// fetchIPQualityRisk performs a best-effort network-type lookup through the
// node's proxy client. The returned fraud score is a local coarse heuristic,
// not a score supplied by ip-api.com.
func fetchIPQualityRisk(ctx context.Context, client *http.Client, ip string, timeout time.Duration) IPInfo {
	info := IPInfo{IP: ip}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

					// Keep the score deliberately coarse: it only separates a
					// provider-declared hosting/proxy exit from ISP/mobile access.
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

	// Purity is only asserted from an explicit non-hosting classification.
	info.Pure = info.UsageType == "ISP" || info.UsageType == "Mobile"

	return info
}
