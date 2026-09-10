package notifier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors classify the failures returned by Client methods. They are reported
// through the %w chain, so match them with errors.Is:
//
//	_, err := client.Status(ctx, id)
//	switch {
//	case errors.Is(err, notifier.ErrNotFound):
//		// unknown notification id
//	case errors.Is(err, notifier.ErrRateLimited):
//		// back off and try again later
//	}
var (
	// ErrBadRequest reports a 400/422 response: the request body or its parameters were rejected.
	ErrBadRequest = errors.New("notifier: bad request")
	// ErrUnauthorized reports a 401 response: the API key is missing, revoked or wrong.
	ErrUnauthorized = errors.New("notifier: unauthorized")
	// ErrForbidden reports a 403 response: the credential is valid but not permitted here.
	ErrForbidden = errors.New("notifier: forbidden")
	// ErrNotFound reports a 404 response: no such notification, tenant or resource.
	ErrNotFound = errors.New("notifier: not found")
	// ErrConflict reports a 409 response, most commonly a reused idempotency key.
	ErrConflict = errors.New("notifier: conflict")
	// ErrRateLimited reports a 429 response. See APIError.RetryAfter for the suggested delay.
	ErrRateLimited = errors.New("notifier: rate limited")
	// ErrUnavailable reports a 503 response: the tenant has no channel available to route the message.
	ErrUnavailable = errors.New("notifier: service unavailable")
	// ErrServer reports any 5xx response other than 503.
	ErrServer = errors.New("notifier: server error")
	// ErrInvalidArgument reports an argument the SDK rejected before sending anything.
	// It never wraps an HTTP response: use it to tell a programming error from a
	// service-side rejection.
	ErrInvalidArgument = errors.New("notifier: invalid argument")
)

// invalidArgument builds an argument error that matches ErrInvalidArgument.
func invalidArgument(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidArgument}, args...)...)
}

// APIError is the error returned when the service answers with a non-2xx status.
//
// It carries everything needed to react without re-parsing the response: the HTTP status,
// the service's human-readable message, the correlation id to quote in a support request,
// and the rate-limit hint when the service provides one.
type APIError struct {
	// StatusCode is the HTTP status code returned by the service.
	StatusCode int
	// Message is the error text from the service's {"error": "..."} body, or the raw
	// body when it was not valid JSON. It is never empty.
	Message string
	// RequestID is the value of the X-Request-Id response header, useful when reporting
	// a problem to the service operators. Empty when the header is absent.
	RequestID string
	// RetryAfter is the delay advertised by the Retry-After header on a 429/503 response.
	// Zero when the header was absent or unparseable.
	RetryAfter time.Duration
	// Method and Path identify the request that failed.
	Method string
	Path   string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	var b strings.Builder
	b.Grow(64 + len(e.Message))
	b.WriteString("notifier: ")
	b.WriteString(e.Method)
	b.WriteByte(' ')
	b.WriteString(e.Path)
	b.WriteString(": ")
	b.WriteString(strconv.Itoa(e.StatusCode))
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.RequestID != "" {
		b.WriteString(" (request id ")
		b.WriteString(e.RequestID)
		b.WriteByte(')')
	}
	return b.String()
}

// Is reports whether the error matches one of the sentinel errors. It makes
// errors.Is(err, ErrNotFound) work directly on an *APIError.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest || e.StatusCode == http.StatusUnprocessableEntity
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrUnavailable:
		return e.StatusCode == http.StatusServiceUnavailable
	case ErrServer:
		return e.StatusCode >= 500 && e.StatusCode != http.StatusServiceUnavailable
	default:
		return false
	}
}

// Retryable reports whether the same request could plausibly succeed if it were sent
// again. The SDK never retries on its own; this is a hint for the caller's own policy.
//
// Retrying a send without an idempotency key risks delivering the message twice, so a
// retryable status is not by itself a reason to resend.
func (e *APIError) Retryable() bool {
	switch {
	case e.StatusCode == http.StatusTooManyRequests:
		return true
	case e.StatusCode == http.StatusServiceUnavailable:
		return true
	case e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// newAPIError builds an *APIError from a failed response.
func newAPIError(resp *http.Response, method, path string, body []byte) *APIError {
	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    errorMessage(body),
		RequestID:  resp.Header.Get("X-Request-Id"),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Method:     method,
		Path:       path,
	}
}

// errorMessage extracts the message from the service's {"error": "..."} envelope, falling
// back to the trimmed raw body when the response was not the expected envelope.
func errorMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "empty error response"
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil && envelope.Error != "" {
		return envelope.Error
	}
	return truncate(string(trimmed), 512)
}

// truncate shortens s to at most max bytes, marking that it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// parseRetryAfter understands both forms allowed by RFC 9110: delta-seconds and HTTP-date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// wrapTransportError gives network failures a stable prefix while preserving the
// underlying error for errors.As with *url.Error or net.Error.
func wrapTransportError(method, path string, err error) error {
	return fmt.Errorf("notifier: %s %s: %w", method, path, err)
}
