package unlock

import (
	"fmt"
	"net/http"
)

const geminiProbeBodyLimit int64 = 1024 * 1024

var geminiRestrictedMarkers = []string{
	"gemini isn't available in your country",
	"gemini is not available in your country",
	"gemini isn't available in your region",
	"gemini is not available in your region",
	"not available in your country",
	"not available in your region",
}

type geminiChecker struct{}

func (geminiChecker) Key() string         { return "gemini" }
func (geminiChecker) Aliases() []string   { return []string{"google_gemini", "bard"} }
func (geminiChecker) DisplayName() string { return "Gemini" }
func (geminiChecker) Order() int          { return 35 }
func (geminiChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "gemini", Label: "Gemini", Description: "检测 Google Gemini 服务地区可访问性", Category: "ai", Order: 35}
}

func (geminiChecker) Check(runtime Runtime) ServiceResult {
	if runtime.LandingCountry == "CN" {
		return ServiceResult{Status: StatusLocked, Region: "CN", Detail: "当前地区仅支持 Workspace 场景"}
	}
	response, err := fetchProbeWithLimit(runtime, "https://gemini.google.com/", nil, geminiProbeBodyLimit)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: "探测失败: " + err.Error()}
	}
	return evaluateGemini(runtime.LandingCountry, response)
}

func evaluateGemini(landingCountry string, response *probeResponse) ServiceResult {
	if response == nil {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: "响应为空"}
	}
	if response.StatusCode == http.StatusForbidden {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "HTTP 403"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: fmt.Sprintf("HTTP %d", response.StatusCode)}
	}
	if containsAny(response.Body, "45631641,null,true") {
		return ServiceResult{Status: StatusUnlocked, Region: landingCountry, Detail: "Gemini 可用"}
	}
	if containsAny(response.Body, geminiRestrictedMarkers...) {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "当前地区不可用"}
	}
	return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "Gemini 可用标记缺失"}
}

func init() { RegisterChecker(geminiChecker{}) }
