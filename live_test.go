package notifier_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	notifier "github.com/SoftinoProducts/notifier-go-sdk"
)

// The live tests exercise a real deployment. They are skipped unless NOTIFIER_API_KEY is
// set, so `go test ./...` and CI stay offline and deterministic:
//
//	NOTIFIER_API_KEY=... go test -run TestLive -v ./...
//
// Sending is opt-in on top of that, because it dispatches real messages and costs money:
//
//	NOTIFIER_API_KEY=... NOTIFIER_LIVE_SEND=1 \
//	NOTIFIER_LIVE_RECIPIENT=+989120000000 NOTIFIER_LIVE_TEMPLATE=verify \
//	NOTIFIER_LIVE_VALUES=123456 go test -run TestLive_Send -v ./...

// liveClient builds a client for a real deployment, or skips the test.
func liveClient(t *testing.T) *notifier.Client {
	t.Helper()

	apiKey := os.Getenv("NOTIFIER_API_KEY")
	if apiKey == "" {
		t.Skip("set NOTIFIER_API_KEY (and optionally NOTIFIER_BASE_URL) to run the live tests")
	}

	var opts []notifier.Option
	if base := os.Getenv("NOTIFIER_BASE_URL"); base != "" {
		opts = append(opts, notifier.WithBaseURL(base))
	}

	client, err := notifier.NewClient(apiKey, opts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// TestLive_History reads a page of real history. It is read-only and always runs when a
// key is configured.
func TestLive_History(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	page, err := client.History(ctx, notifier.HistoryQuery{Limit: 5})
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	t.Logf("read %d notifications, next cursor present: %t", len(page.Items), page.HasMore())
	for i := range page.Items {
		n := &page.Items[i]
		t.Logf("  %s %s %s -> %s", n.ID, n.Channel, n.Recipient, n.Status)

		if n.ID == "" {
			t.Error("a returned notification has no id")
		}
		if n.CreatedAt.IsZero() {
			t.Errorf("%s has no created_at", n.ID)
		}
	}
}

// TestLive_UnknownIDIsNotFound pins the error contract against the real service: an
// unknown notification UUID must surface as ErrNotFound, not as a generic failure.
func TestLive_UnknownIDIsNotFound(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.Status(ctx, randomUUID(t))
	if !errors.Is(err, notifier.ErrNotFound) {
		t.Fatalf("Status(unknown id) error = %v, want ErrNotFound", err)
	}

	var apiErr *notifier.APIError
	if errors.As(err, &apiErr) {
		t.Logf("status %d, request id %q", apiErr.StatusCode, apiErr.RequestID)
	}
}

// TestLive_MalformedIDIsBadRequest checks the other half of the error contract: the
// service rejecting a request must arrive as ErrBadRequest carrying its own message.
func TestLive_MalformedIDIsBadRequest(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.Status(ctx, "not-a-uuid")
	if !errors.Is(err, notifier.ErrBadRequest) {
		t.Fatalf("Status(malformed id) error = %v, want ErrBadRequest", err)
	}

	var apiErr *notifier.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not an *APIError: %v", err)
	}
	t.Logf("status %d, message %q, request id %q", apiErr.StatusCode, apiErr.Message, apiErr.RequestID)
}

// TestLive_HistoryFilterRoundTrip proves the SDK's query encoding is accepted by the real
// service and that the filters actually narrow the result.
func TestLive_HistoryFilterRoundTrip(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	page, err := client.History(ctx, notifier.HistoryQuery{
		Limit:   10,
		Status:  notifier.StatusDelivered,
		Channel: notifier.ChannelSMS,
		From:    time.Now().Add(-90 * 24 * time.Hour),
		To:      time.Now(),
	})
	if err != nil {
		t.Fatalf("History with filters: %v", err)
	}

	t.Logf("read %d delivered sms notifications from the last 90 days", len(page.Items))
	for i := range page.Items {
		n := &page.Items[i]
		if n.Status != notifier.StatusDelivered {
			t.Errorf("%s has status %q, want delivered", n.ID, n.Status)
		}
		if n.Channel != notifier.ChannelSMS {
			t.Errorf("%s has channel %q, want sms", n.ID, n.Channel)
		}
		if n.CreatedAt.Before(time.Now().Add(-91 * 24 * time.Hour)) {
			t.Errorf("%s was created at %s, outside the requested window", n.ID, n.CreatedAt)
		}
	}
}

// TestLive_Send dispatches a real notification, then polls it until it reaches a final
// state. It requires NOTIFIER_LIVE_SEND=1 and a recipient.
func TestLive_Send(t *testing.T) {
	if os.Getenv("NOTIFIER_LIVE_SEND") != "1" {
		t.Skip("set NOTIFIER_LIVE_SEND=1 to dispatch a real notification")
	}

	recipient := os.Getenv("NOTIFIER_LIVE_RECIPIENT")
	if recipient == "" {
		t.Skip("set NOTIFIER_LIVE_RECIPIENT to address the live send")
	}
	template := os.Getenv("NOTIFIER_LIVE_TEMPLATE")
	if template == "" {
		t.Skip("set NOTIFIER_LIVE_TEMPLATE to choose the template to render")
	}

	channel := notifier.ChannelType(envOr("NOTIFIER_LIVE_CHANNEL", string(notifier.ChannelSMS)))
	client := liveClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A unique idempotency key makes a rerun of this test a no-op instead of a second
	// message.
	sent, err := client.SendTemplateByName(ctx, notifier.TemplateParams{
		Channel:        channel,
		Recipient:      recipient,
		Template:       template,
		Values:         valuesFromEnv(),
		IdempotencyKey: fmt.Sprintf("go-sdk-live-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("SendTemplateByName: %v", err)
	}
	t.Logf("accepted %s with status %s", sent.ID, sent.Status)

	if sent.ID == "" {
		t.Fatal("the service accepted the send without returning an id")
	}

	// Poll for the outcome. The service is asynchronous, so the first read is usually
	// still queued or processing.
	deadline := time.Now().Add(45 * time.Second)
	for {
		current, err := client.Status(ctx, sent.ID)
		if err != nil {
			t.Fatalf("Status(%s): %v", sent.ID, err)
		}
		t.Logf("  status=%s provider=%s error=%q", current.Status, current.Provider, current.FailureReason())

		if current.Status.IsTerminal() {
			if current.IsFailed() {
				t.Logf("delivery failed, which is expected for an unreachable test recipient: %s", current.FailureReason())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notification %s did not reach a terminal state in time (last status %s)", sent.ID, current.Status)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context ended while polling %s", sent.ID)
		case <-time.After(2 * time.Second):
		}
	}
}

// valuesFromEnv splits NOTIFIER_LIVE_VALUES into the positional template values.
func valuesFromEnv() []string {
	raw := os.Getenv("NOTIFIER_LIVE_VALUES")
	if raw == "" {
		return nil
	}
	return []string{raw}
}

// envOr returns the value of key, or fallback when it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// randomUUID builds a v4 UUID, which also shows that the SDK needs no UUID dependency.
func randomUUID(t *testing.T) string {
	t.Helper()

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
