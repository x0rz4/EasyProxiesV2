//go:build !linux && !windows

package httpapi

func processRSS() (uint64, bool) { return 0, false }
