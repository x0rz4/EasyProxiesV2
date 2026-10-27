package boxmgr_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	json "easy_proxies/internal/jsonx"
)

// The optional loopback proxy keeps production assets, API and CSP intact.
// Only this fixture serves instrumentation and replaces the native confirm
// widget's return value; application navigation/dirty-state code is unchanged.
func frontendControlledProxy(t *testing.T, target string) *httptest.Server {
	t.Helper()
	upstream, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts, active, cancelled := map[string]int{}, map[string]int{}, map[string]int{}
	fail, hold := "", ""
	burst := 0
	release := make(chan struct{})
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if r.Context().Err() == nil {
			http.Error(w, "fixture upstream unavailable", 502)
		}
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Header.Get("Content-Type") == "text/html; charset=utf-8" && response.StatusCode == 200 {
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				return err
			}
			body = bytes.Replace(body, []byte("<head>"), []byte(`<head><script src="/__fixture/controls.js"></script>`), 1)
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
			response.Header.Set("Content-Length", fmt.Sprint(len(body)))
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__fixture/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		switch r.URL.Path {
		case "/__fixture/controls.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, frontendControlsScript)
			return
		case "/__fixture/stats":
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"requests": counts, "active": active, "cancelled": cancelled})
			return
		case "/__fixture/config":
			var config struct {
				Fail, Hold string
				Burst      int
			}
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&config) != nil {
				http.Error(w, "fixture config required", 400)
				return
			}
			mu.Lock()
			close(release)
			release = make(chan struct{})
			fail, hold, burst = config.Fail, config.Hold, config.Burst
			mu.Unlock()
			w.WriteHeader(204)
			return
		}
		key := r.Method + " " + r.URL.Path
		if r.URL.Query().Has("node_ids") {
			key += "?node_ids_count=" + fmt.Sprint(len(strings.Split(r.URL.Query().Get("node_ids"), ",")))
		}
		mu.Lock()
		counts[key]++
		active[key]++
		failure, held, n, ready := fail == r.URL.Path, hold == r.URL.Path, burst, release
		mu.Unlock()
		defer func() {
			mu.Lock()
			active[key]--
			if r.Context().Err() != nil {
				cancelled[key]++
			}
			mu.Unlock()
		}()
		if held {
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		if failure {
			http.Error(w, `{"error":"synthetic fixture failure"}`, 503)
			return
		}
		if r.URL.Path == "/api/debug/stream" && n > 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			for i := 0; i < n; i++ {
				if _, err := fmt.Fprintf(w, "data: {\"node_tag\":\"burst-%d\",\"node_name\":\"synthetic burst\",\"event\":{\"time\":%q,\"success\":true,\"latency_ms\":1,\"destination\":\"loopback-fixture\"}}\n\n", i, stamp); err != nil {
					return
				}
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { mu.Lock(); close(release); mu.Unlock(); server.CloseClientConnections(); server.Close() })
	return server
}

const frontendControlsScript = `
addEventListener('DOMContentLoaded', () => {
  const bar = document.createElement('aside'); bar.setAttribute('aria-label', '隔离测试控制');
  Object.assign(bar.style, {position:'fixed',bottom:'0',right:'0',zIndex:'9999',background:'#fff',color:'#111',border:'1px solid #888',fontSize:'12px',padding:'4px',maxWidth:'95vw'});
  bar.innerHTML = '<button id="fixture-reject">夹具：取消确认</button> <button id="fixture-accept">夹具：接受确认</button> <button id="fixture-focus">夹具：模拟回到页面</button> <button id="fixture-reset">夹具：重置计数</button><output id="fixture-state">ready</output>';
  document.body.append(bar);
  let accept = false, confirms = 0, commits = 0;
  const output = () => document.querySelector('#fixture-state').textContent = JSON.stringify({accept,confirms,logCommits:commits});
  document.querySelector('#fixture-reject').onclick = () => {accept=false;output()};
  document.querySelector('#fixture-accept').onclick = () => {accept=true;output()};
  document.querySelector('#fixture-focus').onclick = () => document.dispatchEvent(new Event('visibilitychange'));
  document.querySelector('#fixture-reset').onclick = () => {confirms=0;commits=0;output()};
  window.confirm = () => {confirms++;output();return accept};
  new MutationObserver(records => {
    if(records.some(record => record.target instanceof HTMLElement && record.target.classList.contains('overflow-auto') && record.target.classList.contains('font-mono'))) {commits++;output()}
  }).observe(document.querySelector('#root'), {childList:true,subtree:true});
  output();
});`
