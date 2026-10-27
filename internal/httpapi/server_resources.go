package httpapi

import (
	"net"
	"net/http"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"sync"
	"time"

	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/monitor"
)

// Profiling is process-wide and can allocate substantial temporary state.
// Bound all profile kinds together, including across Server replacements.
var processProfileMu sync.Mutex

type resourceSnapshot struct {
	SampledAt        time.Time                      `json:"sampled_at"`
	Goroutines       int                            `json:"goroutines"`
	GOMAXPROCS       int                            `json:"gomaxprocs"`
	HeapObjectsBytes uint64                         `json:"heap_objects_bytes"`
	GoMemoryBytes    uint64                         `json:"go_memory_bytes"`
	AllocatedBytes   uint64                         `json:"allocated_bytes"`
	GCCycles         uint64                         `json:"gc_cycles"`
	GoUserCPUSeconds float64                        `json:"go_user_cpu_seconds"`
	GoGCCPUSeconds   float64                        `json:"go_gc_cpu_seconds"`
	RSSBytes         uint64                         `json:"rss_bytes"`
	RSSAvailable     bool                           `json:"rss_available"`
	Traffic          monitor.TrafficSummary         `json:"traffic"`
	Probe            monitor.ProbeRoundStatus       `json:"probe"`
	Runtime          *controlplane.RuntimeResources `json:"runtime,omitempty"`
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/user:cpu-seconds"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
	}
	metrics.Read(samples)
	result := resourceSnapshot{
		SampledAt: time.Now().UTC(), Goroutines: runtime.NumGoroutine(), GOMAXPROCS: runtime.GOMAXPROCS(0),
		HeapObjectsBytes: samples[0].Value.Uint64(),
		GoMemoryBytes:    samples[1].Value.Uint64() - samples[2].Value.Uint64(),
		AllocatedBytes:   samples[3].Value.Uint64(), GCCycles: samples[4].Value.Uint64(),
		GoUserCPUSeconds: samples[5].Value.Float64(), GoGCCPUSeconds: samples[6].Value.Float64(),
	}
	result.RSSBytes, result.RSSAvailable = processRSS()
	if s.mgr != nil {
		result.Traffic = s.mgr.TrafficSummary(false)
		result.Probe = s.mgr.ProbeRoundStatus()
	}
	if reader, ok := s.nodeMgr.(controlplane.RuntimeResourceReader); ok {
		resources := reader.RuntimeResources()
		result.Runtime = &resources
	}
	writeJSON(w, result)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if !s.profileEnabled {
		http.NotFound(w, r)
		return
	}
	// ServeMux also matches HEAD to a GET pattern. A header-only probe must
	// never start an expensive CPU/heap capture.
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || !s.authService().PasswordRequired() {
		writeAPIError(w, http.StatusForbidden, "性能采样要求本机连接且已设置管理密码")
		return
	}
	kind := r.PathValue("kind")
	if kind != "cpu" && kind != "heap" && kind != "allocs" && kind != "goroutine" {
		http.NotFound(w, r)
		return
	}
	seconds := 10
	for key, values := range r.URL.Query() {
		if key != "seconds" || kind != "cpu" || len(values) != 1 {
			writeAPIError(w, http.StatusBadRequest, "仅 CPU 采样支持 seconds=1..30")
			return
		}
		seconds, err = strconv.Atoi(values[0])
		if err != nil || seconds < 1 || seconds > 30 {
			writeAPIError(w, http.StatusBadRequest, "seconds 必须为 1..30")
			return
		}
	}
	if !processProfileMu.TryLock() {
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "性能采样正在进行")
		return
	}
	defer processProfileMu.Unlock()
	select {
	case <-r.Context().Done():
		return
	case <-s.done:
		writeAPIError(w, http.StatusServiceUnavailable, "服务正在关闭")
		return
	default:
	}
	deadline := time.Now().Add(10 * time.Second)
	if kind == "cpu" {
		deadline = deadline.Add(time.Duration(seconds) * time.Second)
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline)
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+kind+`.pprof"`)
	if kind == "cpu" {
		if err := pprof.StartCPUProfile(w); err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "CPU 采样不可用")
			return
		}
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
		case <-s.done:
		}
		pprof.StopCPUProfile()
		return
	}
	if err := pprof.Lookup(kind).WriteTo(w, 0); err != nil && s.logger != nil {
		s.logger.Printf("[profile] %s write failed: %v", kind, err)
	}
}
