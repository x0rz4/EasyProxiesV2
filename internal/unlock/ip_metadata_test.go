package unlock

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func metadataRuntime(transport http.RoundTripper) Runtime {
	return Runtime{
		Context: context.Background(),
		Client:  &http.Client{Transport: transport},
		Timeout: time.Second,
	}
}

func TestFetchIPMetadata(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		requestErr error
		wantASN    string
		wantOrg    string
		wantCode   string
	}{
		{name: "organization", status: http.StatusOK, body: `{"status":"success","country":"Hong Kong","countryCode":"hk","regionName":"Central","isp":"Example ISP","org":"Example Org","as":"AS4515 Example"}`, wantASN: "AS4515 Example", wantOrg: "Example Org", wantCode: "HK"},
		{name: "isp fallback", status: http.StatusOK, body: `{"status":"success","countryCode":"jp","isp":"Fallback ISP","org":"","as":"AS2516"}`, wantASN: "AS2516", wantOrg: "Fallback ISP", wantCode: "JP"},
		{name: "provider failure", status: http.StatusOK, body: `{"status":"fail","message":"private range"}`},
		{name: "non 200", status: http.StatusTooManyRequests, body: `rate limited`},
		{name: "invalid json", status: http.StatusOK, body: `{`},
		{name: "request failure", requestErr: errors.New("network unavailable")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := metadataRuntime(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "ip-api.com" || !strings.Contains(request.URL.Path, "198.51.100.7") {
					t.Fatalf("unexpected metadata URL: %s", request.URL)
				}
				if test.requestErr != nil {
					return nil, test.requestErr
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header), Request: request}, nil
			}))
			got := fetchIPMetadata(runtime, "198.51.100.7")
			if got.ASN != test.wantASN || got.Org != test.wantOrg || got.ISOCode != test.wantCode {
				t.Fatalf("metadata = %+v", got)
			}
		})
	}
}

func TestMergeIPMetadataPreservesExistingGeography(t *testing.T) {
	got := mergeIPMetadata(
		IPInfo{IP: "198.51.100.7", Country: "GeoIP Country", ISOCode: "US", Region: "us"},
		IPInfo{Country: "API Country", ISOCode: "CA", Region: "Ontario", ASN: "AS64500", Org: "Example Org"},
	)
	if got.Country != "GeoIP Country" || got.ISOCode != "US" || got.Region != "us" || got.ASN != "AS64500" || got.Org != "Example Org" {
		t.Fatalf("merged metadata = %+v", got)
	}
}

func TestFetchIPMetadataTimeoutIsBestEffort(t *testing.T) {
	runtime := metadataRuntime(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	runtime.Timeout = 5 * time.Millisecond

	got := fetchIPMetadata(runtime, "198.51.100.7")
	if got.IP != "198.51.100.7" || got.ASN != "" || got.Org != "" {
		t.Fatalf("metadata after timeout = %+v", got)
	}
}
