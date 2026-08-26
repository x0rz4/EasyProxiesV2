package unlock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	json "easy_proxies/internal/jsonx"
)

const (
	bahamutProbeBodyLimit int64 = 32 * 1024
	bahamutTraceBodyLimit int64 = 8 * 1024
	bahamutDefaultTimeout       = 5 * time.Second
	bahamutMaxAttempts          = 2
	bahamutAdID                 = "89422"
	bahamutLenientSN            = "37783"
	bahamutStrictSN             = "38832"
)

type bahamutChecker struct{}

func (bahamutChecker) Key() string { return "bahamut" }
func (bahamutChecker) Aliases() []string {
	return []string{"bahamut_anime", "bahamutanime", "ani_gamer", "anime_gamer"}
}
func (bahamutChecker) DisplayName() string { return "Bahamut Anime" }
func (bahamutChecker) Order() int          { return 45 }
func (bahamutChecker) Meta() ProviderMeta {
	return ProviderMeta{Value: "bahamut", Label: "Bahamut Anime", Description: "检测巴哈姆特动画疯地区可访问性", Category: "streaming", Order: 45}
}

func (bahamutChecker) Check(runtime Runtime) ServiceResult {
	sessionRuntime := bahamutRuntime(runtime)
	headers := bahamutHeaders()
	deviceResponse, err := bahamutFetch(sessionRuntime, "https://ani.gamer.com.tw/ajax/getdeviceid.php", headers, bahamutProbeBodyLimit)
	if err != nil {
		return bahamutFailure(runtime, "设备探测失败: "+err.Error())
	}
	deviceID, unsupported := evaluateBahamutDeviceID(deviceResponse.RawBody)
	if unsupported {
		return evaluateBahamut(runtime.LandingCountry, deviceResponse, nil, nil, nil)
	}
	if deviceID == "" {
		return bahamutFailure(runtime, "设备 ID 缺失")
	}
	lenient, err := bahamutFetch(sessionRuntime, bahamutTokenURL(bahamutLenientSN, deviceID), headers, bahamutProbeBodyLimit)
	if err != nil {
		return bahamutFailure(runtime, "入口令牌探测失败: "+err.Error())
	}
	if !evaluateBahamutToken(lenient.RawBody) {
		return evaluateBahamut(runtime.LandingCountry, deviceResponse, lenient, nil, nil)
	}
	strict, err := bahamutFetch(sessionRuntime, bahamutTokenURL(bahamutStrictSN, deviceID), headers, bahamutProbeBodyLimit)
	if err != nil {
		return bahamutFailure(runtime, "台湾令牌探测失败: "+err.Error())
	}
	if evaluateBahamutToken(strict.RawBody) {
		return evaluateBahamut(runtime.LandingCountry, deviceResponse, lenient, strict, nil)
	}
	trace, _ := bahamutFetch(sessionRuntime, "https://ani.gamer.com.tw/cdn-cgi/trace", headers, bahamutTraceBodyLimit)
	return evaluateBahamut(runtime.LandingCountry, deviceResponse, lenient, strict, trace)
}

func evaluateBahamut(landingCountry string, device, lenient, strict, trace *probeResponse) ServiceResult {
	if device == nil || device.RawBody == "" {
		return ServiceResult{Status: StatusFailed, Region: landingCountry, Detail: "设备响应为空"}
	}
	_, unsupported := evaluateBahamutDeviceID(device.RawBody)
	if unsupported {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "当前地区不在服务范围"}
	}
	if lenient == nil || !evaluateBahamutToken(lenient.RawBody) {
		return ServiceResult{Status: StatusLocked, Region: landingCountry, Detail: "入口动画令牌不可用"}
	}
	if strict != nil && evaluateBahamutToken(strict.RawBody) {
		return ServiceResult{Status: StatusUnlocked, Region: "TW", Detail: "台湾完整解锁"}
	}
	region := ""
	if trace != nil {
		region = evaluateBahamutRegion(trace.RawBody)
	}
	if region == "" {
		return ServiceResult{Status: StatusPartial, Region: landingCountry, Detail: "入口可用，但无法确认具体地区"}
	}
	return ServiceResult{Status: StatusUnlocked, Region: region, Detail: "动画疯可用"}
}

func evaluateBahamutDeviceID(rawBody string) (string, bool) {
	var response struct {
		DeviceID string `json:"deviceid"`
	}
	if err := json.Unmarshal([]byte(rawBody), &response); err != nil {
		return "", strings.HasPrefix(strings.TrimSpace(rawBody), "<")
	}
	return response.DeviceID, false
}

func evaluateBahamutToken(rawBody string) bool {
	var response struct {
		AnimeSN int `json:"animeSn"`
	}
	return json.Unmarshal([]byte(rawBody), &response) == nil && response.AnimeSN != 0
}

func evaluateBahamutRegion(rawBody string) string {
	for _, line := range strings.Split(rawBody, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == "loc" {
			value = strings.ToUpper(strings.TrimSpace(value))
			if len(value) == 2 {
				return value
			}
		}
	}
	return ""
}

func bahamutRuntime(runtime Runtime) Runtime {
	timeout := runtime.Timeout
	if timeout <= 0 || timeout > bahamutDefaultTimeout {
		timeout = bahamutDefaultTimeout
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: runtime.Client.Transport,
		Timeout:   timeout,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return Runtime{Context: runtime.Context, Client: client, Timeout: timeout, LandingCountry: runtime.LandingCountry}
}

func bahamutFetch(runtime Runtime, target string, headers map[string]string, bodyLimit int64) (*probeResponse, error) {
	var lastErr error
	for attempt := 0; attempt < bahamutMaxAttempts; attempt++ {
		response, err := bahamutFetchOnce(runtime, target, headers, bodyLimit)
		if err == nil && response.StatusCode < 500 {
			return response, nil
		}
		if err == nil {
			err = fmt.Errorf("HTTP %d", response.StatusCode)
		}
		lastErr = err
		if !bahamutRetryable(err) {
			break
		}
	}
	return nil, lastErr
}

func bahamutFetchOnce(runtime Runtime, target string, headers map[string]string, bodyLimit int64) (*probeResponse, error) {
	ctx := runtime.Context
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, runtime.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := runtime.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit))
	if err != nil {
		return nil, err
	}
	rawBody := string(body)
	return &probeResponse{StatusCode: response.StatusCode, FinalURL: target, RawBody: rawBody, Body: strings.ToLower(rawBody), Header: response.Header.Clone()}, nil
}

func bahamutRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return strings.HasPrefix(err.Error(), "HTTP 5")
}

func bahamutTokenURL(sn, deviceID string) string {
	return fmt.Sprintf("https://ani.gamer.com.tw/ajax/token.php?adID=%s&sn=%s&device=%s", bahamutAdID, sn, url.QueryEscape(deviceID))
}

func bahamutHeaders() map[string]string {
	return map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0.0.0 Safari/537.36",
	}
}

func bahamutFailure(runtime Runtime, detail string) ServiceResult {
	return ServiceResult{Status: StatusFailed, Region: runtime.LandingCountry, Detail: detail}
}

func init() { RegisterChecker(bahamutChecker{}) }
