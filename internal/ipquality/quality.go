// Package ipquality contains independent IP-quality provider adapters. Results
// remain provider-specific; this package never synthesizes a combined score.
package ipquality

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/nodedetect"
)

const (
	StatusUntested = "untested"
	StatusSuccess  = "success"
	StatusPartial  = "partial"
	StatusFailed   = "failed"
	StatusDisabled = "disabled"
)

type Result struct {
	Provider      string    `json:"provider"`
	Status        string    `json:"status"`
	IP            string    `json:"ip,omitempty"`
	Family        string    `json:"family,omitempty"`
	Country       string    `json:"country,omitempty"`
	CountryCode   string    `json:"country_code,omitempty"`
	ASN           string    `json:"asn,omitempty"`
	Org           string    `json:"org,omitempty"`
	ISP           string    `json:"isp,omitempty"`
	IsBroadcast   *bool     `json:"is_broadcast"`
	IsResidential *bool     `json:"is_residential"`
	FraudScore    *int      `json:"fraud_score"`
	Proxy         *bool     `json:"proxy"`
	Hosting       *bool     `json:"hosting"`
	Mobile        *bool     `json:"mobile"`
	Reason        string    `json:"reason,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

type Provider interface {
	Name() string
	Check(context.Context, nodedetect.DialFunc) Result
}

type IPPureProvider struct {
	URL     string
	Timeout time.Duration
}

func (p IPPureProvider) Name() string { return "ippure" }

func (p IPPureProvider) Check(ctx context.Context, dial nodedetect.DialFunc) Result {
	result := Result{Provider: p.Name(), Status: StatusFailed, CheckedAt: time.Now().UTC()}
	if strings.TrimSpace(p.URL) == "" {
		result.Status, result.Reason = StatusDisabled, "provider URL is not configured"
		return result
	}
	if p.Timeout <= 0 {
		p.Timeout = 5 * time.Second
	}
	transport := &http.Transport{DialContext: dial, ForceAttemptHTTP2: false, DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: p.Timeout}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	req.Header.Set("User-Agent", "EasyProxies/NodeCheck")
	resp, err := client.Do(req)
	if err != nil {
		result.Reason = safeProviderError(err, p.URL)
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return result
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	var response struct {
		IP            string `json:"ip"`
		IsBroadcast   *bool  `json:"isBroadcast"`
		IsResidential *bool  `json:"isResidential"`
		FraudScore    *int   `json:"fraudScore"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		result.Reason = "invalid_json"
		return result
	}
	result.IP = strings.TrimSpace(response.IP)
	result.Family = ipFamily(result.IP)
	result.IsBroadcast, result.IsResidential = response.IsBroadcast, response.IsResidential
	if response.FraudScore != nil && (*response.FraudScore < 0 || *response.FraudScore > 100) {
		result.Status, result.Reason = StatusPartial, "fraud_score_out_of_range"
		return result
	}
	result.FraudScore = response.FraudScore
	if result.IP == "" || result.Family == "" || response.IsBroadcast == nil || response.IsResidential == nil || response.FraudScore == nil {
		result.Status, result.Reason = StatusPartial, "missing_quality_fields"
		if result.IP != "" && result.Family == "" {
			result.Reason = "invalid_ip"
		}
		return result
	}
	result.Status = StatusSuccess
	return result
}

// IPAPIClient queries the free ip-api batch endpoint. It honours provider
// backoff headers and deliberately exposes no fabricated fraud score.
type IPAPIClient struct {
	BaseURL string
	Client  *http.Client
	// MinInterval defaults to four seconds (15 requests/minute). Tests may set
	// a smaller positive value; a negative value disables spacing.
	MinInterval time.Duration
	mu          sync.Mutex
	next        time.Time
	last        time.Time
}

func (c *IPAPIClient) CheckBatch(ctx context.Context, ips []string) map[string]Result {
	results := make(map[string]Result, len(ips))
	for start := 0; start < len(ips); start += 100 {
		end := start + 100
		if end > len(ips) {
			end = len(ips)
		}
		batch := c.checkChunk(ctx, ips[start:end])
		for ip, result := range batch {
			results[ip] = result
		}
	}
	return results
}

func (c *IPAPIClient) checkChunk(ctx context.Context, ips []string) map[string]Result {
	now := time.Now().UTC()
	failed := func(reason string) map[string]Result {
		out := make(map[string]Result, len(ips))
		for _, ip := range ips {
			out[ip] = Result{Provider: "ip-api", Status: StatusFailed, IP: ip, Family: ipFamily(ip), Reason: reason, CheckedAt: now}
		}
		return out
	}
	if err := c.wait(ctx); err != nil {
		return failed(err.Error())
	}
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		base = "http://ip-api.com/batch"
	}
	u, err := url.Parse(base)
	if err != nil {
		return failed("invalid provider URL")
	}
	query := u.Query()
	query.Set("fields", "status,message,query,country,countryCode,isp,org,as,mobile,proxy,hosting")
	u.RawQuery = query.Encode()
	body, err := json.Marshal(ips)
	if err != nil {
		return failed(err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return failed(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EasyProxies/NodeCheck")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return failed(safeProviderError(err, base))
	}
	defer resp.Body.Close()
	c.applyBackoff(resp)
	if resp.StatusCode == http.StatusTooManyRequests {
		return failed("rate_limited")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failed(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	var payload []struct {
		Status      string `json:"status"`
		Message     string `json:"message"`
		Query       string `json:"query"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		ISP         string `json:"isp"`
		Org         string `json:"org"`
		AS          string `json:"as"`
		Mobile      *bool  `json:"mobile"`
		Proxy       *bool  `json:"proxy"`
		Hosting     *bool  `json:"hosting"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil {
		return failed("invalid_json")
	}
	out := failed("missing_result")
	requested := make(map[string]string, len(ips))
	for _, ip := range ips {
		requested[canonicalIP(ip)] = ip
	}
	for _, item := range payload {
		result := Result{Provider: "ip-api", Status: StatusSuccess, IP: item.Query, Family: ipFamily(item.Query), Country: item.Country, CountryCode: item.CountryCode, ASN: item.AS, Org: item.Org, ISP: item.ISP, Proxy: item.Proxy, Hosting: item.Hosting, Mobile: item.Mobile, CheckedAt: now}
		if item.Status != "success" {
			result.Status, result.Reason = StatusFailed, item.Message
		}
		key := item.Query
		if original, ok := requested[canonicalIP(item.Query)]; ok {
			key = original
		}
		out[key] = result
	}
	return out
}

func (c *IPAPIClient) wait(ctx context.Context) error {
	c.mu.Lock()
	next := c.next
	interval := c.MinInterval
	if interval == 0 {
		interval = 4 * time.Second
	}
	if interval > 0 && c.last.Add(interval).After(next) {
		next = c.last.Add(interval)
	}
	c.mu.Unlock()
	if wait := time.Until(next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
	return nil
}

func (c *IPAPIClient) applyBackoff(resp *http.Response) {
	remainingHeader := resp.Header.Get("X-Rl")
	remaining, _ := strconv.Atoi(remainingHeader)
	ttl, _ := strconv.Atoi(resp.Header.Get("X-Ttl"))
	if resp.StatusCode != http.StatusTooManyRequests && (remainingHeader == "" || remaining != 0) {
		return
	}
	if ttl <= 0 {
		ttl = 60
	}
	c.mu.Lock()
	c.next = time.Now().Add(time.Duration(ttl) * time.Second)
	c.mu.Unlock()
}

func ipFamily(raw string) string {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return "ipv4"
	}
	return "ipv6"
}

func canonicalIP(raw string) string {
	if parsed := net.ParseIP(strings.TrimSpace(raw)); parsed != nil {
		return parsed.String()
	}
	return strings.TrimSpace(raw)
}

func safeProviderError(err error, target string) string {
	if err == nil {
		return ""
	}
	parsed, parseErr := url.Parse(target)
	if parseErr != nil {
		return "provider request failed"
	}
	userInfo := ""
	if parsed.User != nil {
		userInfo = parsed.User.String() + "@"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	message := strings.ReplaceAll(err.Error(), target, parsed.String())
	if userInfo != "" {
		message = strings.ReplaceAll(message, userInfo, "")
	}
	return message
}

var _ Provider = IPPureProvider{}
