package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSend_PlainMessage(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	res, err := client.Send(context.Background(), SendParams{
		Channel:   ChannelSMS,
		Recipient: "+989120000000",
		Body:      "Your code is 123456",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	if body["channel_type"] != "sms" {
		t.Errorf("channel_type = %v, want sms", body["channel_type"])
	}
	if body["recipient"] != "+989120000000" {
		t.Errorf("recipient = %v", body["recipient"])
	}
	if body["body"] != "Your code is 123456" {
		t.Errorf("body = %v", body["body"])
	}
	for _, key := range []string{"subject", "template_id", "template_name", "group_name", "locale", "metadata", "send_at"} {
		if _, present := body[key]; present {
			t.Errorf("%s should be omitted when unset, got %v", key, body[key])
		}
	}

	if res.ID != "9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("ID = %q", res.ID)
	}
	if !res.IsDelivered() || res.IsFailed() || res.IsPending() {
		t.Errorf("unexpected status helpers for %q", res.Status)
	}
	if res.Provider != "kavenegar" || res.ProviderMessageID != "1234567890" {
		t.Errorf("provider detail = (%q, %q)", res.Provider, res.ProviderMessageID)
	}
}

func TestSend_PositionalTemplateValues(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.Send(context.Background(), SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "order_notice",
		Values:       []string{"50000", "Tehran"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	if body["template_name"] != "order_notice" {
		t.Errorf("template_name = %v", body["template_name"])
	}
	params, ok := body["template_params"].([]any)
	if !ok {
		t.Fatalf("template_params missing or not an array: %v", body["template_params"])
	}
	if len(params) != 2 || params[0] != "50000" || params[1] != "Tehran" {
		t.Errorf("template_params = %v, want [50000 Tehran]", params)
	}
	if _, present := body["template_vars"]; present {
		t.Errorf("template_vars should be omitted when empty, got %v", body["template_vars"])
	}
}

func TestSend_NamedTemplateVars(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.Send(context.Background(), SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "verify",
		Vars:         map[string]any{"token": "123456"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	vars, ok := body["template_vars"].(map[string]any)
	if !ok || vars["token"] != "123456" {
		t.Errorf("template_vars = %v, want {token: 123456}", body["template_vars"])
	}
}

func TestSend_GroupTemplateShorthand(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.Send(context.Background(), SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "vibe:salerequest",
		Values:       []string{"50000"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	if body["group_name"] != "vibe" {
		t.Errorf("group_name = %v, want vibe", body["group_name"])
	}
	if body["template_name"] != "salerequest" {
		t.Errorf("template_name = %v, want salerequest", body["template_name"])
	}
}

func TestSend_ExplicitGroupOverridesShorthand(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.Send(context.Background(), SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "vibe:salerequest",
		Group:        "sms-primary",
		Values:       []string{"1"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	if body["group_name"] != "sms-primary" {
		t.Errorf("group_name = %v, want sms-primary", body["group_name"])
	}
	if body["template_name"] != "salerequest" {
		t.Errorf("template_name = %v, want salerequest", body["template_name"])
	}
}

func TestSend_AllOptionalFields(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	sendAt := time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC)
	_, err := client.Send(context.Background(), SendParams{
		Channel:        ChannelEmail,
		Recipient:      "ops@example.com",
		Subject:        "Nightly report",
		TemplateID:     "3f8b1c2d-0000-1111-2222-333344445555",
		Vars:           map[string]any{"count": 7},
		ChannelID:      "11111111-2222-3333-4444-555555555555",
		GroupID:        "22222222-3333-4444-5555-666666666666",
		Locale:         "en",
		SendAt:         &sendAt,
		CallbackURL:    "https://example.com/notifier/cb",
		IdempotencyKey: "order-42-confirmation",
		Metadata:       json.RawMessage(`{"order_id":42}`),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	body := cap.jsonBody(t)
	if body["channel_type"] != "email" || body["subject"] != "Nightly report" {
		t.Errorf("unexpected channel/subject: %v / %v", body["channel_type"], body["subject"])
	}
	if body["template_id"] != "3f8b1c2d-0000-1111-2222-333344445555" {
		t.Errorf("template_id = %v", body["template_id"])
	}
	if body["channel_id"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("channel_id = %v", body["channel_id"])
	}
	if body["group_id"] != "22222222-3333-4444-5555-666666666666" {
		t.Errorf("group_id = %v", body["group_id"])
	}
	if body["locale"] != "en" {
		t.Errorf("locale = %v", body["locale"])
	}
	if body["send_at"] != "2026-09-11T08:30:00Z" {
		t.Errorf("send_at = %v, want 2026-09-11T08:30:00Z", body["send_at"])
	}
	if body["callback_url"] != "https://example.com/notifier/cb" {
		t.Errorf("callback_url = %v", body["callback_url"])
	}
	if body["idempotency_key"] != "order-42-confirmation" {
		t.Errorf("idempotency_key = %v", body["idempotency_key"])
	}
	if md, ok := body["metadata"].(map[string]any); !ok || md["order_id"] != float64(42) {
		t.Errorf("metadata = %v, want {order_id:42}", body["metadata"])
	}
}

func TestSend_ValidationRejectsBeforeAnyRequest(t *testing.T) {
	tests := map[string]SendParams{
		"missing channel":   {Recipient: "+98912", Body: "hi"},
		"missing recipient": {Channel: ChannelSMS, Body: "hi"},
		"blank recipient":   {Channel: ChannelSMS, Recipient: "   ", Body: "hi"},
		"missing content":   {Channel: ChannelSMS, Recipient: "+98912"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			cap := &captured{}
			client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

			_, err := client.Send(context.Background(), params)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
			if _, _, _, _, calls := cap.snapshot(); calls != 0 {
				t.Errorf("the SDK sent %d requests; validation must happen locally", calls)
			}
		})
	}
}

func TestSendText(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	if _, err := client.SendText(context.Background(), ChannelBale, "+989120000000", "hello"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	body := cap.jsonBody(t)
	if body["channel_type"] != "bale" || body["body"] != "hello" {
		t.Errorf("unexpected body: %v", body)
	}
}

func TestSendTemplateByName(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.SendTemplateByName(context.Background(), TemplateParams{
		Channel:   ChannelSMS,
		Recipient: "+989120000000",
		Template:  "sms-group:verify",
		Values:    []string{"123456"},
		Locale:    "fa",
	})
	if err != nil {
		t.Fatalf("SendTemplateByName: %v", err)
	}

	body := cap.jsonBody(t)
	if body["template_name"] != "verify" || body["group_name"] != "sms-group" {
		t.Errorf("routing = (%v, %v), want (verify, sms-group)", body["template_name"], body["group_name"])
	}
	if body["locale"] != "fa" {
		t.Errorf("locale = %v", body["locale"])
	}
}

func TestSendTemplateByName_RequiresTemplate(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.SendTemplateByName(context.Background(), TemplateParams{
		Channel:   ChannelSMS,
		Recipient: "+989120000000",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if _, _, _, _, calls := cap.snapshot(); calls != 0 {
		t.Errorf("the SDK sent %d requests, want 0", calls)
	}
}

func TestStatus_ByNotificationID(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, notificationJSON))

	res, err := client.Status(context.Background(), "9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	method, target, _, _, _ := cap.snapshot()
	if method != http.MethodGet {
		t.Errorf("method = %s, want GET", method)
	}
	if target != "/v1/notifications/9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("target = %s", target)
	}
	if !res.IsDelivered() {
		t.Errorf("status = %q, want delivered", res.Status)
	}
	if res.SentAt != nil {
		t.Logf("note: sent_at present in fixture: %v", res.SentAt)
	}
}

func TestStatus_RequiresID(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusOK, notificationJSON))
	if _, err := client.Status(context.Background(), "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
}

func TestStatusByProviderMessageID(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, notificationJSON))

	if _, err := client.StatusByProviderMessageID(context.Background(), "1234567890"); err != nil {
		t.Fatalf("StatusByProviderMessageID: %v", err)
	}
	_, target, _, _, _ := cap.snapshot()
	if target != "/v1/notifications/by-provider-message-id/1234567890" {
		t.Errorf("target = %s", target)
	}
}

func TestStatusByProviderMessageID_RequiresID(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusOK, notificationJSON))
	if _, err := client.StatusByProviderMessageID(context.Background(), ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
}

func TestStatusByProviderMessageID_EscapesPathSegment(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, notificationJSON))

	if _, err := client.StatusByProviderMessageID(context.Background(), "a/b c"); err != nil {
		t.Fatalf("StatusByProviderMessageID: %v", err)
	}
	_, target, _, _, _ := cap.snapshot()
	if strings.Contains(target, "/a/b") {
		t.Errorf("path segment was not escaped: %s", target)
	}
}

func TestHistory_SendsFiltersAsQuery(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, `{"items":[],"next_cursor":"abc"}`))

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	page, err := client.History(context.Background(), HistoryQuery{
		Limit:     50,
		After:     "cursor-1",
		Status:    StatusFailed,
		Channel:   ChannelSMS,
		Recipient: "+98912",
		ChannelID: "11111111-2222-3333-4444-555555555555",
		GroupID:   "22222222-3333-4444-5555-666666666666",
		GroupName: "vibe",
		From:      from,
		To:        to,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	_, target, _, _, _ := cap.snapshot()
	if !strings.HasPrefix(target, "/v1/notifications?") {
		t.Fatalf("target = %s, want a query string", target)
	}
	query, err := urlParseQuery(target)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	want := map[string]string{
		"limit":        "50",
		"after":        "cursor-1",
		"status":       "failed",
		"channel_type": "sms",
		"recipient":    "+98912",
		"channel_id":   "11111111-2222-3333-4444-555555555555",
		"group_id":     "22222222-3333-4444-5555-666666666666",
		"group_name":   "vibe",
		"from":         "2026-09-01T00:00:00Z",
		"to":           "2026-09-10T00:00:00Z",
	}
	for key, expected := range want {
		if got := query.Get(key); got != expected {
			t.Errorf("query %s = %q, want %q", key, got, expected)
		}
	}
	if page.NextCursor != "abc" || !page.HasMore() {
		t.Errorf("page = %+v, want next_cursor abc", page)
	}
}

func TestHistory_OmitsEmptyFilters(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, `{"items":[]}`))

	page, err := client.History(context.Background(), HistoryQuery{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	_, target, _, _, _ := cap.snapshot()
	if target != "/v1/notifications" {
		t.Errorf("target = %s, want no query string", target)
	}
	if page.HasMore() {
		t.Error("expected no next cursor")
	}
}

func TestHistoryAll_PaginatesUntilExhausted(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"id":"1","status":"delivered"},{"id":"2","status":"failed"}],"next_cursor":"c1"}`)
		case "c1":
			_, _ = io.WriteString(w, `{"items":[{"id":"3","status":"queued"}]}`)
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("after"))
		}
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var ids []string
	if err := client.HistoryAll(context.Background(), HistoryQuery{Limit: 2}, func(n *Notification) error {
		ids = append(ids, n.ID)
		return nil
	}); err != nil {
		t.Fatalf("HistoryAll: %v", err)
	}

	if strings.Join(ids, ",") != "1,2,3" {
		t.Errorf("ids = %v, want [1 2 3]", ids)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestHistoryAll_StopsOnCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"1"},{"id":"2"}],"next_cursor":"c1"}`)
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	stop := errors.New("stop")
	seen := 0
	err = client.HistoryAll(context.Background(), HistoryQuery{}, func(*Notification) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("error = %v, want the callback error", err)
	}
	if seen != 2 {
		t.Errorf("callback ran %d times, want 2", seen)
	}
}

func TestHistoryAll_StopsWhenTheContextEndsMidWalk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"1"}],"next_cursor":"c1"}`)
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = client.HistoryAll(ctx, HistoryQuery{}, func(*Notification) error {
		cancel() // the caller loses interest after the first item
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestHistoryAll_DetectsStalledCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Always claims more pages with a cursor that never advances.
		_, _ = io.WriteString(w, `{"items":[{"id":"1"}],"next_cursor":"same"}`)
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	err = client.HistoryAll(context.Background(), HistoryQuery{}, func(*Notification) error { return nil })
	if !errors.Is(err, errCursorStalled) {
		t.Fatalf("error = %v, want errCursorStalled", err)
	}
}

func TestHistoryAll_RequiresCallback(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusOK, `{"items":[]}`))
	if err := client.HistoryAll(context.Background(), HistoryQuery{}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
}

// A developer rehearses an integration by adding one field to a request they already send: the
// template, group and channel configuration are untouched.
func TestSend_SimulatedFlagIsSentOnTheWire(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	_, err := client.SendTemplateByName(context.Background(), TemplateParams{
		Channel:   ChannelSMS,
		Recipient: "+989120000000",
		Template:  "sms-group:verify",
		Values:    []string{"123456"},
		Simulated: true,
	})
	if err != nil {
		t.Fatalf("SendTemplateByName: %v", err)
	}

	body := cap.jsonBody(t)
	if body["simulated"] != true {
		t.Errorf("simulated = %v, want true", body["simulated"])
	}
	// The routing the caller rehearses is unchanged.
	if body["group_name"] != "sms-group" || body["template_name"] != "verify" {
		t.Errorf("routing = (%v, %v), want (sms-group, verify)", body["group_name"], body["template_name"])
	}
}

func TestSend_SimulatedIsOmittedWhenFalse(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	if _, err := client.SendText(context.Background(), ChannelSMS, "+989120000000", "hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if _, present := cap.jsonBody(t)["simulated"]; present {
		t.Error("simulated should be omitted when false")
	}
}

func TestNotification_ReportsSimulated(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusOK,
		`{"id":"1","status":"delivered","provider":"simulator","provider_message_id":"sim-sms-kavenegar-1","simulated":true}`))

	n, err := client.Status(context.Background(), "1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !n.Simulated {
		t.Error("the client did not read back the simulated flag")
	}
	if n.Provider != "simulator" {
		t.Errorf("provider = %q, want simulator", n.Provider)
	}
}
