package notifier

import (
	"net/http"
	"time"
)

// Version is the SDK version, reported in the User-Agent header.
const Version = "0.1.0"

// config holds the resolved client configuration while options are applied.
type config struct {
	baseURL    string
	timeout    time.Duration
	timeoutSet bool
	userAgent  string
	httpClient *http.Client
}

// Option configures a Client. Options are applied in order and a nil Option is ignored,
// so an option can be passed unconditionally from a variable that may be nil.
type Option func(*config)

// WithBaseURL points the client at a different API endpoint, for example a staging
// deployment or an in-cluster service address. The default is DefaultBaseURL.
//
//	notifier.WithBaseURL("http://notifier-api.notifier.svc.cluster.local")
func WithBaseURL(baseURL string) Option {
	return func(c *config) { c.baseURL = baseURL }
}

// WithTimeout bounds a single HTTP round trip, including reading the response body.
// It is applied on top of the caller's context, whichever expires first. Pass 0 to
// disable the client-side deadline and rely on the context alone.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		c.timeout = d
		c.timeoutSet = true
	}
}

// WithHTTPClient supplies the *http.Client used for transport, for example to add a
// tracing RoundTripper or to control TLS settings. The client is not mutated: when
// WithTimeout is also given, the timeout is applied to a copy.
//
// When no client is supplied the SDK uses a shared transport with connection pooling
// tuned for concurrent calls.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.httpClient = hc }
}

// WithUserAgent overrides the User-Agent header, which defaults to the SDK name,
// version and Go runtime version.
func WithUserAgent(userAgent string) Option {
	return func(c *config) { c.userAgent = userAgent }
}
