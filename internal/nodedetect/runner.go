// Package nodedetect implements manually-triggered, routing-neutral node
// diagnostics. It never publishes monitor health events.
package nodedetect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	minValidDownloadBytes   int64 = 10 * 1024
	defaultMaxDownloadBytes int64 = 10_000_000
)

type DialFunc func(context.Context, string, string) (net.Conn, error)

type SpeedOptions struct {
	URL                string
	Duration           time.Duration
	RequestTimeout     time.Duration
	MaxBytes           int64
	PeakSampleInterval time.Duration
}

type SpeedProgress struct {
	BytesDownloaded       int64 `json:"bytes_downloaded"`
	ElapsedMs             int64 `json:"elapsed_ms"`
	AverageBytesPerSecond int64 `json:"average_bytes_per_second"`
}

type SpeedResult struct {
	AverageBytesPerSecond int64 `json:"average_bytes_per_second"`
	PeakBytesPerSecond    int64 `json:"peak_bytes_per_second"`
	BytesDownloaded       int64 `json:"bytes_downloaded"`
	DurationMs            int64 `json:"duration_ms"`
}

func newClient(dial DialFunc, timeout time.Duration) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		DialContext:         dial,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: 1,
	}
	return &http.Client{Transport: transport, Timeout: timeout}, transport
}

func MeasureLatency(ctx context.Context, dial DialFunc, target string, timeout time.Duration, includeHandshake bool) (time.Duration, error) {
	if dial == nil {
		return 0, errors.New("node dialer is unavailable")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client, transport := newClient(dial, timeout)
	defer transport.CloseIdleConnections()
	if !includeHandshake {
		if _, err := timedRequest(ctx, client, target); err != nil {
			return 0, fmt.Errorf("latency warm-up: %s", sanitizedError(err, target))
		}
	}
	latency, err := timedRequest(ctx, client, target)
	if err != nil {
		return 0, fmt.Errorf("latency request: %s", sanitizedError(err, target))
	}
	return latency, nil
}

func timedRequest(ctx context.Context, client *http.Client, target string) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "EasyProxies/NodeCheck")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	latency := time.Since(start)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return 0, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return latency, nil
}

func MeasureSpeed(ctx context.Context, dial DialFunc, options SpeedOptions, progress func(SpeedProgress)) (SpeedResult, error) {
	if dial == nil {
		return SpeedResult{}, errors.New("node dialer is unavailable")
	}
	if options.Duration <= 0 {
		options.Duration = 5 * time.Second
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 8 * time.Second
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultMaxDownloadBytes
	}
	if options.PeakSampleInterval <= 0 {
		options.PeakSampleInterval = 100 * time.Millisecond
	}
	client, transport := newClient(dial, options.RequestTimeout+options.Duration)
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, options.URL, nil)
	if err != nil {
		return SpeedResult{}, err
	}
	req.Header.Set("User-Agent", "EasyProxies/NodeCheck")
	resp, err := client.Do(req)
	if err != nil {
		return SpeedResult{}, fmt.Errorf("speed test connect: %s", sanitizedError(err, options.URL))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return SpeedResult{}, fmt.Errorf("speed test returned HTTP %d", resp.StatusCode)
	}

	start := time.Now()
	deadline := start.Add(options.Duration)
	var deadlineReached atomic.Bool
	deadlineTimer := time.AfterFunc(options.Duration, func() {
		deadlineReached.Store(true)
		_ = resp.Body.Close()
	})
	defer deadlineTimer.Stop()
	buf := make([]byte, 64*1024)
	var total, sampleBytes, peak int64
	sampleStart := start
	lastProgress := start
	stopReason := ""
	for total < options.MaxBytes {
		if err := ctx.Err(); err != nil {
			return makeSpeedResult(total, start, peak, sampleBytes, sampleStart), err
		}
		now := time.Now()
		if !now.Before(deadline) {
			stopReason = "duration"
			break
		}
		remaining := options.MaxBytes - total
		readBuf := buf
		if remaining < int64(len(readBuf)) {
			readBuf = readBuf[:remaining]
		}
		n, readErr := resp.Body.Read(readBuf)
		now = time.Now()
		total += int64(n)
		sampleBytes += int64(n)
		if elapsed := now.Sub(sampleStart); elapsed >= options.PeakSampleInterval {
			current := int64(float64(sampleBytes) / elapsed.Seconds())
			if current > peak {
				peak = current
			}
			sampleBytes = 0
			sampleStart = now
		}
		if progress != nil && now.Sub(lastProgress) >= 500*time.Millisecond {
			elapsed := now.Sub(start)
			progress(SpeedProgress{BytesDownloaded: total, ElapsedMs: elapsed.Milliseconds(), AverageBytesPerSecond: rate(total, elapsed)})
			lastProgress = now
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				stopReason = "eof"
				break
			}
			if deadlineReached.Load() || !time.Now().Before(deadline) || isTimeout(readErr) {
				stopReason = "duration"
				break
			}
			return makeSpeedResult(total, start, peak, sampleBytes, sampleStart), fmt.Errorf("speed test read: %w", readErr)
		}
	}
	if total >= options.MaxBytes {
		stopReason = "limit"
	}
	result := makeSpeedResult(total, start, peak, sampleBytes, sampleStart)
	if total < minValidDownloadBytes {
		return result, fmt.Errorf("download too small: %d bytes", total)
	}
	_ = stopReason
	if progress != nil {
		progress(SpeedProgress{BytesDownloaded: total, ElapsedMs: result.DurationMs, AverageBytesPerSecond: result.AverageBytesPerSecond})
	}
	return result, nil
}

func makeSpeedResult(total int64, start time.Time, peak, sampleBytes int64, sampleStart time.Time) SpeedResult {
	duration := time.Since(start)
	if sampleBytes > 0 {
		if sampleDuration := time.Since(sampleStart); sampleDuration > 0 {
			current := rate(sampleBytes, sampleDuration)
			if current > peak {
				peak = current
			}
		}
	}
	average := rate(total, duration)
	if peak == 0 {
		peak = average
	}
	return SpeedResult{AverageBytesPerSecond: average, PeakBytesPerSecond: peak, BytesDownloaded: total, DurationMs: duration.Milliseconds()}
}

func DiscoverExitIP(ctx context.Context, dial DialFunc, target string, timeout time.Duration) (string, error) {
	client, transport := newClient(dial, timeout)
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New(sanitizedError(err, target))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("exit IP endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", errors.New("exit IP endpoint returned an invalid IP")
	}
	return ip, nil
}

func rate(bytes int64, duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64(float64(bytes) / duration.Seconds())
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func sanitizedError(err error, target string) string {
	if err == nil {
		return ""
	}
	safe := target
	userInfo := ""
	if parsed, parseErr := url.Parse(target); parseErr == nil {
		if parsed.User != nil {
			userInfo = parsed.User.String() + "@"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		safe = parsed.String()
	}
	message := strings.ReplaceAll(err.Error(), target, safe)
	if userInfo != "" {
		message = strings.ReplaceAll(message, userInfo, "")
	}
	return message
}
