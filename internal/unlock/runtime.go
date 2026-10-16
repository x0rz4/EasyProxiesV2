package unlock

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"easy_proxies/internal/netx"
)

const defaultProbeBodyLimit int64 = 32 * 1024

// Runtime is the common execution environment shared by every checker for one
// node. The HTTP client always dials through that node's outbound.
type Runtime struct {
	Context        context.Context
	Client         *http.Client
	Timeout        time.Duration
	LandingCountry string
}

type probeResponse struct {
	StatusCode int
	FinalURL   string
	Body       string
	RawBody    string
	Header     http.Header
}

func newRuntime(ctx context.Context, dialer DialFunc, timeout time.Duration) Runtime {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	// Detectors probe many unrelated hosts per node, so connections are not
	// reused; each phase gets the same deadline as the whole check, and
	// certificates are not verified because exits present region-specific
	// chains that would otherwise read as a broken node.
	client := netx.NewClient(netx.DialFunc(dialer), netx.Options{
		Timeout:               timeout,
		DialTimeout:           timeout,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		InsecureSkipVerify:    true,
		DisableKeepAlives:     true,
		MaxRedirects:          5,
	})
	return Runtime{Context: ctx, Client: client, Timeout: timeout}
}

func (runtime Runtime) close() {
	netx.CloseIdleConnections(runtime.Client)
}

func fetchProbe(runtime Runtime, target string, headers map[string]string) (*probeResponse, error) {
	return fetchProbeWithLimit(runtime, target, headers, defaultProbeBodyLimit)
}

func fetchProbeWithLimit(runtime Runtime, target string, headers map[string]string, bodyLimit int64) (*probeResponse, error) {
	return fetchRequest(runtime, http.MethodGet, target, headers, nil, bodyLimit)
}

func fetchRequest(runtime Runtime, method, target string, headers map[string]string, body []byte, bodyLimit int64) (*probeResponse, error) {
	if bodyLimit <= 0 {
		bodyLimit = defaultProbeBodyLimit
	}
	ctx := runtime.Context
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, runtime.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for key, value := range headers {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := runtime.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, err
	}
	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	rawBody := string(bodyBytes)
	return &probeResponse{
		StatusCode: resp.StatusCode,
		FinalURL:   finalURL,
		Body:       strings.ToLower(rawBody),
		RawBody:    rawBody,
		Header:     resp.Header.Clone(),
	}, nil
}

func containsAny(text string, values ...string) bool {
	text = strings.ToLower(text)
	for _, value := range values {
		if value != "" && strings.Contains(text, strings.ToLower(strings.TrimSpace(value))) {
			return true
		}
	}
	return false
}
