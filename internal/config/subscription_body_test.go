package config

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type subscriptionTestReader struct{ remaining, read int }

func (r *subscriptionTestReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = '#'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestSubscriptionBodyLimit(t *testing.T) {
	for _, size := range []int{0, MaxSubscriptionBodyBytes, MaxSubscriptionBodyBytes + 1, MaxSubscriptionBodyBytes * 2} {
		r := &subscriptionTestReader{remaining: size}
		body, err := ReadSubscriptionBody(r)
		if size > MaxSubscriptionBodyBytes {
			if err == nil || !strings.Contains(err.Error(), "exceeds 10MB") {
				t.Fatalf("oversized response size=%d err=%v", size, err)
			}
		} else if err != nil || len(body) != size {
			t.Fatalf("size=%d len=%d err=%v", size, len(body), err)
		}
		if r.read > MaxSubscriptionBodyBytes+1 {
			t.Fatalf("read %d bytes past budget", r.read)
		}
	}
}

func TestLegacySubscriptionRejectsOversizedChunkedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = io.Copy(w, &subscriptionTestReader{remaining: MaxSubscriptionBodyBytes + 1})
	}))
	defer server.Close()
	if _, err := loadNodesFromSubscription(server.URL, 5*time.Second); err == nil || !strings.Contains(err.Error(), "exceeds 10MB") {
		t.Fatalf("legacy oversized response=%v", err)
	}
}

func TestLegacySubscriptionAcceptsNormalResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "http://example.com:80#normal")
	}))
	defer server.Close()
	nodes, err := loadNodesFromSubscription(server.URL, time.Second)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("normal subscription nodes=%v err=%v", nodes, err)
	}
}
