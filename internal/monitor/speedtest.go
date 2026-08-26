package monitor

import (
	"context"
	"time"

	"easy_proxies/internal/nodedetect"
)

// SpeedtestRunner is the compatibility wrapper for the legacy single-node SSE
// endpoint. New callers should use the comprehensive node-check task API.
type SpeedtestRunner struct {
	Options nodedetect.SpeedOptions
}

func (r *SpeedtestRunner) Run(ctx context.Context, dialer DialerFunc, callback func(mbps float64, done bool)) (nodedetect.SpeedResult, error) {
	options := r.Options
	if options.URL == "" {
		options = nodedetect.SpeedOptions{URL: "https://speed.cloudflare.com/__down?bytes=100000000", Duration: 5 * time.Second, RequestTimeout: 8 * time.Second, MaxBytes: 100_000_000, PeakSampleInterval: 100 * time.Millisecond}
	}
	result, err := nodedetect.MeasureSpeed(ctx, nodedetect.DialFunc(dialer), options, func(progress nodedetect.SpeedProgress) {
		callback(float64(progress.AverageBytesPerSecond)*8/1_000_000, false)
	})
	if err != nil {
		return result, err
	}
	callback(float64(result.AverageBytesPerSecond)*8/1_000_000, true)
	return result, nil
}
