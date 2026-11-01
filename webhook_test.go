package notifier

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testWebhookSecret = "whsec_test_secret"

// goldenBody, goldenTimestamp and goldenSignature are a fixed vector for the callback
// contract: HMAC-SHA256(secret, "<timestamp>.<raw body>"). They are duplicated in
// notifier-api's webhook tests and in docs/callback-signature.md, so a change to the signed
// material breaks all of them. Any implementation — SDK or a tenant's own receiver — can
// verify against this vector without talking to the service.
const (
	goldenTimestamp = "1730000000"
	goldenBody      = `{"notification_id":"801e8e60-4686-4cba-8068-e36c88618b16","status":"delivered","channel_type":"telegram"}`
	goldenSignature = "79c471e9051eb5cb40beba0463903244d673cd74a0361ab9114daece83c38114"

	// bodyOnlySignature is what a receiver gets when it signs the body alone. It is the
	// scheme this SDK shipped before the timestamp was bound in, and every callback it
	// verified was rejected by the service. It must never verify again.
	bodyOnlySignature = "6e3cc01bb604f7cd43b6a4b6ec86456ec0853155581b3f70a03e2d9ccc6c3ba6"
)

// signCallback reproduces what the service does: HMAC-SHA256 over "<timestamp>.<body>",
// hex encoded, tagged with the algorithm.
func signCallback(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// signBodyOnly is the old, broken scheme: it is here to be refused.
func signBodyOnly(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// verifierAt returns a verifier whose clock is pinned, so timestamp tests are not
// timing-dependent.
func verifierAt(t *testing.T, seconds int64) *SignatureVerifier {
	t.Helper()
	v, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}
	v.now = func() time.Time { return time.Unix(seconds, 0) }
	return v
}

func TestSignatureVerifier_GoldenVector(t *testing.T) {
	v := verifierAt(t, 1730000000)
	if err := v.Verify([]byte(goldenBody), goldenTimestamp, signaturePrefix+goldenSignature); err != nil {
		t.Fatalf("the published golden vector did not verify: %v", err)
	}
}

// TestSignatureVerifier_RejectsBodyOnlySignature is the regression test for the drift that
// broke a live tenant: the service bound the timestamp into the signed material, the SDK
// did not, so every callback was answered with a 401.
func TestSignatureVerifier_RejectsBodyOnlySignature(t *testing.T) {
	v := verifierAt(t, 1730000000)
	err := v.Verify([]byte(goldenBody), goldenTimestamp, signaturePrefix+bodyOnlySignature)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifySignature_AcceptsUppercaseHex(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)
	digest := strings.TrimPrefix(signCallback(testWebhookSecret, ts, body), signaturePrefix)
	if err := v.Verify(body, ts, signaturePrefix+strings.ToUpper(digest)); err != nil {
		t.Fatalf("uppercase hex should decode to the same digest: %v", err)
	}
}

func TestVerifySignature_AcceptsEmptyBody(t *testing.T) {
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)
	if err := v.Verify(nil, ts, signCallback(testWebhookSecret, ts, nil)); err != nil {
		t.Fatalf("the signature of an empty body should verify: %v", err)
	}
}

func TestVerifySignature_Rejects(t *testing.T) {
	const ts = "1730000000"
	body := []byte(`{"id":"9f1a2b3c","status":"delivered"}`)

	tests := map[string]struct {
		secret    string
		body      []byte
		timestamp string
		header    string
	}{
		"wrong secret":       {"other-secret", body, ts, signCallback(testWebhookSecret, ts, body)},
		"tampered body":      {testWebhookSecret, []byte(`{"id":"9f1a2b3c","status":"failed"}`), ts, signCallback(testWebhookSecret, ts, body)},
		"body-only scheme":   {testWebhookSecret, body, ts, signBodyOnly(testWebhookSecret, body)},
		"signed other time":  {testWebhookSecret, body, "1730000001", signCallback(testWebhookSecret, ts, body)},
		"truncated digest":   {testWebhookSecret, body, ts, signCallback(testWebhookSecret, ts, body)[:20]},
		"missing signature":  {testWebhookSecret, body, ts, ""},
		"prefix only":        {testWebhookSecret, body, ts, signaturePrefix},
		"bare hex":           {testWebhookSecret, body, ts, strings.TrimPrefix(signCallback(testWebhookSecret, ts, body), signaturePrefix)},
		"wrong algorithm":    {testWebhookSecret, body, ts, "md5=" + strings.TrimPrefix(signCallback(testWebhookSecret, ts, body), signaturePrefix)},
		"not hex":            {testWebhookSecret, body, ts, signaturePrefix + "zzzz"},
		"empty secret":       {"", body, ts, signCallback("", ts, body)},
		"secret is a prefix": {testWebhookSecret[:5], body, ts, signCallback(testWebhookSecret, ts, body)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			v, err := NewSignatureVerifier(tc.secret)
			if err != nil {
				// An empty secret fails closed at construction time.
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("NewSignatureVerifier: %v", err)
				}
				return
			}
			v.now = func() time.Time { return time.Unix(1730000000, 0) }
			if err := v.Verify(tc.body, tc.timestamp, tc.header); !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("error = %v, want ErrInvalidSignature", err)
			}
		})
	}
}

func TestVerifySignature_RejectsMissingTimestamp(t *testing.T) {
	body := []byte(`{"id":"1"}`)

	for name, value := range map[string]string{
		"absent":       "",
		"not a number": "yesterday",
		"padded":       " 1730000000",
		"nanoseconds":  "1730000000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			v := verifierAt(t, 1730000000)
			err := v.Verify(body, value, signCallback(testWebhookSecret, value, body))
			if !errors.Is(err, ErrMissingTimestamp) && !errors.Is(err, ErrStaleTimestamp) {
				t.Fatalf("error = %v, want ErrMissingTimestamp or ErrStaleTimestamp", err)
			}
		})
	}
}

// TestSignatureVerifier_MillisecondTimestampIsStale covers a receiver that was written
// against a timestamp in milliseconds: it parses, so it must fail the window rather than
// the parse.
func TestSignatureVerifier_MillisecondTimestampIsStale(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000000"

	v := verifierAt(t, 1730000000)
	err := v.Verify(body, ts, signCallback(testWebhookSecret, ts, body))
	if !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("error = %v, want ErrStaleTimestamp", err)
	}
}

func TestSignatureVerifier_RejectsStaleAndFutureTimestamps(t *testing.T) {
	body := []byte(`{"id":"1"}`)

	tests := map[string]struct {
		signedAt int64
		now      int64
	}{
		"replayed hours later": {signedAt: 1730000000, now: 1730000000 + 3600},
		"just over tolerance":  {signedAt: 1730000000, now: 1730000000 + int64(DefaultTimestampTolerance/time.Second) + 1},
		"from the future":      {signedAt: 1730000600, now: 1730000000},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ts := strconv.FormatInt(tc.signedAt, 10)
			v := verifierAt(t, tc.now)
			err := v.Verify(body, ts, signCallback(testWebhookSecret, ts, body))
			if !errors.Is(err, ErrStaleTimestamp) {
				t.Fatalf("error = %v, want ErrStaleTimestamp", err)
			}
		})
	}
}

func TestSignatureVerifier_AcceptsTimestampAtTheEdge(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	tolerance := int64(DefaultTimestampTolerance / time.Second)

	for name, now := range map[string]int64{
		"exactly at the past edge":   1730000000 + tolerance,
		"exactly at the future edge": 1730000000 - tolerance,
	} {
		t.Run(name, func(t *testing.T) {
			v := verifierAt(t, now)
			if err := v.Verify(body, "1730000000", signCallback(testWebhookSecret, "1730000000", body)); err != nil {
				t.Fatalf("a timestamp exactly at the tolerance edge was rejected: %v", err)
			}
		})
	}
}

func TestSignatureVerifier_ToleranceZeroDisablesTheWindow(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	ts := "1730000000"

	v, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}
	v.WithTimestampTolerance(0)
	v.now = func() time.Time { return time.Unix(1730000000+86400, 0) }

	if err := v.Verify(body, ts, signCallback(testWebhookSecret, ts, body)); err != nil {
		t.Fatalf("tolerance 0 must skip the window: %v", err)
	}
}

func TestNewSignatureVerifier_RejectsEmptySecret(t *testing.T) {
	if _, err := NewSignatureVerifier(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if _, err := NewSignatureVerifier(testWebhookSecret); err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}
}

func TestSignatureVerifier_MiddlewareForwardsTheBody(t *testing.T) {
	body := []byte(`{"id":"9f1a2b3c","status":"delivered","recipient":"+989120000000"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)

	var (
		called   bool
		gotBody  []byte
		gotLen   int64
		gotClose bool
	)
	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotBody, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
		gotClose = r.Body != nil
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, ts, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("the wrapped handler was not called for a valid signature")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if string(gotBody) != string(body) {
		t.Errorf("handler read %q, want %q", gotBody, body)
	}
	if gotLen != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", gotLen, len(body))
	}
	if !gotClose {
		t.Error("the body must still be readable downstream")
	}
}

func TestSignatureVerifier_MiddlewareRejectsInvalidSignature(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)

	called := false
	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signCallback("wrong-secret", ts, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("the wrapped handler ran for an invalid signature")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid callback signature") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestSignatureVerifier_MiddlewareRejectsBodyOnlyCallback(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)

	called := false
	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signBodyOnly(testWebhookSecret, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("a body-only signature was accepted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSignatureVerifier_MiddlewareRejectsStaleCallback(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000+3600)

	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the wrapped handler must not run for a stale callback")
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, ts, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "stale callback timestamp") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestSignatureVerifier_MiddlewareRejectsMissingTimestamp(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	v := verifierAt(t, 1730000000)

	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the wrapped handler must not run without a timestamp")
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, "1730000000", body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSignatureVerifier_MiddlewareRejectsOversizedBody(t *testing.T) {
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)

	called := false
	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	big := strings.Repeat("x", int(v.maxBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(big))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, ts, []byte(big)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("the wrapped handler ran for an oversized body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestSignatureVerifier_MiddlewareRejectsMissingBody(t *testing.T) {
	v := verifierAt(t, 1730000000)

	handler := v.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the wrapped handler must not run")
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", nil)
	req.Body = nil
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSignatureVerifier_HandlerFuncForm(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	const ts = "1730000000"
	v := verifierAt(t, 1730000000)

	handler := v.Handler(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodPost, "/cb", strings.NewReader(string(body)))
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, ts, body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body = %q, want the original payload", rec.Body.String())
	}
}
