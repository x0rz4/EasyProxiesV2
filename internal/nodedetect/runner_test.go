package nodedetect

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func directDial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func TestMeasureLatencyUnifiedDelayWarmsConnection(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if _, err := MeasureLatency(context.Background(), directDial, server.URL, time.Second, false); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
	requests.Store(0)
	if _, err := MeasureLatency(context.Background(), directDial, server.URL, time.Second, true); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests with handshake = %d, want 1", requests.Load())
	}
}

func TestMeasureLatencyRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad", http.StatusBadGateway) }))
	defer server.Close()
	if _, err := MeasureLatency(context.Background(), directDial, server.URL, time.Second, true); err == nil {
		t.Fatal("expected HTTP status error")
	}
}

func TestMeasureLatencyHonoursTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if _, err := MeasureLatency(context.Background(), directDial, server.URL, 20*time.Millisecond, true); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestMeasureSpeedStopsAtByteLimit(t *testing.T) {
	payload := make([]byte, 16*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for index := 0; index < 16; index++ {
			_, _ = w.Write(payload)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer server.Close()
	result, err := MeasureSpeed(context.Background(), directDial, SpeedOptions{URL: server.URL, Duration: 2 * time.Second, RequestTimeout: time.Second, MaxBytes: 64 * 1024, PeakSampleInterval: 50 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesDownloaded != 64*1024 || result.AverageBytesPerSecond <= 0 || result.PeakBytesPerSecond <= 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestMeasureSpeedRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad", http.StatusForbidden) }))
	defer server.Close()
	if _, err := MeasureSpeed(context.Background(), directDial, SpeedOptions{URL: server.URL, Duration: time.Second, RequestTimeout: time.Second, MaxBytes: 100_000, PeakSampleInterval: 50 * time.Millisecond}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected HTTP 403 error, got %v", err)
	}
}

func TestMeasureSpeedRejectsTinyPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "tiny") }))
	defer server.Close()
	result, err := MeasureSpeed(context.Background(), directDial, SpeedOptions{URL: server.URL, Duration: time.Second, RequestTimeout: time.Second, MaxBytes: 100_000, PeakSampleInterval: 50 * time.Millisecond}, nil)
	if err == nil {
		t.Fatal("expected tiny download error")
	}
	if result.BytesDownloaded != int64(len("tiny")) || result.DurationMs < 0 {
		t.Fatalf("partial result was not preserved: %+v", result)
	}
}

func TestMeasureSpeedStopsAtDuration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 16*1024)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer server.Close()
	started := time.Now()
	result, err := MeasureSpeed(context.Background(), directDial, SpeedOptions{URL: server.URL, Duration: 60 * time.Millisecond, RequestTimeout: time.Second, MaxBytes: 100_000_000, PeakSampleInterval: 50 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesDownloaded < minValidDownloadBytes || time.Since(started) > time.Second {
		t.Fatalf("duration limit not respected: result=%+v elapsed=%s", result, time.Since(started))
	}
}
