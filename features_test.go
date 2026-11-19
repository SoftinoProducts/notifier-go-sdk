package notifier

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSend_PriorityAndIdempotencyHeaders(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))
	if _, err := client.Send(context.Background(), SendParams{
		Channel:        ChannelSMS,
		Recipient:      "+989120000000",
		Body:           "hi",
		Priority:       PriorityHigh,
		IdempotencyKey: "otp:1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, header, body, calls := cap.snapshot()
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
	if got := header.Get("X-Priority"); got != "high" {
		t.Errorf("X-Priority = %q, want high", got)
	}
	if got := header.Get("Idempotency-Key"); got != "otp:1" {
		t.Errorf("Idempotency-Key = %q, want otp:1", got)
	}
	if !strings.Contains(string(body), `"idempotency_key":"otp:1"`) {
		t.Errorf("body = %s, want idempotency_key", body)
	}
}

func TestSend_DefaultPriorityHeaderIsBulk(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))
	if _, err := client.Send(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, header, _, _ := cap.snapshot()
	if got := header.Get("X-Priority"); got != "bulk" {
		t.Errorf("X-Priority = %q, want bulk", got)
	}
}

func TestSendTemplateByName_PropagatesPriorityAndKey(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))
	if _, err := client.SendTemplateByName(context.Background(), TemplateParams{
		Channel:        ChannelSMS,
		Recipient:      "+989120000000",
		Template:       "verify",
		Values:         []string{"123456"},
		Priority:       PriorityHigh,
		IdempotencyKey: "otp:template",
	}); err != nil {
		t.Fatalf("SendTemplateByName: %v", err)
	}
	_, _, header, _, _ := cap.snapshot()
	if got := header.Get("X-Priority"); got != "high" {
		t.Errorf("X-Priority = %q, want high", got)
	}
	if got := header.Get("Idempotency-Key"); got != "otp:template" {
		t.Errorf("Idempotency-Key = %q", got)
	}
}

func TestBulk_SendsBulkPriorityHeader(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, `{"batch_id":"b","count":1}`))
	_, err := client.Bulk(context.Background(), BulkParams{
		Channel:  ChannelSMS,
		Messages: []BulkMessage{{Recipient: "+989120000000", Body: "hi"}},
	})
	if err != nil {
		t.Fatalf("Bulk: %v", err)
	}
	_, _, header, _, _ := cap.snapshot()
	if got := header.Get("X-Priority"); got != "bulk" {
		t.Errorf("X-Priority = %q, want bulk", got)
	}
}

func TestSend_RejectsInvalidPriorityBeforeSending(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	_, err := client.Send(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi", Priority: "urgent",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid priority must be rejected before the request")
	}
}

func TestSend_ServerErrorDoesNotLeakRenderedBody(t *testing.T) {
	const otp = "987654"
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("<html>Your verification code is " + otp + "</html>"))
	}))
	_, err := client.Send(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.Message != "HTTP 500" {
		t.Errorf("Message = %q, want generic HTTP 500", apiErr.Message)
	}
	if strings.Contains(err.Error(), otp) {
		t.Error("error string leaked the rendered body/OTP")
	}
}

func TestNewIdempotencyKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		key, err := NewIdempotencyKey()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != 32 {
			t.Fatalf("key length = %d, want 32", len(key))
		}
		if seen[key] {
			t.Fatalf("duplicate key %q", key)
		}
		seen[key] = true
	}
}

func TestWithMaxConnsPerHost(t *testing.T) {
	client, err := NewClient("key", WithBaseURL("https://api.example.com"), WithMaxConnsPerHost(7))
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.http.Transport)
	}
	if tr.MaxConnsPerHost != 7 {
		t.Errorf("MaxConnsPerHost = %d, want 7", tr.MaxConnsPerHost)
	}

	if _, err := NewClient("key", WithHTTPClient(&http.Client{}), WithMaxConnsPerHost(7)); err == nil {
		t.Fatal("combining WithHTTPClient and WithMaxConnsPerHost should error")
	}
	if _, err := NewClient("key", WithMaxConnsPerHost(-1)); err == nil {
		t.Fatal("a negative max connection count should error")
	}
}

func TestAPIError_RetryableSend(t *testing.T) {
	tests := []struct {
		status int
		key    string
		want   bool
	}{
		{http.StatusBadRequest, "k", false},
		{http.StatusTooManyRequests, "", true},
		{http.StatusServiceUnavailable, "", false},
		{http.StatusServiceUnavailable, "k", true},
		{http.StatusGatewayTimeout, "k", true},
		{http.StatusRequestTimeout, "k", true},
	}
	for _, tc := range tests {
		got := (&APIError{StatusCode: tc.status}).RetryableSend(tc.key)
		if got != tc.want {
			t.Errorf("status %d key %q: RetryableSend = %v, want %v", tc.status, tc.key, got, tc.want)
		}
	}
}

func TestIsPreSendError(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "api.example.com"}
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
	readErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"dns", dnsErr, true},
		{"dial", dialErr, true},
		{"read", readErr, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"generic", errors.New("EOF"), false},
	}
	for _, tc := range tests {
		if got := IsPreSendError(tc.err); got != tc.want {
			t.Errorf("%s: IsPreSendError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEndToEnd_SenderClientStatusAndClose(t *testing.T) {
	const id = "8c1d5d36-3a1e-4a9b-9f52-0f1a2b3c4d5e"
	var mu sync.Mutex
	var priorities []string
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + id + `","status":"delivered","channel_type":"sms","recipient":"+989120000000"}`))
			return
		}
		mu.Lock()
		priorities = append(priorities, r.Header.Get("X-Priority"))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"` + id + `","status":"queued","channel_type":"sms","recipient":"+989120000000"}`))
	}))
	defer srv.Close()

	client, err := NewClient(testAPIKey, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	sender := newSender(t, client, SenderOptions{
		HighWorkers: 1, BulkWorkers: 1,
		HighQueueSize: 10, BulkQueueSize: 10,
	})

	high, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "otp",
		Priority: PriorityHigh, IdempotencyKey: "otp:sale-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := waitResult(t, high); res.Err != nil || res.Notification == nil {
		t.Fatalf("high result = %+v", res)
	}

	bulk, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000001", Body: "bulk",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := waitResult(t, bulk); res.Err != nil || res.Notification == nil {
		t.Fatalf("bulk result = %+v", res)
	}

	status, err := client.Status(context.Background(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.IsDelivered() {
		t.Errorf("status = %s, want delivered", status.Status)
	}
	if err := sender.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(priorities) != 2 || priorities[0] != "high" || priorities[1] != "bulk" {
		t.Errorf("priorities = %v, want [high bulk]", priorities)
	}
	if keys[0] != "otp:sale-1" || keys[1] == "" {
		t.Errorf("keys = %v", keys)
	}
}

func TestSender_CloseIsConcurrentSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()
	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender, err := NewSender(client, SenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = sender.Close(ctx)
		}()
	}
	wg.Wait()
}
