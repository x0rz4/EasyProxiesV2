package monitor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// SpeedtestRunner runs a quick download speed test and streams results to a callback.
type SpeedtestRunner struct {
	dialer func(ctx context.Context, network, addr string) (interface{}, error)
}

// Run executes the speedtest. The callback is called periodically with the current Mbps.
func (r *SpeedtestRunner) Run(ctx context.Context, dialer func(ctx context.Context, network, addr string) (interface{}, error), callback func(mbps float64, isDone bool)) error {
	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer(dialCtx, network, addr)
			if err != nil {
				return nil, err
			}
			return conn.(net.Conn), nil
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second, // Max 15s for the test
	}

	// 25MB payload
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://speed.cloudflare.com/__down?bytes=25000000", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("speedtest failed to connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("speedtest returned HTTP %d", resp.StatusCode)
	}

	buf := make([]byte, 32*1024)
	var totalBytes int64
	lastReport := time.Now()

	for {
		n, err := resp.Body.Read(buf)
		totalBytes += int64(n)

		now := time.Now()
		if now.Sub(lastReport) >= 500*time.Millisecond {
			elapsed := now.Sub(start).Seconds()
			if elapsed > 0 {
				mbps := (float64(totalBytes) * 8) / (1000 * 1000) / elapsed
				callback(mbps, false)
			}
			lastReport = now
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("speedtest read error: %w", err)
		}
	}

	elapsed := time.Since(start).Seconds()
	var finalMbps float64
	if elapsed > 0 {
		finalMbps = (float64(totalBytes) * 8) / (1000 * 1000) / elapsed
	}
	callback(finalMbps, true)

	return nil
}
