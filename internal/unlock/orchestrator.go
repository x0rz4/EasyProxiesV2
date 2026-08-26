package unlock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"easy_proxies/internal/geoip"
)

const checkerConcurrency = 3

func checkRegistered(ctx context.Context, dialer DialFunc, tag, name string, geoLookup *geoip.Lookup, timeout time.Duration) *Result {
	startedAt := time.Now()
	runtime := newRuntime(ctx, dialer, timeout)
	defer runtime.close()
	result := &Result{Tag: tag, Name: name}
	result.IP = probeExitIP(runtime, geoLookup)
	runtime.LandingCountry = strings.ToUpper(strings.TrimSpace(result.IP.ISOCode))

	checkers := globalCheckerRegistry.list()
	result.Services = make([]ServiceResult, len(checkers))
	workerLimit := checkerConcurrency
	if workerLimit > len(checkers) {
		workerLimit = len(checkers)
	}
	if workerLimit == 0 {
		result.Duration = time.Since(startedAt).Milliseconds()
		return result
	}
	semaphore := make(chan struct{}, workerLimit)
	var waitGroup sync.WaitGroup
	for index, checker := range checkers {
		waitGroup.Add(1)
		go func(index int, checker Checker) {
			defer waitGroup.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				result.Services[index] = failedResult(checker, ctx.Err())
				return
			}
			result.Services[index] = runChecker(runtime, checker)
		}(index, checker)
	}
	waitGroup.Wait()
	result.Duration = time.Since(startedAt).Milliseconds()
	return result
}

func runChecker(runtime Runtime, checker Checker) (result ServiceResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = failedResult(checker, fmt.Errorf("checker panic: %v", recovered))
		}
		result = normalizeServiceResult(checker, result)
	}()
	return checker.Check(runtime)
}

func failedResult(checker Checker, err error) ServiceResult {
	detail := "检测失败"
	if err != nil {
		detail += ": " + err.Error()
	}
	return ServiceResult{Name: checker.Key(), DisplayName: checker.DisplayName(), Status: StatusFailed, Detail: detail}
}

func normalizeServiceResult(checker Checker, result ServiceResult) ServiceResult {
	meta := checkerProviderMeta(checker)
	result.Name = meta.Value
	result.DisplayName = meta.Label
	result.Category = meta.Category
	result.Description = meta.Description
	result.Region = strings.ToUpper(strings.TrimSpace(result.Region))
	switch result.Status {
	case StatusUnlocked, StatusPartial, StatusOriginalsOnly, StatusLocked, StatusFailed:
	default:
		result.Status = StatusFailed
		if result.Detail == "" {
			result.Detail = "检测器返回了未知状态"
		}
	}
	return result
}
