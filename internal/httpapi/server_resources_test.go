package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"easy_proxies/internal/controlplane"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
)

func TestProfileAccessAndParameters(t *testing.T) {
	s := contractServer(t)
	_, token := newContractSession(t, s)
	s.profileEnabled = true
	for _, tt := range []struct {
		name, address, query, kind       string
		enabled, password, authenticated bool
		want                             int
	}{
		{"disabled", "127.0.0.1:7", "", "heap", false, true, true, 404},
		{"no_session", "127.0.0.1:7", "", "heap", true, true, false, 401},
		{"no_password", "127.0.0.1:7", "", "heap", true, false, true, 403},
		{"remote", "192.0.2.1:7", "", "heap", true, true, true, 403},
		{"invalid_peer", "bad", "", "heap", true, true, true, 403},
		{"unknown", "127.0.0.1:7", "", "trace", true, true, true, 404},
		{"duration", "127.0.0.1:7", "?seconds=31", "cpu", true, true, true, 400},
		{"negative", "127.0.0.1:7", "?seconds=-1", "cpu", true, true, true, 400},
		{"duplicate", "127.0.0.1:7", "?seconds=1&seconds=2", "cpu", true, true, true, 400},
		{"gc", "127.0.0.1:7", "?gc=1", "heap", true, true, true, 400},
		{"debug", "127.0.0.1:7", "?debug=2", "goroutine", true, true, true, 400},
		{"delta", "127.0.0.1:7", "?seconds=1", "allocs", true, true, true, 400},
		{"heap", "127.0.0.1:7", "", "heap", true, true, true, 200},
		{"allocs", "[::1]:7", "", "allocs", true, true, true, 200},
		{"goroutine", "127.0.0.1:7", "", "goroutine", true, true, true, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.profileEnabled = tt.enabled
			s.cfg.Password = ""
			if tt.password {
				s.cfg.Password = "secret"
			}
			r := httptest.NewRequest("GET", "/api/operations/profile/"+tt.kind+tt.query, nil)
			r.RemoteAddr = tt.address
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			if tt.authenticated {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.want == 200 {
				assertBinaryProfile(t, w.Body.Bytes())
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("profile may be cached")
				}
			}
		})
	}
}

func assertBinaryProfile(t *testing.T, data []byte) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("profile is not gzip: %v", err)
	}
	defer reader.Close()
	payload, err := io.ReadAll(reader)
	if err != nil || len(payload) == 0 {
		t.Fatalf("invalid profile payload: len=%d err=%v", len(payload), err)
	}
}

type profileDeadlineWriter struct {
	*httptest.ResponseRecorder
	onDeadline func()
	deadline   time.Time
}

func (w *profileDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	if w.onDeadline != nil && !deadline.IsZero() {
		w.onDeadline()
	}
	return nil
}

func TestCPUProfileCancellationShutdownAndConcurrency(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		s := contractServer(t)
		s.profileEnabled = true
		s.done = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest("GET", "/api/operations/profile/cpu?seconds=30", nil).WithContext(ctx)
		request.SetPathValue("kind", "cpu")
		request.RemoteAddr = "127.0.0.1:77"
		w := &profileDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), onDeadline: func() {
			// The process-wide slot must stay held until profile output finishes.
			other := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/operations/profile/heap", nil)
			r.SetPathValue("kind", "heap")
			r.RemoteAddr = "127.0.0.1:77"
			s.handleProfile(other, r)
			if other.Code != http.StatusTooManyRequests {
				t.Errorf("concurrent profile status=%d", other.Code)
			}
			if shutdown {
				close(s.done)
			} else {
				cancel()
			}
		}}
		s.handleProfile(w, request)
		cancel()
		assertBinaryProfile(t, w.Body.Bytes())
		if !w.deadline.IsZero() {
			t.Fatal("write deadline was not reset")
		}
		if !processProfileMu.TryLock() {
			t.Fatal("profile slot was not released")
		}
		processProfileMu.Unlock()
	}
}

func TestResourceSnapshotReportsMemoryAndNoCredentials(t *testing.T) {
	s, mgr, _ := newOperationsTestServer(t)
	s.nodeMgr = resourceTestManager{}
	entry := mgr.Register(monitor.NodeInfo{NodeID: 1, Tag: "secret", URI: "socks5://user:secret@127.0.0.1:1080"})
	entry.IncActive()
	entry.AddTraffic(123, 456)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("GET", "/api/operations/resources", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result resourceSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.HeapObjectsBytes == 0 || result.GoMemoryBytes == 0 || result.Goroutines == 0 || result.GOMAXPROCS == 0 {
		t.Fatalf("missing runtime metrics: %+v", result)
	}
	if result.Traffic.ActiveConnections != 1 || result.Traffic.TotalUpload != 123 || result.Traffic.TotalDownload != 456 {
		t.Fatalf("incorrect traffic: %+v", result.Traffic)
	}
	if result.Runtime == nil || result.Runtime.GroupMembers != 7 {
		t.Fatalf("runtime reader was not used: %+v", result.Runtime)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("secret")) || bytes.Contains(w.Body.Bytes(), []byte("socks5://")) {
		t.Fatal("resource metrics exposed node metadata")
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "linux") && (!result.RSSAvailable || result.RSSBytes == 0) {
		t.Fatalf("RSS unavailable: %+v", result)
	}
}

type resourceTestManager struct{ controlplane.NodeManager }

func (resourceTestManager) RuntimeResources() controlplane.RuntimeResources {
	return controlplane.RuntimeResources{GroupMembers: 7}
}

func TestProfileReadableByGoTool(t *testing.T) {
	s := contractServer(t)
	s.profileEnabled = true
	for _, kind := range []string{"heap", "cpu"} {
		request := httptest.NewRequest("GET", "/api/operations/profile/"+kind+"?seconds=1", nil)
		if kind != "cpu" {
			request.URL.RawQuery = ""
		}
		request.SetPathValue("kind", kind)
		request.RemoteAddr = "127.0.0.1:7777"
		w := httptest.NewRecorder()
		s.handleProfile(w, request)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		path := filepath.Join(t.TempDir(), kind+".pprof")
		if err := os.WriteFile(path, w.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		output, err := exec.CommandContext(ctx, "go", "tool", "pprof", "-top", path).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s profile rejected by pprof: %v: %s", kind, err, output)
		}
	}
}

func TestProfileOptInAndHead(t *testing.T) {
	_, mgr, _ := newOperationsTestServer(t)
	for _, setting := range []string{"", "1"} {
		t.Setenv("EASY_PROXIES_PROFILE_ENABLED", setting)
		s := NewServer(monitor.Config{Enabled: true, Password: "secret"}, mgr, nil)
		if s.profileEnabled != (setting == "1") {
			t.Fatal("startup opt-in was not respected")
		}
		s.profileEnabled = true
		w := httptest.NewRecorder()
		r := httptest.NewRequest("HEAD", "/api/operations/profile/cpu", nil)
		s.handleProfile(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Body.Len() != 0 {
			t.Fatalf("HEAD profile response: %d %s", w.Code, w.Body.String())
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		s.Shutdown(ctx)
		cancel()
	}
}
