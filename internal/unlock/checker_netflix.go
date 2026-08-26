package unlock

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const netflixProbeBodyLimit int64 = 512 * 1024

var netflixRegionPattern = regexp.MustCompile(`(?s)"id":"([A-Za-z]{2})".*"countryName":"[^"]*"`)

type netflixChecker struct{}

func (netflixChecker) Key() string         { return "netflix" }
func (netflixChecker) Aliases() []string   { return []string{"nf"} }
func (netflixChecker) DisplayName() string { return "Netflix" }
func (netflixChecker) Order() int          { return 10 }
func (netflixChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "netflix", Label: "Netflix", Description: "检测完整区服与 Originals 可用性", Category: "streaming", Order: 10}
}

func (netflixChecker) Check(runtime Runtime) ServiceResult {
	headers := map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}
	primary, err := fetchProbeWithLimit(runtime, "https://www.netflix.com/title/81280792", headers, netflixProbeBodyLimit)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "主标题探测失败: " + err.Error()}
	}
	fallback, err := fetchProbeWithLimit(runtime, "https://www.netflix.com/title/70143836", headers, netflixProbeBodyLimit)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "对照标题探测失败: " + err.Error()}
	}
	return evaluateNetflix(primary, fallback)
}

func evaluateNetflix(primary, fallback *probeResponse) ServiceResult {
	result := ServiceResult{Region: netflixRegion(primary)}
	if primary == nil || fallback == nil {
		result.Status, result.Detail = StatusFailed, "响应为空"
		return result
	}
	if containsAny(primary.Body, "nsez-403") || containsAny(primary.FinalURL, "nsez-403") ||
		containsAny(fallback.Body, "nsez-403") || containsAny(fallback.FinalURL, "nsez-403") {
		result.Status, result.Detail = StatusLocked, "Netflix 拒绝当前出口"
		return result
	}
	if primary.StatusCode >= http.StatusBadRequest || fallback.StatusCode >= http.StatusBadRequest {
		result.Status = StatusLocked
		result.Detail = fmt.Sprintf("HTTP %d/%d", primary.StatusCode, fallback.StatusCode)
		return result
	}
	primaryBlocked := containsAny(primary.Body, "oh no!")
	fallbackBlocked := containsAny(fallback.Body, "oh no!")
	if primaryBlocked && fallbackBlocked {
		result.Status, result.Detail = StatusOriginalsOnly, "仅 Netflix Originals"
		return result
	}
	if !primaryBlocked || !fallbackBlocked {
		result.Status, result.Detail = StatusUnlocked, "完整解锁"
		return result
	}
	result.Status, result.Detail = StatusFailed, "页面判定标记缺失"
	return result
}

func netflixRegion(response *probeResponse) string {
	if response == nil {
		return ""
	}
	if matches := netflixRegionPattern.FindStringSubmatch(response.RawBody); len(matches) >= 2 {
		return strings.ToUpper(strings.TrimSpace(matches[1]))
	}
	parsed, err := url.Parse(response.FinalURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) > 0 && len(parts[0]) == 2 {
		return strings.ToUpper(parts[0])
	}
	return ""
}

func init() { RegisterChecker(netflixChecker{}) }
