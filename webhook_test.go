package notifier

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testWebhookSecret = "whsec_test_secret"

// signCallback reproduces what the service does: HMAC-SHA256 over the exact body bytes,
// hex encoded, tagged with the algorithm.
func signCallback(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_AcceptsAValidSignature(t *testing.T) {
	body := []byte(`{"id":"9f1a2b3c","status":"delivered"}`)
	if !VerifySignature(testWebhookSecret, body, signCallback(testWebhookSecret, body)) {
		t.Error("a correctly signed body was rejected")
	}
}

func TestVerifySignature_AcceptsUppercaseHex(t *testing.T) {
	body := []byte(`{"id":"1"}`)
	digest := strings.TrimPrefix(signCallback(testWebhookSecret, body), signaturePrefix)
	header := signaturePrefix + strings.ToUpper(digest)
	if !VerifySignature(testWebhookSecret, body, header) {
		t.Error("uppercase hex should decode to the same digest")
	}
}

func TestVerifySignature_AcceptsEmptyBody(t *testing.T) {
	if !VerifySignature(testWebhookSecret, nil, signCallback(testWebhookSecret, nil)) {
		t.Error("the signature of an empty body should verify")
	}
}

func TestVerifySignature_Rejects(t *testing.T) {
	body := []byte(`{"id":"9f1a2b3c","status":"delivered"}`)

	tests := map[string]struct {
		secret string
		body   []byte
		header string
	}{
		"wrong secret":       {"other-secret", body, signCallback(testWebhookSecret, body)},
		"tampered body":      {testWebhookSecret, []byte(`{"id":"9f1a2b3c","status":"failed"}`), signCallback(testWebhookSecret, body)},
		"truncated digest":   {testWebhookSecret, body, signCallback(testWebhookSecret, body)[:20]},
		"missing header":     {testWebhookSecret, body, ""},
		"prefix only":        {testWebhookSecret, body, signaturePrefix},
		"bare hex":           {testWebhookSecret, body, strings.TrimPrefix(signCallback(testWebhookSecret, body), signaturePrefix)},
		"wrong algorithm":    {testWebhookSecret, body, "md5=" + strings.TrimPrefix(signCallback(testWebhookSecret, body), signaturePrefix)},
		"not hex":            {testWebhookSecret, body, signaturePrefix + "zzzz"},
		"empty secret":       {"", body, signCallback("", body)},
		"secret is a prefix": {testWebhookSecret[:5], body, signCallback(testWebhookSecret, body)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if VerifySignature(tc.secret, tc.body, tc.header) {
				t.Error("an invalid signature was accepted")
			}
		})
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
	verifier, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}

	var (
		called   bool
		gotBody  []byte
		gotLen   int64
		gotClose bool
	)
	handler := verifier.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotBody, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
		gotClose = r.Body != nil
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, body))
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
	verifier, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}

	called := false
	handler := verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(string(body)))
	req.Header.Set(SignatureHeader, signCallback("wrong-secret", body))
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

func TestSignatureVerifier_MiddlewareRejectsOversizedBody(t *testing.T) {
	verifier, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}

	called := false
	handler := verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	big := strings.Repeat("x", int(verifier.maxBytes)+1)
	req := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(big))
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, []byte(big)))
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
	verifier, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}

	handler := verifier.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
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
	verifier, err := NewSignatureVerifier(testWebhookSecret)
	if err != nil {
		t.Fatalf("NewSignatureVerifier: %v", err)
	}

	handler := verifier.Handler(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})

	req := httptest.NewRequest(http.MethodPost, "/cb", strings.NewReader(string(body)))
	req.Header.Set(SignatureHeader, signCallback(testWebhookSecret, body))
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(body) {
		t.Errorf("body = %q, want the original payload", rec.Body.String())
	}
}
