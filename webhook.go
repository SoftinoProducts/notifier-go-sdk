package notifier

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
)

const (
	// SignatureHeader carries the HMAC-SHA256 signature of a delivery callback, as
	// "sha256=<lowercase hex>".
	SignatureHeader = "X-Notifier-Signature"

	// signaturePrefix is the algorithm tag the service prefixes to the hex digest.
	signaturePrefix = "sha256="

	// MaxCallbackBytes caps how much of a callback body is buffered for verification.
	// Callback payloads are a few hundred bytes.
	MaxCallbackBytes = 1 << 20
)

// ErrInvalidSignature is the reason a callback is rejected by SignatureVerifier. The
// middleware writes a 401 and never calls the wrapped handler.
var ErrInvalidSignature = errors.New("notifier: invalid callback signature")

// SignatureVerifier authenticates delivery callbacks: Notifier signs the exact request
// body with HMAC-SHA256 using the tenant's webhook signing secret and sends the digest in
// the X-Notifier-Signature header as "sha256=<hex>".
//
// A verifier is immutable and safe for concurrent use.
type SignatureVerifier struct {
	secret   []byte
	maxBytes int64
}

// NewSignatureVerifier builds a verifier for the tenant's webhook signing secret.
//
// It returns ErrInvalidArgument when the secret is empty: accepting an empty secret would
// silently trust every request, so it fails closed instead.
func NewSignatureVerifier(secret string) (*SignatureVerifier, error) {
	if secret == "" {
		return nil, invalidArgument("webhook signing secret is required")
	}
	return &SignatureVerifier{secret: []byte(secret), maxBytes: MaxCallbackBytes}, nil
}

// VerifySignature reports whether header is a valid signature of body under secret. It is
// the one-shot form of SignatureVerifier.Verify, useful outside net/http (for example in
// a queue consumer that receives the raw body).
func VerifySignature(secret string, body []byte, header string) bool {
	v := &SignatureVerifier{secret: []byte(secret)}
	return v.Verify(body, header)
}

// Verify reports whether header authenticates body. The comparison is constant time.
func (v *SignatureVerifier) Verify(body []byte, header string) bool {
	if len(v.secret) == 0 || header == "" {
		return false
	}
	digest, ok := trimSignaturePrefix(header)
	if !ok {
		return false
	}
	provided, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, v.secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

// Middleware verifies the signature of every request before delegating to next. The body
// is buffered once and then restored, so the wrapped handler reads it normally.
//
// Requests with a missing, malformed or wrong signature are answered with 401; an
// unreadable or oversized body with 400/413. The wrapped handler is not called.
func (v *SignatureVerifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			http.Error(w, "missing request body", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, v.maxBytes+1))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "cannot read request body", http.StatusBadRequest)
			return
		}
		if int64(len(body)) > v.maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Restore the payload: the wrapped handler must still be able to decode it.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		if !v.Verify(body, r.Header.Get(SignatureHeader)) {
			http.Error(w, ErrInvalidSignature.Error(), http.StatusUnauthorized)
			return
		}
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

// trimSignaturePrefix strips the "sha256=" algorithm tag, reporting whether the header
// used the expected (and only supported) scheme.
func trimSignaturePrefix(header string) (string, bool) {
	if len(header) <= len(signaturePrefix) || header[:len(signaturePrefix)] != signaturePrefix {
		return "", false
	}
	return header[len(signaturePrefix):], true
}
