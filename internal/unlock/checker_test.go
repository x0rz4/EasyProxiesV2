package unlock

import (
	"net/http"
	"strings"
	"testing"
)

func testResponse(status int, finalURL, body string) *probeResponse {
	return &probeResponse{StatusCode: status, FinalURL: finalURL, Body: strings.ToLower(body), RawBody: body}
}

func TestEvaluateNetflix(t *testing.T) {
	tests := []struct {
		name              string
		primary, fallback *probeResponse
		status, region    string
	}{
		{name: "full", primary: testResponse(200, "https://www.netflix.com/us/title/1", `{"id":"US","countryName":"United States"}`), fallback: testResponse(200, "", "Oh no!"), status: StatusUnlocked, region: "US"},
		{name: "originals only", primary: testResponse(200, "", "Oh no!"), fallback: testResponse(200, "", "Oh no!"), status: StatusOriginalsOnly},
		{name: "nsez blocked", primary: testResponse(200, "https://www.netflix.com/nsez-403", ""), fallback: testResponse(200, "", ""), status: StatusLocked},
		{name: "http blocked", primary: testResponse(403, "", ""), fallback: testResponse(200, "", ""), status: StatusLocked},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateNetflix(test.primary, test.fallback)
			if result.Status != test.status || result.Region != test.region {
				t.Fatalf("result=%+v want status=%s region=%s", result, test.status, test.region)
			}
		})
	}
}

func TestEvaluateDisney(t *testing.T) {
	device := testResponse(200, "", `{"assertion":"device"}`)
	token := testResponse(200, "", `{"refresh_token":"refresh"}`)
	tests := []struct {
		name                   string
		device, token, graph   *probeResponse
		preview                *probeResponse
		status, region, detail string
	}{
		{name: "blocked device", device: testResponse(403, "", "403 ERROR"), status: StatusLocked},
		{name: "missing assertion", device: testResponse(200, "", `{}`), status: StatusFailed},
		{name: "available", device: device, token: token, graph: testResponse(200, "", `{"countryCode":"US","inSupportedLocation":true}`), preview: testResponse(200, "https://disneyplus.com/home", ""), status: StatusUnlocked, region: "US"},
		{name: "partial", device: device, token: token, graph: testResponse(200, "", `{"countryCode":"KR","inSupportedLocation":false}`), preview: testResponse(200, "https://disneyplus.com/home", ""), status: StatusPartial, region: "KR"},
		{name: "preview unavailable", device: device, token: token, graph: testResponse(200, "", `{"countryCode":"US","inSupportedLocation":true}`), preview: testResponse(200, "https://disneyplus.com/unavailable", ""), status: StatusLocked, region: "US"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateDisney(test.device, test.token, test.graph, test.preview)
			if result.Status != test.status || result.Region != test.region {
				t.Fatalf("result=%+v want status=%s region=%s", result, test.status, test.region)
			}
		})
	}
}

func TestEvaluateOpenAI(t *testing.T) {
	tests := []struct {
		name, compliance, ios, status string
	}{
		{name: "available", status: StatusUnlocked},
		{name: "blocked", compliance: "unsupported_country", ios: "VPN", status: StatusLocked},
		{name: "web only", ios: "VPN", status: StatusPartial},
		{name: "mobile only", compliance: "unsupported_country", status: StatusPartial},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateOpenAI("US", testResponse(200, "", test.compliance), testResponse(200, "", test.ios))
			if result.Status != test.status || result.Region != "US" {
				t.Fatalf("result=%+v want=%s", result, test.status)
			}
		})
	}
}

func TestEvaluateYouTube(t *testing.T) {
	tests := []struct {
		name, body, status, region string
	}{
		{name: "available", body: `{"INNERTUBE_CONTEXT_GL":"JP"} ad-free`, status: StatusUnlocked, region: "JP"},
		{name: "unsupported", body: "Premium is not available in your country", status: StatusLocked, region: "US"},
		{name: "china", body: "https://www.google.cn", status: StatusLocked, region: "CN"},
		{name: "marker missing", body: `{"INNERTUBE_CONTEXT_GL":"HK"}`, status: StatusFailed, region: "HK"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateYouTube("US", testResponse(http.StatusOK, "", test.body))
			if result.Status != test.status || result.Region != test.region {
				t.Fatalf("result=%+v want status=%s region=%s", result, test.status, test.region)
			}
		})
	}
	if youtubeProbeBodyLimit < 768*1024 {
		t.Fatalf("youtube body limit=%d, expected at least 768 KiB", youtubeProbeBodyLimit)
	}
}

func TestEvaluateGemini(t *testing.T) {
	tests := []struct {
		name, body, status string
	}{
		{name: "available", body: "45631641,null,true", status: StatusUnlocked},
		{name: "region blocked", body: "Gemini isn't available in your country", status: StatusLocked},
		{name: "marker missing", body: "regular page", status: StatusLocked},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateGemini("US", testResponse(http.StatusOK, "", test.body))
			if result.Status != test.status || result.Region != "US" {
				t.Fatalf("result=%+v want=%s", result, test.status)
			}
		})
	}
}

func TestEvaluateClaude(t *testing.T) {
	tests := []struct {
		name, finalURL, status string
		code                   int
	}{
		{name: "available", finalURL: "https://claude.ai/", code: http.StatusOK, status: StatusUnlocked},
		{name: "region blocked", finalURL: "https://www.anthropic.com/app-unavailable-in-region", code: http.StatusOK, status: StatusLocked},
		{name: "forbidden", finalURL: "https://claude.ai/login", code: http.StatusForbidden, status: StatusLocked},
		{name: "unexpected redirect", finalURL: "https://claude.ai/login", code: http.StatusOK, status: StatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateClaude("JP", testResponse(test.code, test.finalURL, ""))
			if result.Status != test.status || result.Region != "JP" {
				t.Fatalf("result=%+v want=%s", result, test.status)
			}
		})
	}
}

func TestEvaluateBahamutHelpers(t *testing.T) {
	if deviceID, unsupported := evaluateBahamutDeviceID(`{"deviceid":"abc123"}`); deviceID != "abc123" || unsupported {
		t.Fatalf("deviceID=%q unsupported=%v", deviceID, unsupported)
	}
	if _, unsupported := evaluateBahamutDeviceID(`<html>blocked</html>`); !unsupported {
		t.Fatal("HTML response was not classified as unsupported")
	}
	if !evaluateBahamutToken(`{"animeSn":37783}`) || evaluateBahamutToken(`{"animeSn":0}`) {
		t.Fatal("unexpected token evaluation")
	}
	if region := evaluateBahamutRegion("ip=1.2.3.4\nloc=hk\n"); region != "HK" {
		t.Fatalf("region=%q", region)
	}
}

func TestEvaluateBahamut(t *testing.T) {
	device := testResponse(200, "", `{"deviceid":"dev123"}`)
	lenient := testResponse(200, "", `{"animeSn":37783}`)
	strict := testResponse(200, "", `{"animeSn":38832}`)
	strictFail := testResponse(200, "", `{"animeSn":0}`)
	tests := []struct {
		name                    string
		device, lenient, strict *probeResponse
		trace                   *probeResponse
		status, region          string
	}{
		{name: "unsupported", device: testResponse(200, "", `<html>blocked</html>`), status: StatusLocked, region: "US"},
		{name: "lenient unavailable", device: device, lenient: strictFail, status: StatusLocked, region: "US"},
		{name: "taiwan", device: device, lenient: lenient, strict: strict, status: StatusUnlocked, region: "TW"},
		{name: "hong kong", device: device, lenient: lenient, strict: strictFail, trace: testResponse(200, "", "loc=HK\n"), status: StatusUnlocked, region: "HK"},
		{name: "region unknown", device: device, lenient: lenient, strict: strictFail, status: StatusPartial, region: "US"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := evaluateBahamut("US", test.device, test.lenient, test.strict, test.trace)
			if result.Status != test.status || result.Region != test.region {
				t.Fatalf("result=%+v want status=%s region=%s", result, test.status, test.region)
			}
		})
	}
}
