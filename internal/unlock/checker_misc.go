package unlock

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

var tiktokRegionPattern = regexp.MustCompile(`(?i)"(?:sys_)?region"\s*:\s*"([A-Za-z]{2})"`)

type tiktokChecker struct{}

func (tiktokChecker) Key() string         { return "tiktok" }
func (tiktokChecker) Aliases() []string   { return nil }
func (tiktokChecker) DisplayName() string { return "TikTok" }
func (tiktokChecker) Order() int          { return 50 }
func (tiktokChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "tiktok", Label: "TikTok", Description: "检测 TikTok 地区入口", Category: "social", Order: 50}
}
func (tiktokChecker) Check(runtime Runtime) ServiceResult {
	resp, err := fetchProbeWithLimit(runtime, "https://www.tiktok.com/explore", nil, 128*1024)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "探测失败: " + err.Error()}
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnavailableForLegalReasons {
		return ServiceResult{Status: StatusLocked, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if matches := tiktokRegionPattern.FindStringSubmatch(resp.RawBody); len(matches) >= 2 {
		return ServiceResult{Status: StatusUnlocked, Region: strings.ToUpper(matches[1]), Detail: "地区入口可用"}
	}
	return ServiceResult{Status: StatusFailed, Detail: "页面地区标记缺失，无法可靠判断"}
}

type amazonChecker struct{}

func (amazonChecker) Key() string         { return "amazon" }
func (amazonChecker) Aliases() []string   { return []string{"primevideo", "prime_video"} }
func (amazonChecker) DisplayName() string { return "Prime Video" }
func (amazonChecker) Order() int          { return 60 }
func (amazonChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "amazon", Label: "Prime Video", Description: "检测 Prime Video 服务入口", Category: "streaming", Order: 60}
}
func (amazonChecker) Check(runtime Runtime) ServiceResult {
	resp, err := fetchProbe(runtime, "https://www.primevideo.com", nil)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "探测失败: " + err.Error()}
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		return ServiceResult{Status: StatusUnlocked, Region: runtime.LandingCountry, Detail: "入口可用"}
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnavailableForLegalReasons:
		return ServiceResult{Status: StatusLocked, Region: runtime.LandingCountry, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	default:
		return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
}

type redditChecker struct{}

func (redditChecker) Key() string         { return "reddit" }
func (redditChecker) Aliases() []string   { return nil }
func (redditChecker) DisplayName() string { return "Reddit" }
func (redditChecker) Order() int          { return 70 }
func (redditChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "reddit", Label: "Reddit", Description: "检测 Reddit 服务入口", Category: "social", Order: 70}
}
func (redditChecker) Check(runtime Runtime) ServiceResult {
	resp, err := fetchProbe(runtime, "https://www.reddit.com", nil)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "探测失败: " + err.Error()}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return ServiceResult{Status: StatusUnlocked, Region: runtime.LandingCountry, Detail: "入口可用"}
	}
	if resp.StatusCode == http.StatusForbidden {
		return ServiceResult{Status: StatusLocked, Region: runtime.LandingCountry, Detail: "数据中心出口被拒绝"}
	}
	return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func init() {
	RegisterChecker(tiktokChecker{})
	RegisterChecker(amazonChecker{})
	RegisterChecker(redditChecker{})
}
