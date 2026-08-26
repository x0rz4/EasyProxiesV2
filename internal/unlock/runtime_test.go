package unlock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchProbeCapturesFinalURLAndHonorsBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(w, request, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("A", 128)))
	}))
	defer server.Close()
	runtime := Runtime{Context: context.Background(), Client: server.Client(), Timeout: time.Second}
	response, err := fetchProbeWithLimit(runtime, server.URL+"/redirect", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(response.FinalURL, "/final") || len(response.RawBody) != 32 || response.Body != strings.Repeat("a", 32) {
		t.Fatalf("response=%+v", response)
	}
}

type panicChecker struct{ stubChecker }

func (panicChecker) Check(Runtime) ServiceResult { panic("boom") }

func TestRunCheckerRecoversAndNormalizes(t *testing.T) {
	checker := panicChecker{stubChecker{key: "panic", label: "Panic", order: 1}}
	result := runChecker(Runtime{}, checker)
	if result.Name != "panic" || result.DisplayName != "Panic" || result.Status != StatusFailed || !strings.Contains(result.Detail, "panic") {
		t.Fatalf("result=%+v", result)
	}
}
