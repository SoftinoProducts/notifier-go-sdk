package notifier

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPIError_IsMapping(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrBadRequest},
		{http.StatusUnprocessableEntity, ErrBadRequest},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrConflict},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusServiceUnavailable, ErrUnavailable},
		{http.StatusInternalServerError, ErrServer},
		{http.StatusBadGateway, ErrServer},
		{http.StatusGatewayTimeout, ErrServer},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status, Message: "x", Method: "GET", Path: "/v1/notifications"}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: errors.Is(err, %v) = false, want true", tc.status, tc.want)
		}
	}
}

func TestAPIError_IsDoesNotOverMatch(t *testing.T) {
	err := &APIError{StatusCode: http.StatusNotFound, Message: "not found"}
	for _, target := range []error{ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrConflict, ErrRateLimited, ErrUnavailable, ErrServer, ErrInvalidArgument} {
		if errors.Is(err, target) {
			t.Errorf("a 404 must not match %v", target)
		}
	}
	// 503 is its own class: it means "this tenant has no channel available", not "the
	// service broke".
	unavailable := &APIError{StatusCode: http.StatusServiceUnavailable}
	if errors.Is(unavailable, ErrServer) {
		t.Error("503 must not also match ErrServer")
	}
}

func TestAPIError_Message(t *testing.T) {
	err := &APIError{
		StatusCode: http.StatusConflict,
		Message:    "duplicate idempotency key",
		RequestID:  "d2f6b0d0-1c2a-4d3e-9f10-abcdefabcdef",
		Method:     http.MethodPost,
		Path:       "/v1/notifications",
	}
	got := err.Error()
	for _, want := range []string{"POST", "/v1/notifications", "409", "duplicate idempotency key", "d2f6b0d0"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to contain %q", got, want)
		}
	}
}

func TestAPIError_Retryable(t *testing.T) {
	tests := map[int]bool{
		http.StatusOK:                  false,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusNotFound:            false,
		http.StatusConflict:            false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusServiceUnavailable:  true,
	}
	for status, want := range tests {
		if got := (&APIError{StatusCode: status}).Retryable(); got != want {
			t.Errorf("status %d: Retryable() = %v, want %v", status, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	// 60 seconds, as the middleware sets it.
	if got := parseRetryAfter("60"); got != 60*time.Second {
		t.Errorf("parseRetryAfter(60) = %v, want 1m", got)
	}
	if got := parseRetryAfter("0"); got != 0 {
		t.Errorf("parseRetryAfter(0) = %v, want 0", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("parseRetryAfter(-5) = %v, want 0", got)
	}
	if got := parseRetryAfter("not-a-number-or-date"); got != 0 {
		t.Errorf("parseRetryAfter(garbage) = %v, want 0", got)
	}

	// HTTP-date form, about 30 seconds in the future.
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 31*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %v, want ~30s", got)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past date) = %v, want 0", got)
	}
}

func TestErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"envelope", `{"error":"not found"}`, "not found"},
		{"envelope with extra keys", `{"error":"invalid input","detail":"x"}`, "invalid input"},
		{"empty", ``, "empty error response"},
		{"whitespace", "  \n ", "empty error response"},
		{"plain text", "upstream timeout", "upstream timeout"},
		{"html error page", "<html>502</html>", "<html>502</html>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("errorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestErrorMessage_TruncatesHugeBodies(t *testing.T) {
	got := errorMessage([]byte(strings.Repeat("x", 5000)))
	if len(got) > 520 {
		t.Errorf("message length = %d, want it truncated", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated message should be marked, got %q", got[len(got)-10:])
	}
}

func TestClient_MapsAPIErrorWithRequestIDAndRetryAfter(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-123")
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))

	_, err := client.Status(context.Background(), "some-id")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *APIError: %v", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Message != "rate limit exceeded" {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.RequestID != "req-123" {
		t.Errorf("RequestID = %q, want req-123", apiErr.RequestID)
	}
	if apiErr.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter = %v, want 1m", apiErr.RetryAfter)
	}
	if !apiErr.Retryable() {
		t.Error("a 429 should be reported as retryable")
	}
	if apiErr.Method != http.MethodGet || apiErr.Path != "/v1/notifications/some-id" {
		t.Errorf("request identity = %s %s", apiErr.Method, apiErr.Path)
	}
}

func TestClient_MapsNotFoundOnStatus(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusNotFound, `{"error":"not found"}`))

	_, err := client.Status(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestClient_MapsConflictOnDuplicateIdempotencyKey(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusConflict, `{"error":"duplicate idempotency key"}`))

	_, err := client.Send(context.Background(), SendParams{
		Channel:        ChannelSMS,
		Recipient:      "+989120000000",
		Body:           "hi",
		IdempotencyKey: "order-42",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
}

func TestClient_MapsUnauthorized(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusUnauthorized, `{"error":"unauthorized"}`))

	_, err := client.History(context.Background(), HistoryQuery{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestClient_SurfacesTransportErrors(t *testing.T) {
	// A closed server makes every dial fail.
	client, err := NewClient("key", WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Status(context.Background(), "id")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	// It must not masquerade as an APIError.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("transport failure was reported as *APIError: %v", err)
	}
	if !strings.Contains(err.Error(), "GET /v1/notifications/id") {
		t.Errorf("error should name the request: %v", err)
	}
}

func TestClient_RejectsOversizedResponses(t *testing.T) {
	// Lower the cap so the test does not have to move 16 MiB.
	original := maxResponseBytes
	maxResponseBytes = 1024
	t.Cleanup(func() { maxResponseBytes = original })

	client := newTestClient(t, respondWith(nil, http.StatusOK, `{"items":[`+strings.Repeat(`"`, 4096)+`]}`))
	_, err := client.History(context.Background(), HistoryQuery{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want an oversized-response error", err)
	}
}

func TestClient_SucceedsOnEmptyResponseBody(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusNoContent, ""))
	if _, err := client.History(context.Background(), HistoryQuery{}); err != nil {
		t.Fatalf("History with a 204 and no body: %v", err)
	}
}

func TestClient_ReportsUndecodableResponse(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusOK, "not json"))
	_, err := client.History(context.Background(), HistoryQuery{})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("error = %v, want a decode error", err)
	}
}
