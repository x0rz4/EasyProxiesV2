package netx

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func stubDial(ctx context.Context, network, address string) (net.Conn, error) { return nil, nil }

// Options are the whole contract of this package: each field must land on the
// transport the caller asked for, and nothing a caller left unset may silently
// acquire a value. These tests pin the mapping so a consumer cannot lose
// connection reuse, timeouts, or TLS verification to a library default.
func TestOptionsMapToTransportFields(t *testing.T) {
	transport := NewTransport(DialFunc(stubDial), Options{
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		InsecureSkipVerify:    true,
		DisableKeepAlives:     true,
		MaxIdleConnsPerHost:   7,
	})

	if transport.DialContext == nil {
		t.Error("DialContext was not installed")
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify did not reach the TLS config")
	}
	if transport.TLSHandshakeTimeout != 3*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 3s", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != 4*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 4s", transport.ResponseHeaderTimeout)
	}
	if !transport.DisableKeepAlives {
		t.Error("DisableKeepAlives was not applied")
	}
	if transport.MaxIdleConnsPerHost != 7 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 7", transport.MaxIdleConnsPerHost)
	}
}

// ForceAttemptHTTP2 is fixed rather than optional: every probe goes over a node
// exit whose ALPN support varies, so one HTTP/1.1 shape keeps regions
// comparable. A caller must not be able to turn it on by accident.
func TestHTTP2IsAlwaysDisabled(t *testing.T) {
	for _, options := range []Options{{}, {Timeout: time.Second, InsecureSkipVerify: true}} {
		if NewTransport(DialFunc(stubDial), options).ForceAttemptHTTP2 {
			t.Fatal("ForceAttemptHTTP2 must stay disabled for probe transports")
		}
	}
}

// A zero Options value must not invent timeouts or verification settings.
func TestZeroOptionsLeavesDefaultsUntouched(t *testing.T) {
	transport := NewTransport(DialFunc(stubDial), Options{})
	if transport.TLSClientConfig != nil {
		t.Error("zero Options must not install a TLS config")
	}
	if transport.TLSHandshakeTimeout != 0 || transport.ResponseHeaderTimeout != 0 {
		t.Error("zero Options must not invent phase timeouts")
	}
	if transport.DisableKeepAlives {
		t.Error("zero Options must leave keep-alives enabled")
	}
}

// DialTimeout bounds each attempt through the node outbound without leaking a
// deadline when the caller passed none.
func TestDialTimeoutIsAppliedToTheDial(t *testing.T) {
	var seen time.Duration
	hasDeadline := false
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if deadline, ok := ctx.Deadline(); ok {
			hasDeadline = true
			seen = time.Until(deadline)
		}
		return nil, nil
	}

	transport := NewTransport(DialFunc(dial), Options{DialTimeout: 2 * time.Second})
	if _, err := transport.DialContext(context.Background(), "tcp", "example.com:80"); err != nil {
		t.Fatal(err)
	}
	if !hasDeadline {
		t.Fatal("DialTimeout did not reach the dial context")
	}
	if seen <= 0 || seen > 2*time.Second {
		t.Fatalf("dial deadline = %v, want within (0, 2s]", seen)
	}

	// With no DialTimeout the caller's context must pass through untouched.
	unbounded := NewTransport(DialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("a deadline was added despite DialTimeout being unset")
		}
		return nil, nil
	}), Options{})
	_, _ = unbounded.DialContext(context.Background(), "tcp", "example.com:80")
}

// MaxRedirects must reproduce the semantics of the hand-written CheckRedirect
// it replaced: follow up to N hops, then hand back the last response.
func TestMaxRedirectsStopsAfterLimit(t *testing.T) {
	client := NewClient(DialFunc(stubDial), Options{MaxRedirects: 5})
	if client.CheckRedirect == nil {
		t.Fatal("MaxRedirects did not install a redirect policy")
	}

	via := make([]*http.Request, 0, 6)
	for hop := 0; hop < 5; hop++ {
		if err := client.CheckRedirect(&http.Request{}, via); err != nil {
			t.Fatalf("hop %d should still follow: %v", hop, err)
		}
		via = append(via, &http.Request{})
	}
	if err := client.CheckRedirect(&http.Request{}, via); err != http.ErrUseLastResponse {
		t.Fatalf("6th hop = %v, want http.ErrUseLastResponse", err)
	}
}

// With no MaxRedirects the policy is absent, so the stdlib default applies.
func TestNoRedirectPolicyWhenUnset(t *testing.T) {
	if NewClient(DialFunc(stubDial), Options{}).CheckRedirect != nil {
		t.Fatal("zero Options must not install a redirect policy")
	}
}

// Clients built here are short-lived; closing their idle connections must be
// safe to call on anything this package or a caller might hold.
func TestCloseIdleConnectionsToleratesForeignClients(t *testing.T) {
	CloseIdleConnections(nil)
	CloseIdleConnections(&http.Client{})
	CloseIdleConnections(&http.Client{Transport: http.DefaultTransport})
	CloseIdleConnections(NewClient(DialFunc(stubDial), Options{MaxIdleConnsPerHost: 1}))
}
