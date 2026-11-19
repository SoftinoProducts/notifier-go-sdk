package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the production Notifier API endpoint.
	DefaultBaseURL = "https://notifier-api.vibe.ir"

	// DefaultTimeout bounds one HTTP round trip, including reading the response body.
	// It is a safety net so a stuck call cannot block a goroutine forever; it is not a
	// delivery guarantee. Pass WithTimeout to change it.
	DefaultTimeout = 30 * time.Second
)

// maxResponseBytes caps the buffered response size. Notification history pages and bulk
// acknowledgements are orders of magnitude smaller; the cap exists so a misbehaving proxy
// cannot exhaust the caller's memory. It is a variable only so tests can lower it.
var maxResponseBytes int64 = 16 << 20

// Client is a Notifier API client.
//
// A Client is safe for concurrent use by multiple goroutines: all of its state is
// immutable after construction and it shares connection pools across callers. Create one
// per API key (per tenant) and reuse it for the lifetime of the process — there is no
// Close method because the shared transport is reused across clients.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      *http.Client
}

// NewClient returns a Client authenticated with the tenant's API key.
//
//	client, err := notifier.NewClient(apiKey)
//	client, err := notifier.NewClient(apiKey, notifier.WithBaseURL("https://notifier.staging.internal"))
//
// It returns an error when the key is blank or the base URL is not a usable http(s) URL.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	cfg := config{
		baseURL:   DefaultBaseURL,
		timeout:   DefaultTimeout,
		userAgent: defaultUserAgent(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("notifier: API key is required")
	}
	if cfg.maxConnsPerHost < 0 {
		return nil, errors.New("notifier: max connections per host must not be negative")
	}
	if cfg.maxConnsPerHost > 0 && cfg.httpClient != nil {
		return nil, errors.New("notifier: WithMaxConnsPerHost cannot be combined with WithHTTPClient")
	}
	base, err := normalizeBaseURL(cfg.baseURL, cfg.allowInsecureBaseURL)
	if err != nil {
		return nil, err
	}

	hc := cfg.httpClient
	switch {
	case hc == nil:
		hc = &http.Client{Transport: transportWithMaxConns(cfg.maxConnsPerHost), Timeout: cfg.timeout}
	case cfg.timeoutSet:
		// Never mutate a client the caller may share elsewhere.
		clone := *hc
		clone.Timeout = cfg.timeout
		hc = &clone
	}

	return &Client{
		baseURL:   base,
		apiKey:    apiKey,
		userAgent: cfg.userAgent,
		http:      hc,
	}, nil
}

// BaseURL returns the normalized API base URL the client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// doJSON performs a request against baseURL+path and decodes the JSON response into out.
// A nil out discards the response body, and a nil body sends no request payload.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.doJSONWithHeaders(ctx, method, path, query, nil, body, out)
}

// doJSONWithHeaders is doJSON plus per-request headers (idempotency, priority). The
// headers are applied after the SDK's own headers and cannot replace X-API-Key.
func (c *Client) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values,
	headers http.Header, body, out any) error {
	req, err := c.newRequest(ctx, method, path, query, headers, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapTransportError(method, path, err)
	}
	return c.decodeResponse(method, path, resp, out)
}

// newRequest builds a request with the API key header and an encoded JSON payload.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values,
	headers http.Header, body any) (*http.Request, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload []byte
	if body != nil {
		var err error
		// json.Marshal (rather than an Encoder over a pooled buffer): the bytes backing a
		// request body must stay valid until the transport is finished with them, which
		// can outlive Do on an early response, so they must not come from a shared pool.
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("notifier: encode %s %s: %w", method, path, err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("notifier: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		if strings.EqualFold(name, "X-API-Key") {
			continue // never let a caller-supplied header replace the credential
		}
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return req, nil
}

// decodeResponse reads, classifies and decodes a response. The body is always read to
// completion so the connection can be returned to the keep-alive pool.
func (c *Client) decodeResponse(method, path string, resp *http.Response, out any) error {
	defer resp.Body.Close()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	n, err := buf.ReadFrom(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return wrapTransportError(method, path, err)
	}
	if n >= maxResponseBytes {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("notifier: %s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, method, path, buf.Bytes())
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || n == 0 {
		return nil
	}
	if err := json.Unmarshal(buf.Bytes(), out); err != nil {
		return fmt.Errorf("notifier: %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// bufPool recycles response buffers. Only buffers the client itself filled from a
// response body are pooled; nothing else ever holds a reference to them once the decoder
// has copied the JSON into typed values.
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// defaultTransport returns a process-wide transport derived from http.DefaultTransport:
// proxy-from-environment, HTTP/2 and the stdlib dial timeouts are inherited, while the
// idle-connection limits are raised. The stdlib default of 2 idle connections per host
// serializes concurrent calls to the same API host.
var defaultTransport = sync.OnceValue(func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	return tunedTransport(base, 0)
})

// transportWithMaxConns returns the shared tuned transport when no per-client cap was
// requested, or a private copy with MaxConnsPerHost set. Cloning preserves the stdlib
// dial/TLS defaults and the SDK's idle-pool tuning.
func transportWithMaxConns(maxConns int) http.RoundTripper {
	if maxConns <= 0 {
		return defaultTransport()
	}
	base := defaultTransport()
	tr, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	return tunedTransport(tr, maxConns)
}

func tunedTransport(base *http.Transport, maxConns int) *http.Transport {
	tuned := base.Clone()
	tuned.MaxIdleConns = 200
	tuned.MaxIdleConnsPerHost = 32
	tuned.IdleConnTimeout = 90 * time.Second
	tuned.ForceAttemptHTTP2 = true
	if maxConns > 0 {
		tuned.MaxConnsPerHost = maxConns
	}
	return tuned
}

// defaultUserAgent identifies the SDK and the Go runtime to the service.
func defaultUserAgent() string {
	return "notifier-go-sdk/" + Version + " " + runtime.Version()
}

// normalizeBaseURL validates and canonicalizes a base URL, dropping any trailing slash.
//
// A plaintext http:// URL is REFUSED when a credential is configured, because the API key
// travels in the X-API-Key header on every request: over http it is readable by anything on
// the path, and the SDK would be the component that leaked it. Use InsecureBaseURL to opt
// out for a loopback or test target, which makes the decision explicit at the call site
// rather than a silent property of a URL string.
func normalizeBaseURL(raw string, allowInsecure bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("notifier: base URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("notifier: invalid base URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure && !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("notifier: base URL %q is not https; the API key would be sent in "+
				"cleartext. Use https, or pass notifier.InsecureBaseURL() for a trusted local target", raw)
		}
	default:
		return "", fmt.Errorf("notifier: base URL %q must use the http or https scheme", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("notifier: base URL %q has no host", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// isLoopbackHost reports whether host is a loopback literal or localhost. A local target
// cannot be eavesdropped from the network, so refusing it would only make tests awkward.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
