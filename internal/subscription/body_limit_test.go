package subscription

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/store"
)

func TestOversizedSubscriptionKeepsPreviousMembership(t *testing.T) {
	var oversized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !oversized.Load() {
			_, _ = io.WriteString(w, "http://example.com:80#retained")
			return
		}
		w.(http.Flusher).Flush()
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("#", config.MaxSubscriptionBodyBytes+1)))
	}))
	defer server.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "body-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := New(&config.Config{}, nil, WithStore(db), WithHTTPClient(server.Client()))
	defer m.Stop()
	sub, err := m.Create(t.Context(), store.Subscription{Name: "limited", URL: server.URL, Enabled: true, RefreshTimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial membership", func() bool { return subscriptionNodeCount(t, m, sub.ID) == 1 })
	oversized.Store(true)
	if err := refreshOneEventually(t, m, sub.ID); err == nil || !strings.Contains(err.Error(), "exceeds 10MB") {
		t.Fatalf("oversized refresh=%v", err)
	}
	if count := subscriptionNodeCount(t, m, sub.ID); count != 1 {
		t.Fatalf("membership after oversized response=%d", count)
	}
}
