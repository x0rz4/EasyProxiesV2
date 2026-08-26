package unlock

import (
	"fmt"
	"regexp"
	"strings"
)

const youtubeProbeBodyLimit int64 = 1024 * 1024

const youtubeProbeCookie = "YSC=FSCWhKo2Zgw; VISITOR_PRIVACY_METADATA=CgJERRIEEgAgYQ%3D%3D; PREF=f7=4000; SOCS=CAISOAgDEitib3FfaWRlbnRpZnlmcm9udGVuZHVpc2VydmVyXzIwMjQwNTI2LjAxX3Aw"

var youtubeRegionPattern = regexp.MustCompile(`(?i)"INNERTUBE_CONTEXT_GL"\s*:\s*"([^"]+)"`)

type youtubeChecker struct{}

func (youtubeChecker) Key() string       { return "youtube" }
func (youtubeChecker) Aliases() []string { return []string{"youtube_premium", "ytpremium"} }
func (youtubeChecker) DisplayName() string {
	return "YouTube Premium"
}
func (youtubeChecker) Order() int { return 40 }
func (youtubeChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "youtube", Label: "YouTube Premium", Description: "检测 Premium 地区支持及页面可用标记", Category: "streaming", Order: 40}
}

func (youtubeChecker) Check(runtime Runtime) ServiceResult {
	resp, err := fetchProbeWithLimit(runtime, "https://www.youtube.com/premium", map[string]string{"Cookie": youtubeProbeCookie}, youtubeProbeBodyLimit)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: "探测失败: " + err.Error()}
	}
	return evaluateYouTube(runtime.LandingCountry, resp)
}

func evaluateYouTube(landingCountry string, response *probeResponse) ServiceResult {
	if response == nil {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: "响应为空"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: fmt.Sprintf("HTTP %d", response.StatusCode)}
	}
	if containsAny(response.Body, "www.google.cn") {
		return ServiceResult{Status: StatusLocked, Region: "CN", Detail: "出口被导向 Google 中国"}
	}
	if containsAny(response.Body, "premium is not available in your country", "premium isn't available in your country") {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "当前地区不支持 Premium"}
	}
	region := ""
	if matches := youtubeRegionPattern.FindStringSubmatch(response.RawBody); len(matches) >= 2 {
		region = strings.ToUpper(strings.TrimSpace(matches[1]))
	}
	if containsAny(response.Body, "ad-free") {
		return ServiceResult{Status: StatusUnlocked, Region: region, Detail: "Premium 可用"}
	}
	return ServiceResult{Status: StatusFailed, Region: region, Detail: "页面判定标记缺失"}
}

func init() { RegisterChecker(youtubeChecker{}) }
