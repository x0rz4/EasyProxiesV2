package unlock

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// checkYouTube probes YouTube Premium unlock status.
func checkYouTube(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "youtube", DisplayName: "YouTube"}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.youtube.com/premium", nil)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en")
	// Use some standard cookies to bypass basic consent blocks if needed
	req.Header.Set("Cookie", "CONSENT=YES+cb.20220301-11-p0.en+FX+700;")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536)) // Need a larger limit to catch region
	content := string(body)

	if strings.Contains(content, "www.google.cn") {
		res.Status = StatusLocked
		res.Detail = "CN Blocked"
		res.Region = "CN"
		return res
	}

	// Try to extract contentRegion
	// e.g. "contentRegion":"US"
	region := ""
	if idx := strings.Index(content, `"contentRegion":"`); idx != -1 {
		start := idx + len(`"contentRegion":"`)
		if end := strings.Index(content[start:], `"`); end != -1 {
			region = content[start : start+end]
		}
	}
	res.Region = region

	isNotAvailable := strings.Contains(content, "Premium is not available in your country") || strings.Contains(content, "Premium isn't available in your country")
	isAvailable := strings.Contains(content, "ad-free") || strings.Contains(content, "YouTube Premium")

	if isNotAvailable {
		res.Status = StatusLocked
		res.Detail = "Not Available"
	} else if isAvailable {
		res.Status = StatusUnlocked
	} else {
		res.Status = StatusFailed
	}
	return res
}

// checkTikTok probes TikTok unlock status.
func checkTikTok(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "tiktok", DisplayName: "TikTok"}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.tiktok.com/", nil)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	content := string(body)

	if strings.Contains(content, "Please wait...") {
		// Retry with /explore
		req, _ = http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.tiktok.com/explore", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
		req.Header.Set("Accept-Language", "en")
		resp2, err2 := client.Do(req)
		if err2 == nil {
			body, _ = io.ReadAll(io.LimitReader(resp2.Body, 65536))
			content = string(body)
			resp2.Body.Close()
		}
	}

	// Try to extract region: e.g. "region":"US" or "sys_region":"US"
	region := ""
	if idx := strings.Index(content, `"region":"`); idx != -1 {
		start := idx + len(`"region":"`)
		if end := strings.Index(content[start:], `"`); end != -1 {
			region = content[start : start+end]
		}
	}
	
	if region != "" {
		res.Region = region
		res.Status = StatusUnlocked
	} else {
		res.Status = StatusLocked
	}

	return res
}

// checkAmazon probes Amazon Prime Video unlock status.
func checkAmazon(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "amazon", DisplayName: "PrimeVideo"}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.primevideo.com", nil)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		res.Status = StatusUnlocked
	} else {
		res.Status = StatusLocked
	}
	return res
}

// checkReddit probes Reddit unlock status.
func checkReddit(ctx context.Context, client *http.Client, timeout time.Duration) ServiceResult {
	res := ServiceResult{Name: "reddit", DisplayName: "Reddit"}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.reddit.com", nil)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		res.Status = StatusFailed
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 403 {
		// Sometimes Reddit blocks datacenter IPs with 403.
		if resp.StatusCode == 403 {
			res.Status = StatusLocked
		} else {
			res.Status = StatusUnlocked
		}
	} else {
		res.Status = StatusFailed
	}
	return res
}
