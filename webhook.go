package notifier

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	// SignatureHeader carries the HMAC-SHA256 signature of a delivery callback, as
	// "sha256=<lowercase hex>".
	SignatureHeader = "X-Notifier-Signature"

	// TimestampHeader carries the Unix-seconds timestamp that is signed together with the
	// body. Without it a captured callback could be replayed forever; with it the verifier
	// can reject anything older than its tolerance window.
	TimestampHeader = "X-Notifier-Timestamp"

	// signaturePrefix is the algorithm tag the service prefixes to the hex digest.
	signaturePrefix = "sha256="

	// MaxCallbackBytes caps how much of a callback body is buffered for verification.
	// Callback payloads are a few hundred bytes.
	MaxCallbackBytes = 1 << 20

	// DefaultTimestampTolerance is how far a callback's timestamp may be from the
	// receiver's clock. It absorbs ordinary clock skew and nothing more; raise it only
	// when a receiver's clock is known to drift.
	DefaultTimestampTolerance = 5 * time.Minute
)

var (
	// ErrInvalidSignature is the reason a callback is rejected by SignatureVerifier when
	// its signature is missing, malformed, or does not match the request. The middleware
	// writes a 401 and never calls the wrapped handler.
	ErrInvalidSignature = errors.New("notifier: invalid callback signature")

	// ErrMissingTimestamp is returned when the request carries no usable
	// X-Notifier-Timestamp header, or one that is not Unix seconds.
	ErrMissingTimestamp = errors.New("notifier: missing callback timestamp")

	// ErrStaleTimestamp is returned when the signed timestamp is outside the verifier's
	// tolerance window, which is how a replayed callback is refused. A stale request is
	// not necessarily forged — it may be a delayed retry — but it cannot be acted on,
	// because the receiver has no way to tell the two apart.
	ErrStaleTimestamp = errors.New("notifier: stale callback timestamp")
)

// SignatureVerifier authenticates delivery callbacks.
//
// Notifier signs "<timestamp>.<raw body>" with HMAC-SHA256 using the tenant's webhook
// signing secret and sends the digest in X-Notifier-Signature as "sha256=<hex>", with the
// same timestamp in X-Notifier-Timestamp. The digest covers the exact body bytes, so a
// receiver must verify before it decodes or re-serializes the JSON; decoding first and
// re-encoding produces different bytes and a signature that never matches.
//
// A verifier is safe for concurrent use once configured. Call WithTimestampTolerance before
// the first request; it mutates the verifier.
type SignatureVerifier struct {
	secret    []byte
	maxBytes  int64
	tolerance time.Duration
	now       func() time.Time
}

// NewSignatureVerifier builds a verifier for the tenant's webhook signing secret.
//
// The default timestamp tolerance is DefaultTimestampTolerance; tune it with
// WithTimestampTolerance.
//
// It returns ErrInvalidArgument when the secret is empty: accepting an empty secret would
// silently trust every request, so it fails closed instead.
func NewSignatureVerifier(secret string) (*SignatureVerifier, error) {
	if secret == "" {
		return nil, invalidArgument("webhook signing secret is required")
	}
	return &SignatureVerifier{
		secret:    []byte(secret),
		maxBytes:  MaxCallbackBytes,
		tolerance: DefaultTimestampTolerance,
		now:       time.Now,
	}, nil
}

// WithTimestampTolerance sets how far a callback's timestamp may be from now.
//
// A tolerance of 0 disables the window. Do that only in a process that already rejects
// repeats by other means (an idempotency key on the effect, for instance); with the window
// off, a captured callback stays valid forever. Negative values are treated as 0.
func (v *SignatureVerifier) WithTimestampTolerance(d time.Duration) *SignatureVerifier {
	if d < 0 {
		d = 0
	}
	v.tolerance = d
	return v
}

// VerifySignature reports whether signature authenticates body as sent at timestamp, using
// the default tolerance. It is the one-shot form of SignatureVerifier.Verify, useful
// outside net/http (for example in a queue consumer that receives the raw body).
func VerifySignature(secret string, body []byte, timestamp, signature string) error {
	if secret == "" {
		return ErrInvalidSignature
	}
	v := &SignatureVerifier{
		secret:    []byte(secret),
		maxBytes:  MaxCallbackBytes,
		tolerance: DefaultTimestampTolerance,
		now:       time.Now,
	}
	return v.Verify(body, timestamp, signature)
}

// Verify reports whether signature authenticates body, sent at the timestamp in
// X-Notifier-Timestamp.
//
// The timestamp is checked against the tolerance window first, then the digest. That order
// matters: a replayed request carries a valid digest, so only the window can refuse it.
func (v *SignatureVerifier) Verify(body []byte, timestamp, signature string) error {
	if len(v.secret) == 0 {
		return ErrInvalidSignature
	}

	if err := v.checkTimestamp(timestamp); err != nil {
		return err
	}

	digest, ok := trimSignaturePrefix(signature)
	if !ok {
		return ErrInvalidSignature
	}
	provided, err := hex.DecodeString(digest)
	if err != nil {
		return ErrInvalidSignature
	}

	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return ErrInvalidSignature
	}
	return nil
}

// checkTimestamp rejects an unparseable timestamp and one outside the tolerance window.
func (v *SignatureVerifier) checkTimestamp(timestamp string) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %q is not unix seconds", ErrMissingTimestamp, timestamp)
	}
	if v.tolerance == 0 {
		return nil
	}

	// Compare in whole seconds: converting to time.Duration overflows for a timestamp
	// that parses but is absurdly far away (milliseconds or nanoseconds instead of
	// seconds), and an overflowed duration compares as "close".
	now := v.now().Unix()
	var skew int64
	if now >= seconds {
		skew = now - seconds
	} else {
		skew = seconds - now
	}
	if skew < 0 { // subtraction overflowed: treat as infinitely far away
		skew = math.MaxInt64
	}
	if skew > int64(v.tolerance/time.Second) {
		return fmt.Errorf("%w: %s is %ds from the receiver's clock (tolerance %s)",
			ErrStaleTimestamp, timestamp, skew, v.tolerance)
	}
	return nil
}

// Middleware verifies the signature of every request before delegating to next. The body
// is buffered once and then restored, so the wrapped handler reads it normally.
//
// Requests with a missing, malformed, or wrong signature are answered with 401, as are
// requests whose signed timestamp is outside the tolerance window; an unreadable or
// oversized body with 400/413. The wrapped handler is not called in any of those cases.
func (v *SignatureVerifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := v.verifyRequest(r)
		if err != nil {
			status := http.StatusUnauthorized
			switch {
			case errors.Is(err, errBodyMissing), errors.Is(err, errBodyRead):
				status = http.StatusBadRequest
			case errors.Is(err, errBodyTooLarge):
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), status)
			return
		}

		// Restore the payload: the wrapped handler must still be able to decode it.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		next.ServeHTTP(w, r)
	})
}

// Handler is Middleware for a bare handler function.
//
//	verifier, err := notifier.NewSignatureVerifier(secret)
//	http.Handle("/notifier/callback", verifier.Handler(onDelivery))
func (v *SignatureVerifier) Handler(next http.HandlerFunc) http.HandlerFunc {
	guarded := v.Middleware(next)
	return guarded.ServeHTTP
}

// errBodyRead and errBodyTooLarge mark the two failures that are not authentication
// failures, so the middleware can answer 400/413 instead of 401.
var (
	errBodyMissing  = errors.New("missing request body")
	errBodyRead     = errors.New("cannot read request body")
	errBodyTooLarge = errors.New("request body too large")
)

// verifyRequest reads and authenticates the request body, returning it for the handler.
func (v *SignatureVerifier) verifyRequest(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errBodyMissing
	}

	limit := io.Reader(r.Body)
	if v.maxBytes > 0 {
		limit = io.LimitReader(r.Body, v.maxBytes+1)
	}
	body, err := io.ReadAll(limit)
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBodyRead, err)
	}
	if v.maxBytes > 0 && int64(len(body)) > v.maxBytes {
		return nil, errBodyTooLarge
	}

	if err := v.Verify(body, r.Header.Get(TimestampHeader), r.Header.Get(SignatureHeader)); err != nil {
		return nil, err
	}
	return body, nil
}

// trimSignaturePrefix strips the "sha256=" algorithm tag, reporting whether the header
// used the expected (and only supported) scheme.
func trimSignaturePrefix(header string) (string, bool) {
	if len(header) <= len(signaturePrefix) || header[:len(signaturePrefix)] != signaturePrefix {
		return "", false
	}
	return header[len(signaturePrefix):], true
}
