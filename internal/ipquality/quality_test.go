package ipquality

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testDial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func TestIPPurePreservesFalseAndZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"1.1.1.1","isBroadcast":false,"isResidential":false,"fraudScore":0}`))
	}))
	defer server.Close()
	result := (IPPureProvider{URL: server.URL, Timeout: time.Second}).Check(context.Background(), testDial)
	if result.Status != StatusSuccess || result.IsBroadcast == nil || *result.IsBroadcast || result.IsResidential == nil || *result.IsResidential || result.FraudScore == nil || *result.FraudScore != 0 {
		t.Fatalf("unexpected nullable result: %+v", result)
	}
}

func TestIPPureMissingFieldsIsPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ip":"2001:db8::1"}`)) }))
	defer server.Close()
	result := (IPPureProvider{URL: server.URL, Timeout: time.Second}).Check(context.Background(), testDial)
	if result.Status != StatusPartial || result.Family != "ipv6" || result.FraudScore != nil {
		t.Fatalf("unexpected partial result: %+v", result)
	}
}

func TestIPPureRejectsInvalidJSONAndOutOfRangeScore(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		reason string
	}{
		{name: "invalid JSON", body: `{`, reason: "invalid_json"},
		{name: "out of range", body: `{"ip":"1.1.1.1","isBroadcast":false,"isResidential":true,"fraudScore":101}`, reason: "fraud_score_out_of_range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			result := (IPPureProvider{URL: server.URL, Timeout: time.Second}).Check(context.Background(), testDial)
			if result.Reason != test.reason || (test.reason == "fraud_score_out_of_range" && result.Status != StatusPartial) {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestIPAPIBatchDoesNotInventFraudScore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rl", "14")
		_, _ = w.Write([]byte(`[{"status":"success","query":"1.1.1.1","country":"Australia","countryCode":"AU","as":"AS13335","proxy":true,"hosting":false,"mobile":false}]`))
	}))
	defer server.Close()
	client := &IPAPIClient{BaseURL: server.URL, MinInterval: -1}
	result := client.CheckBatch(context.Background(), []string{"1.1.1.1"})["1.1.1.1"]
	if result.Status != StatusSuccess || result.Proxy == nil || !*result.Proxy || result.FraudScore != nil || result.IsResidential != nil {
		t.Fatalf("unexpected ip-api result: %+v", result)
	}
}

func TestIPAPIBatchSplitsAtOneHundredAndHonoursBackoff(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var ips []string
		if err := json.NewDecoder(r.Body).Decode(&ips); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(ips) > 100 {
			http.Error(w, "batch too large", http.StatusBadRequest)
			return
		}
		payload := make([]map[string]any, 0, len(ips))
		for _, ip := range ips {
			payload = append(payload, map[string]any{"status": "success", "query": ip, "proxy": false, "hosting": false, "mobile": false})
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	ips := make([]string, 0, 205)
	for index := 1; index <= 205; index++ {
		ips = append(ips, net.IPv4(10, 0, byte(index/255), byte(index%255)).String())
	}
	client := &IPAPIClient{BaseURL: server.URL, MinInterval: -1}
	results := client.CheckBatch(context.Background(), ips)
	if requests.Load() != 3 || len(results) != len(ips) {
		t.Fatalf("requests=%d results=%d, want 3/%d", requests.Load(), len(results), len(ips))
	}

	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Rl", "0")
		w.Header().Set("X-Ttl", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rateLimited.Close()
	client = &IPAPIClient{BaseURL: rateLimited.URL, MinInterval: -1}
	result := client.CheckBatch(context.Background(), []string{"1.1.1.1"})["1.1.1.1"]
	if result.Status != StatusFailed || result.Reason != "rate_limited" || time.Until(client.next) < time.Second {
		t.Fatalf("rate-limit backoff missing: result=%+v next=%s", result, client.next)
	}
}
