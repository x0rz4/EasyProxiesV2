package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerTimeoutsPreserveStreaming(t *testing.T) {
	manager, err := NewManager(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)

	server := NewServer(Config{Enabled: true, Listen: "127.0.0.1:0"}, manager, nil)
	if server == nil || server.srv == nil {
		t.Fatal("NewServer returned no HTTP server")
	}
	t.Cleanup(func() { server.Shutdown(context.Background()) })

	if got := server.srv.ReadHeaderTimeout; got != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", got)
	}
	if got := server.srv.IdleTimeout; got != 60*time.Second {
		t.Fatalf("IdleTimeout = %v, want 60s", got)
	}
	if server.srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, want disabled", server.srv.ReadTimeout)
	}
	if server.srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want disabled", server.srv.WriteTimeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/debug/stream", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	server.srv.Handler.ServeHTTP(response, request)

	if !response.Flushed {
		t.Fatal("debug stream did not flush its initial event")
	}
	if !strings.Contains(response.Body.String(), ": connected\n\n") {
		t.Fatalf("debug stream body = %q, want initial SSE event", response.Body.String())
	}
}
