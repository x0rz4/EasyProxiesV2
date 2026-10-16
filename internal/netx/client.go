// Package netx builds the HTTP clients and transports used by node
// diagnostics and probe targets. Every one of them reaches the network
// through a single node's outbound rather than resolving names locally, so
// they all share one transport shape. What legitimately differs between
// callers — timeouts, TLS verification, connection reuse, redirect policy —
// is passed in explicitly as Options instead of being re-typed at each call
// site, where the differences were previously invisible and inconsistent.
package netx

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DialFunc opens a raw connection through one node's outbound. Callers convert
// their own dialer (a pool member, a boxmgr handle) to this shape.
//
// A nil DialFunc is not a "use the stdlib" signal: every transport built here
// exists to route through a node, and one with no dial would silently become a
// bare transport with none of http.DefaultTransport's HTTP/2, proxy and idle
// pool settings. Host-local lookups that genuinely want those should use
// http.DefaultClient-style construction directly.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Options describes how one consumer's HTTP traffic differs from another's.
// The zero value is a plain client: no timeouts, TLS verified, connections
// reused, the stdlib redirect default.
type Options struct {
	// Timeout bounds a whole request including the body read
	// (http.Client.Timeout). Zero means no client-level deadline.
	Timeout time.Duration
	// DialTimeout bounds each individual connection attempt through the node
	// outbound. Zero leaves the dial to the caller's own context.
	DialTimeout time.Duration
	// TLSHandshakeTimeout and ResponseHeaderTimeout bound the two request
	// phases that a client-level Timeout would only catch as one lump. Zero
	// leaves both to the client Timeout.
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	// InsecureSkipVerify accepts any certificate. Probe targets can present
	// region-specific chains that fail local verification, and a diagnostic
	// must not report a node as broken over that.
	InsecureSkipVerify bool
	// DisableKeepAlives forces a fresh connection per request. Probes are
	// one-shot, and reusing a pooled connection to a stale exit would silently
	// measure the previous node.
	DisableKeepAlives bool
	// MaxIdleConnsPerHost is the idle pool size when keep-alives are on. Zero
	// means the stdlib default (2).
	MaxIdleConnsPerHost int
	// MaxRedirects is how many redirects to follow before returning the last
	// response. Zero means the stdlib default (10).
	MaxRedirects int
}

// NewTransport builds the transport half of a probe client. Callers that issue
// a single RoundTrip rather than a full client use this directly. dial must be
// non-nil; see DialFunc.
func NewTransport(dial DialFunc, options Options) *http.Transport {
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig(options.InsecureSkipVerify),
		TLSHandshakeTimeout:   options.TLSHandshakeTimeout,
		ResponseHeaderTimeout: options.ResponseHeaderTimeout,
		DisableKeepAlives:     options.DisableKeepAlives,
		MaxIdleConnsPerHost:   options.MaxIdleConnsPerHost,
		// Every consumer sets this false. Probe targets are reached over a node
		// outbound whose ALPN behaviour varies by exit, so staying on HTTP/1.1
		// keeps one request shape across every region instead of negotiating h2
		// only where the exit happens to allow it.
		ForceAttemptHTTP2: false,
	}
	if dial == nil {
		// Only reachable from a test or a future caller that ignored DialFunc.
		// Leaving DialContext nil falls back to the stdlib dial rather than
		// panicking on the first request.
		return transport
	}
	if options.DialTimeout > 0 {
		dial = boundedDial(dial, options.DialTimeout)
	}
	transport.DialContext = dial
	return transport
}

// NewClient builds a client that dials through one node's outbound.
func NewClient(dial DialFunc, options Options) *http.Client {
	client := &http.Client{Transport: NewTransport(dial, options), Timeout: options.Timeout}
	if options.MaxRedirects > 0 {
		limit := options.MaxRedirects
		client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= limit {
				return http.ErrUseLastResponse
			}
			return nil
		}
	}
	return client
}

// CloseIdleConnections releases pooled connections held by a client built
// here. Probe clients are short-lived and one-shot; leaving their idle
// connections to the finalizer keeps a reference to the node outbound alive
// past the point the caller is done with it.
func CloseIdleConnections(client *http.Client) {
	if client == nil {
		return
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func boundedDial(dial DialFunc, timeout time.Duration) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return dial(bounded, network, address)
	}
}

func tlsConfig(insecure bool) *tls.Config {
	if !insecure {
		return nil
	}
	// #nosec G402 -- opt-in via Options.InsecureSkipVerify, documented there.
	return &tls.Config{InsecureSkipVerify: true}
}

// PortAvailable reports whether a TCP listener can bind address:port right
// now. It backs config-time port assignment: a port is only "free" if the
// local process can actually take it, which a table of already-assigned ports
// cannot see (another process may hold it). The probe is inherently racy —
// the port can be taken between this check and the real bind — so callers use
// it to pick a candidate, and their own Start retry loop remains the
// authority on conflicts.
func PortAvailable(address string, port uint16) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
