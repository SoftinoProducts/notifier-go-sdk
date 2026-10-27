package notifier

import (
	"net/http"
	"time"
)

// Version is the SDK version, reported in the User-Agent header.
const Version = "0.4.2"

// config holds the resolved client configuration while options are applied.
type config struct {
	baseURL string
	// allowInsecureBaseURL permits plaintext http to a non-loopback host. See
	// InsecureBaseURL.
	allowInsecureBaseURL bool
	timeout              time.Duration
	timeoutSet           bool
	userAgent            string
	httpClient           *http.Client
}

// Option configures a Client. Options are applied in order and a nil Option is ignored,
// so an option can be passed unconditionally from a variable that may be nil.
type Option func(*config)

// WithBaseURL points the client at a different API endpoint, for example a staging
// deployment or an in-cluster service address. The default is DefaultBaseURL.
//
//	notifier.WithBaseURL("https://notifier-api.notifier.svc.cluster.local")
//
// An https URL is required unless the host is loopback. The API key travels in the
// X-API-Key header on every request, so a plaintext target would expose it to anything on
// the path; refusing the URL here keeps the SDK from being the component that leaks a
// credential. For a deliberately trusted non-loopback target (an in-cluster Service, a
// sidecar), pair this with InsecureBaseURL and the choice becomes explicit.
func WithBaseURL(baseURL string) Option {
	return func(c *config) { c.baseURL = baseURL }
}

// InsecureBaseURL permits a plaintext http:// base URL on a non-loopback host.
//
// It exists so an operator who understands the network path can opt out deliberately. It is
// never implied: without it, http:// is refused everywhere except loopback.
func InsecureBaseURL() Option {
	return func(c *config) { c.allowInsecureBaseURL = true }
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
