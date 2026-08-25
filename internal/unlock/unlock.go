// Package unlock detects streaming/AI service unlock status and native IP
// purity by sending well-known probe requests through a node's outbound.
//
// A check is performed by building an http.Client whose Transport.DialContext
// is a monitor.DialerFunc for the node, so all traffic is routed via that
// proxy. Each service detector issues the requests that the community
// "unlock-test" scripts use and interprets the response into a ServiceResult.
package unlock

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"easy_proxies/internal/geoip"
)

// DialFunc dials a raw connection to address ("host:port") through a node's
// outbound. The signature matches http.Transport.DialContext so it can be
// plugged directly into an HTTP client whose traffic is routed via the node.
// It is kept here (rather than importing monitor.DialerFunc) so this package
// has no upward dependency on monitor — the monitor layer resolves the
// dialer and node name and passes them to Check.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Status values describing a service check outcome.
const (
	// StatusUnlocked means the service treats the node's IP as a supported
	// region and full content is available.
	StatusUnlocked = "unlocked"
	// StatusOriginalsOnly means the IP is in a Netflix Originals-only region:
	// the service's own catalog is blocked but original productions play.
	StatusOriginalsOnly = "originals_only"
	// StatusLocked means the service actively blocks the node's IP.
	StatusLocked = "locked"
	// StatusFailed means the probe could not complete (network error, timeout,
	// unexpected response). The node may still unlock the service — the test
	// just could not determine it.
	StatusFailed = "failed"
)

// ServiceResult is the outcome of one service unlock check.
type ServiceResult struct {
	// Name is the service identifier: "netflix", "disney_plus", "chatgpt".
	Name string `json:"name"`
	// DisplayName is the human-readable label, e.g. "Netflix".
	DisplayName string `json:"display_name"`
	// Status is one of the Status* constants.
	Status string `json:"status"`
	// Region is the region/geo the service reported for the node's IP, when
	// known (e.g. "JP", "US"). Empty when undetermined.
	Region string `json:"region,omitempty"`
	// Detail is a short human-readable note explaining the verdict.
	Detail string `json:"detail,omitempty"`
}

// IPInfo describes the node's exit IP and its GeoIP classification.
type IPInfo struct {
	// IP is the exit IP address as seen by the probe endpoint.
	IP string `json:"ip"`
	// Country is the full country name from the GeoIP database.
	Country string `json:"country,omitempty"`
	// ISOCode is the ISO 3166-1 alpha-2 country code (e.g. "JP").
	ISOCode string `json:"iso_code,omitempty"`
	// Region is the lowercased ISO code used internally for routing/display.
	Region string `json:"region,omitempty"`
	// Pure reports whether the IP is a "native" residential-style IP rather than
	// a flagged datacenter/VPN range. Inference here is heuristic: the IP is
	// considered pure when its GeoIP country resolves cleanly and the IP
	// inspection endpoint returns a non-empty loc field.
	Pure       bool   `json:"pure"`
	ASN        string `json:"asn,omitempty"`
	Org        string `json:"org,omitempty"`
	IPType     string `json:"ip_type,omitempty"`
	UsageType  string `json:"usage_type,omitempty"`
	FraudScore int    `json:"fraud_score,omitempty"`
	RiskLevel  string `json:"risk_level,omitempty"`
}

// Result is the full unlock report for one node.
type Result struct {
	Tag      string          `json:"tag"`
	Name     string          `json:"name"`
	Services []ServiceResult `json:"services"`
	IP       IPInfo          `json:"ip"`
	Error    string          `json:"error,omitempty"`
	Duration int64           `json:"duration_ms"`
}

// Services is the ordered list of services checked, in display order.
var Services = []string{"netflix", "disney_plus", "chatgpt", "youtube", "tiktok", "amazon", "reddit"}

var serviceDisplay = map[string]string{
	"netflix":     "Netflix",
	"disney_plus": "Disney+",
	"chatgpt":     "ChatGPT",
	"youtube":     "YouTube",
	"tiktok":      "TikTok",
	"amazon":      "PrimeVideo",
	"reddit":      "Reddit",
}

// DisplayName returns the human label for a service id.
func DisplayName(name string) string {
	if d, ok := serviceDisplay[name]; ok {
		return d
	}
	return name
}

// Check runs all configured unlock/purity tests through the node whose
// outbound is reached via dialer. The caller resolves the dialer (e.g. via
// monitor.Manager.DialerFor) and the node's tag/display name, then passes
// them here; this keeps the unlock package free of any dependency on monitor.
// geoLookup may be nil (IP purity/region will then be reported as unknown).
// The per-request timeout bounds each HTTP call.
func Check(ctx context.Context, dialer DialFunc, tag, name string, geoLookup *geoip.Lookup, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if dialer == nil {
		return nil, errors.New("dialer not available for this node")
	}

	if name == "" {
		name = tag
	}
	res := &Result{Tag: tag, Name: name}
	start := time.Now()

	client := newClient(ctx, dialer, timeout)

	// Native IP purity: hit a lightweight IP inspection endpoint through the
	// node, then classify with GeoIP.
	res.IP = probeIP(ctx, client, geoLookup, timeout)

	// Run each service check under the shared deadline.
	res.Services = []ServiceResult{
		checkNetflix(ctx, client, timeout),
		checkDisneyPlus(ctx, client, timeout),
		checkChatGPT(ctx, client, timeout),
		checkYouTube(ctx, client, timeout),
		checkTikTok(ctx, client, timeout),
		checkAmazon(ctx, client, timeout),
		checkReddit(ctx, client, timeout),
	}

	res.Duration = time.Since(start).Milliseconds()
	return res, nil
}

// newClient builds an http.Client that routes all connections through dialer.
func newClient(ctx context.Context, dialer DialFunc, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			// Bound each dial by the request timeout so a hung proxy can't
			// hold the slot forever.
			d, cancel := context.WithTimeout(dialCtx, timeout)
			defer cancel()
			return dialer(d, network, addr)
		},
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		TLSHandshakeTimeout: timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Don't follow redirects: unlock decisions depend on the exact
			// status code / location returned by the service.
			return http.ErrUseLastResponse
		},
	}
}

// probeIP determines the exit IP via a public trace endpoint routed through
// the node, then classifies it with the GeoIP database when available.
func probeIP(ctx context.Context, client *http.Client, geo *geoip.Lookup, timeout time.Duration) IPInfo {
	info := IPInfo{}

	// cloudflare trace returns plain text with loc=<CC>; it is fast, free and
	// widely used by unlock-test scripts for IP purity checks.
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return info
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return info
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ip=") {
			info.IP = strings.TrimPrefix(line, "ip=")
		}
		if strings.HasPrefix(line, "loc=") {
			info.ISOCode = strings.ToUpper(strings.TrimPrefix(line, "loc="))
		}
		if strings.HasPrefix(line, "colo=") {
			// colo is the cloudflare datacenter; keep as a detail signal but
			// not authoritative for purity.
			_ = line
		}
	}

	// Prefer the GeoIP database for country/region when available; it is more
	// accurate than the trace endpoint's coarse loc field.
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

	// Heuristic purity: a clean country resolution with a non-empty trace loc
	// is treated as native.
	info.Pure = info.ISOCode != "" && info.IP != ""
	
	if info.IP != "" {
		// Enhance with IP-API / Risk data
		riskInfo := fetchIPQualityRisk(ctx, client, info.IP, timeout)
		info.ASN = riskInfo.ASN
		info.Org = riskInfo.Org
		info.IPType = riskInfo.IPType
		info.UsageType = riskInfo.UsageType
		info.FraudScore = riskInfo.FraudScore
		info.RiskLevel = riskInfo.RiskLevel
		if riskInfo.Pure {
			info.Pure = true
		}
	}
	
	return info
}

// checkNetflix probes Netflix unlock status. The method follows the
// community check: request a self-produced title and a region-restricted
// title and classify from the status codes.
func checkNetflix(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "netflix", DisplayName: "Netflix"}

	// A Netflix Originals title that plays everywhere, used to confirm the IP
	// is reachable at all and to read the region.
	const originalsTitle = "81280792" // Behind the Curve (Originals)
	// A region-locked title used to distinguish full unlock from Originals-only.
	const restrictedTitle = "70143836" // Breaking Bad (region restricted)

	base := "https://www.netflix.com/title/"
	headers := map[string]string{
		"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
		"Accept-Language": "en-US,en;q=0.9",
	}

	// First, the page-status probe to read the geo header without following
	// the redirect Netflix uses for blocked regions.
	loc, status, err := netflixProbe(ctx, client, base+originalsTitle, headers, timeout)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "探测失败: " + err.Error()
		return res
	}
	res.Region = regionFromNetflixLoc(loc)

	switch {
	case status == 200 || status == 0:
		// Reachable; now distinguish full vs originals-only with the restricted
		// title. A 403/404 on the restricted title means the region's catalog
		// is blocked but originals may still play.
		_, rStatus, rErr := netflixProbe(ctx, client, base+restrictedTitle, headers, timeout)
		if rErr != nil {
			// Restricted probe failed outright; fall back to unlocked since the
			// originals page was reachable.
			res.Status = StatusUnlocked
			res.Detail = "可访问（受限标题探测失败）"
			return res
		}
		if rStatus == 200 {
			res.Status = StatusUnlocked
			res.Detail = "完整解锁"
		} else {
			res.Status = StatusOriginalsOnly
			res.Detail = "仅 Netflix Originals（地区目录受限）"
		}
	case status == 403 || status == 404 || status == 451:
		res.Status = StatusLocked
		res.Detail = fmt.Sprintf("被封锁 (HTTP %d)", status)
	case status == 301 || status == 302:
		// Netflix redirects blocked/unsupported regions to a login/error page.
		if strings.Contains(strings.ToLower(loc), "login") || loc == "" {
			res.Status = StatusLocked
			res.Detail = "被重定向至登录页"
		} else {
			res.Status = StatusLocked
			res.Detail = "被重定向"
		}
	default:
		res.Status = StatusFailed
		res.Detail = fmt.Sprintf("未知响应 (HTTP %d)", status)
	}
	return res
}

// netflixProbe GETs a Netflix title URL and returns (Location header, status).
func netflixProbe(ctx context.Context, client *http.Client, url string, headers map[string]string, timeout time.Duration) (string, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.Header.Get("Location"), resp.StatusCode, nil
}

// regionFromNetflixLoc extracts the country code from Netflix's
// X-Original-... / Location "?geo=<CC>" parameter if present.
func regionFromNetflixLoc(loc string) string {
	if loc == "" {
		return ""
	}
	if idx := strings.Index(loc, "geo="); idx != -1 {
		rest := loc[idx+4:]
		// geo code is two uppercase letters.
		if len(rest) >= 2 {
			return strings.ToUpper(rest[:2])
		}
	}
	return ""
}

// checkDisneyPlus probes Disney+ unlock status via the bamgrid device
// token endpoint with the well-known client bearer. HTTP 200 => unlocked,
// 403 => blocked.
func checkDisneyPlus(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "disney_plus", DisplayName: "Disney+"}

	// The bamgrid device-graphql endpoint returns 200 for supported regions
	// and 403 for unsupported ones. The Authorization token is the base64 of
	// "disney&bratling&control", the public client key used by unlock tests.
	const disneyURL = "https://disney.api.edge.bamgrid.com/devices/v1/platforms/android/prod"
	const bearer = "Bearer ZGlzbmV5JmJyYXRsaW5nJmNvbnRyb2w="

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, disneyURL, nil)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "探测失败: " + err.Error()
		return res
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "探测失败: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	switch resp.StatusCode {
	case 200:
		res.Status = StatusUnlocked
		res.Detail = "完整解锁"
	case 403:
		res.Status = StatusLocked
		res.Detail = "被封锁 (HTTP 403)"
	case 451:
		res.Status = StatusLocked
		res.Detail = "地区不可用 (HTTP 451)"
	default:
		res.Status = StatusFailed
		res.Detail = fmt.Sprintf("未知响应 (HTTP %d)", resp.StatusCode)
	}
	return res
}

// checkChatGPT probes ChatGPT availability. It first reads the cloudflare
// trace to confirm reachability, then hits the OpenAI compliance endpoint
// that gates ChatGPT by region. 200/unsupported_country classification follows
// the response body.
func checkChatGPT(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "chatgpt", DisplayName: "ChatGPT"}

	// The OpenAI compliance/eligibility endpoint returns a JSON flag for
	// unsupported regions. This mirrors the check used by community scripts.
	const complianceURL = "https://chat.openai.com/cdn-cgi/trace"
	const apiURL = "https://api.openai.com/compliance/cookie_requirements"

	// 1) Reachability + region via trace (reuse a direct request).
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, complianceURL, nil)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "探测失败: " + err.Error()
		return res
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		res.Detail = "探测失败: " + err.Error()
		return res
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	if resp.StatusCode != 200 {
		res.Status = StatusFailed
		res.Detail = fmt.Sprintf("trace 响应异常 (HTTP %d)", resp.StatusCode)
		return res
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "loc=") {
			res.Region = strings.ToUpper(strings.TrimPrefix(line, "loc="))
		}
	}

	// 2) Eligibility via the compliance endpoint. Some regions are served but
	// flagged as "unsupported_country" — that counts as locked.
	apiCtx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	req2, err := http.NewRequestWithContext(apiCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		res.Status = StatusUnlocked
		res.Detail = "可访问（合规接口探测失败）"
		return res
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req2.Header.Set("Accept", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		// Trace succeeded but the API call failed: ChatGPT front-end is likely
		// reachable, so treat as unlocked with a caveat.
		res.Status = StatusUnlocked
		res.Detail = "可访问（合规接口探测失败）"
		return res
	}
	defer resp2.Body.Close()
	apiBody, _ := io.ReadAll(io.LimitReader(resp2.Body, 4096))

	switch {
	case resp2.StatusCode == 200:
		var cr struct {
			UnsupportedCountry bool `json:"unsupported_country"`
			HasCountry         bool `json:"has_country"`
		}
		if json.Unmarshal(apiBody, &cr) == nil && cr.UnsupportedCountry {
			res.Status = StatusLocked
			res.Detail = "地区不支持"
			return res
		}
		res.Status = StatusUnlocked
		res.Detail = "完整解锁"
	case resp2.StatusCode == 403:
		res.Status = StatusLocked
		res.Detail = "被封锁 (HTTP 403)"
	default:
		res.Status = StatusFailed
		res.Detail = "未知响应 (HTTP " + strconv.Itoa(resp2.StatusCode) + ")"
	}
	return res
}
