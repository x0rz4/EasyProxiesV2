package unlock

import "net/http"

type openAIChecker struct{}

func (openAIChecker) Key() string         { return "chatgpt" }
func (openAIChecker) Aliases() []string   { return []string{"openai", "open_ai"} }
func (openAIChecker) DisplayName() string { return "ChatGPT" }
func (openAIChecker) Order() int          { return 30 }
func (openAIChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "chatgpt", Label: "ChatGPT / OpenAI", Description: "检测网页合规接口与移动端入口", Category: "ai", Order: 30}
}

func (openAIChecker) Check(runtime Runtime) ServiceResult {
	region := runtime.LandingCountry
	compliance, err := fetchProbe(runtime, "https://api.openai.com/compliance/cookie_requirements", map[string]string{
		"Accept": "*/*", "Authorization": "Bearer null", "Content-Type": "application/json",
		"Origin": "https://platform.openai.com", "Referer": "https://platform.openai.com/",
	})
	if err != nil {
		return ServiceResult{Status: StatusFailed, Region: region, Detail: "合规接口探测失败: " + err.Error()}
	}
	ios, err := fetchProbe(runtime, "https://ios.chat.openai.com/", map[string]string{"Accept": "*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"})
	if err != nil {
		return ServiceResult{Status: StatusFailed, Region: region, Detail: "移动端入口探测失败: " + err.Error()}
	}
	return evaluateOpenAI(region, compliance, ios)
}

func evaluateOpenAI(region string, compliance, ios *probeResponse) ServiceResult {
	result := ServiceResult{Region: region}
	if compliance == nil || ios == nil {
		result.Status, result.Detail = StatusFailed, "响应为空"
		return result
	}
	complianceBlocked := containsAny(compliance.Body, "unsupported_country") || isRestrictedStatus(compliance.StatusCode)
	iosBlocked := containsAny(ios.Body, "vpn") || isRestrictedStatus(ios.StatusCode)
	complianceReachable := compliance.StatusCode >= 200 && compliance.StatusCode < 400
	iosReachable := ios.StatusCode >= 200 && ios.StatusCode < 400
	switch {
	case !complianceBlocked && !iosBlocked && complianceReachable && iosReachable:
		result.Status, result.Detail = StatusUnlocked, "网页与移动端均可用"
	case complianceBlocked && iosBlocked:
		result.Status, result.Detail = StatusLocked, "地区或出口受限"
	case complianceReachable && !complianceBlocked && iosBlocked:
		result.Status, result.Detail = StatusPartial, "仅网页端可用"
	case complianceBlocked && iosReachable && !iosBlocked:
		result.Status, result.Detail = StatusPartial, "仅移动端可用"
	default:
		result.Status, result.Detail = StatusFailed, "服务响应异常"
	}
	return result
}

func isRestrictedStatus(status int) bool {
	return status == http.StatusForbidden || status == http.StatusUnavailableForLegalReasons
}

func init() { RegisterChecker(openAIChecker{}) }
