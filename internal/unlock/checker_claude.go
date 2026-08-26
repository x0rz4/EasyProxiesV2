package unlock

import (
	"fmt"
	"net/http"
	"strings"
)

type claudeChecker struct{}

func (claudeChecker) Key() string         { return "claude" }
func (claudeChecker) Aliases() []string   { return []string{"anthropic", "anthropic_claude"} }
func (claudeChecker) DisplayName() string { return "Claude" }
func (claudeChecker) Order() int          { return 37 }
func (claudeChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "claude", Label: "Claude", Description: "检测 Anthropic Claude 服务地区可访问性", Category: "ai", Order: 37}
}

func (claudeChecker) Check(runtime Runtime) ServiceResult {
	response, err := fetchProbe(runtime, "https://claude.ai/", nil)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: "探测失败: " + err.Error()}
	}
	return evaluateClaude(runtime.LandingCountry, response)
}

func evaluateClaude(landingCountry string, response *probeResponse) ServiceResult {
	if response == nil {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: "响应为空"}
	}
	finalURL := strings.TrimRight(strings.ToLower(strings.TrimSpace(response.FinalURL)), "/")
	if finalURL == "https://claude.ai" {
		return ServiceResult{Status: StatusUnlocked, Region: landingCountry, Detail: "Claude 可用"}
	}
	if finalURL == "https://www.anthropic.com/app-unavailable-in-region" {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "当前地区不可用"}
	}
	if response.StatusCode == http.StatusForbidden {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "HTTP 403"}
	}
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: "未知跳转: " + response.FinalURL}
	}
	return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: fmt.Sprintf("HTTP %d", response.StatusCode)}
}

func init() { RegisterChecker(claudeChecker{}) }
