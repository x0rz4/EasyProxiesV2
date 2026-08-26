package unlock

import (
	"net/http"
	"regexp"
	"strings"
)

const (
	disneyBrowserBearer = "Bearer ZGlzbmV5JmJyb3dzZXImMS4wLjA.Cu56AgSfBTDag5NiRA81oLHkDZfu5L3CKadnefEAY84"
	disneyDevicePayload = `{"deviceFamily":"browser","applicationRuntime":"chrome","deviceProfile":"windows","attributes":{}}`
	disneyTokenPayload  = "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Atoken-exchange&latitude=0&longitude=0&platform=browser&subject_token={{ASSERTION}}&subject_token_type=urn%3Abamtech%3Aparams%3Aoauth%3Atoken-type%3Adevice"
	disneyGraphPayload  = `{"query":"mutation refreshToken($input: RefreshTokenInput!) { refreshToken(refreshToken: $input) { activeSession { sessionId } } }","variables":{"input":{"refreshToken":"{{TOKEN}}"}}}`
)

var (
	disneyAssertionPattern = regexp.MustCompile(`(?i)"assertion"\s*:\s*"([^"]+)"`)
	disneyRefreshPattern   = regexp.MustCompile(`(?i)"refresh_token"\s*:\s*"([^"]+)"`)
	disneyCountryPattern   = regexp.MustCompile(`(?i)"countryCode"\s*:\s*"([^"]+)"`)
	disneySupportedPattern = regexp.MustCompile(`(?i)"inSupportedLocation"\s*:\s*(false|true)`)
)

type disneyChecker struct{}

func (disneyChecker) Key() string         { return "disney_plus" }
func (disneyChecker) Aliases() []string   { return []string{"disney", "disney+", "disneyplus"} }
func (disneyChecker) DisplayName() string { return "Disney+" }
func (disneyChecker) Order() int          { return 20 }
func (disneyChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "disney_plus", Label: "Disney+", Description: "检测设备注册、令牌、地区及服务入口", Category: "streaming", Order: 20}
}

func (disneyChecker) Check(runtime Runtime) ServiceResult {
	device, err := fetchRequest(runtime, http.MethodPost, "https://disney.api.edge.bamgrid.com/devices", map[string]string{
		"Authorization": disneyBrowserBearer,
		"Content-Type":  "application/json; charset=UTF-8",
	}, []byte(disneyDevicePayload), 64*1024)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "设备注册失败: " + err.Error()}
	}
	assertion := regexValue(disneyAssertionPattern, device.RawBody)
	if assertion == "" {
		return evaluateDisney(device, nil, nil, nil)
	}
	token, err := fetchRequest(runtime, http.MethodPost, "https://disney.api.edge.bamgrid.com/token", map[string]string{
		"Authorization": disneyBrowserBearer,
		"Content-Type":  "application/x-www-form-urlencoded",
	}, []byte(strings.ReplaceAll(disneyTokenPayload, "{{ASSERTION}}", assertion)), 128*1024)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "令牌探测失败: " + err.Error()}
	}
	refreshToken := regexValue(disneyRefreshPattern, token.RawBody)
	if refreshToken == "" {
		return evaluateDisney(device, token, nil, nil)
	}
	graph, err := fetchRequest(runtime, http.MethodPost, "https://disney.api.edge.bamgrid.com/graph/v1/device/graphql", map[string]string{
		"Authorization": strings.TrimPrefix(disneyBrowserBearer, "Bearer "),
		"Content-Type":  "application/json",
	}, []byte(strings.ReplaceAll(disneyGraphPayload, "{{TOKEN}}", refreshToken)), 128*1024)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "地区探测失败: " + err.Error()}
	}
	preview, err := fetchProbe(runtime, "https://disneyplus.com", nil)
	if err != nil {
		return ServiceResult{Status: StatusFailed, Detail: "入口探测失败: " + err.Error()}
	}
	return evaluateDisney(device, token, graph, preview)
}

func evaluateDisney(device, token, graph, preview *probeResponse) ServiceResult {
	if device == nil || device.RawBody == "" {
		return ServiceResult{Status: StatusFailed, Detail: "设备响应为空"}
	}
	if containsAny(device.Body, "403 error") || device.StatusCode == http.StatusForbidden {
		return ServiceResult{Status: StatusLocked, Detail: "IP 被 Disney+ 拒绝"}
	}
	if regexValue(disneyAssertionPattern, device.RawBody) == "" {
		return ServiceResult{Status: StatusFailed, Detail: "设备断言缺失"}
	}
	if token == nil || token.RawBody == "" {
		return ServiceResult{Status: StatusFailed, Detail: "令牌响应为空"}
	}
	if containsAny(token.Body, "forbidden-location", "403 error") || token.StatusCode == http.StatusForbidden {
		return ServiceResult{Status: StatusLocked, Detail: "地区或 IP 受限"}
	}
	if regexValue(disneyRefreshPattern, token.RawBody) == "" {
		return ServiceResult{Status: StatusFailed, Detail: "刷新令牌缺失"}
	}
	if graph == nil || graph.RawBody == "" {
		return ServiceResult{Status: StatusFailed, Detail: "地区响应为空"}
	}
	region := strings.ToUpper(strings.TrimSpace(regexValue(disneyCountryPattern, graph.RawBody)))
	if region == "" {
		return ServiceResult{Status: StatusFailed, Detail: "地区字段缺失"}
	}
	if region == "JP" {
		return ServiceResult{Status: StatusUnlocked, Region: region, Detail: "完整解锁"}
	}
	if preview != nil && containsAny(preview.FinalURL, "preview", "unavailable") {
		return ServiceResult{Status: StatusLocked, Region: region, Detail: "入口跳转至不可用页面"}
	}
	supported := strings.ToLower(regexValue(disneySupportedPattern, graph.RawBody))
	switch supported {
	case "true":
		return ServiceResult{Status: StatusUnlocked, Region: region, Detail: "完整解锁"}
	case "false":
		return ServiceResult{Status: StatusPartial, Region: region, Detail: "服务已覆盖但当前地区尚未完整开放"}
	default:
		return ServiceResult{Status: StatusFailed, Region: region, Detail: "支持状态字段缺失"}
	}
}

func regexValue(pattern *regexp.Regexp, value string) string {
	matches := pattern.FindStringSubmatch(value)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

func init() { RegisterChecker(disneyChecker{}) }
