package notifier_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	notifier "github.com/SoftinoProducts/notifier-go-sdk"
)

// stubServer starts a test server and returns a client pointed at it, so the examples
// below are runnable and self-contained.
func stubServer(handler http.HandlerFunc) (*notifier.Client, *httptest.Server, error) {
	server := httptest.NewServer(handler)
	client, err := notifier.NewClient("your-api-key", notifier.WithBaseURL(server.URL))
	if err != nil {
		server.Close()
		return nil, nil, err
	}
	return client, server, nil
}

func ExampleClient_SendText() {
	client, server, err := stubServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8","status":"queued"}`)
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	res, err := client.SendText(context.Background(),
		notifier.ChannelSMS, "+989120000000", "Your code is 123456")
	if err != nil {
		panic(err)
	}

	fmt.Println(res.ID, res.Status)
	// Output: 9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8 queued
}

func ExampleClient_SendTemplateByName() {
	var received string
	client, server, err := stubServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"abc","status":"queued"}`)
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	// The template "verify" declares param_names ["token"], so the value binds
	// positionally and the service renders the text.
	if _, err := client.SendTemplateByName(context.Background(), notifier.TemplateParams{
		Channel:   notifier.ChannelSMS,
		Recipient: "+989120000000",
		Template:  "verify",
		Values:    []string{"123456"},
	}); err != nil {
		panic(err)
	}

	fmt.Println(received)
	// Output: {"channel_type":"sms","recipient":"+989120000000","template_name":"verify","template_params":["123456"]}
}

func ExampleClient_Send_templateByGroupAndName() {
	var received string
	client, server, err := stubServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"abc","status":"queued"}`)
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	// "sms-group:verify" routes through the channel group named sms-group and renders the
	// template named verify. The value is addressed by placeholder name here.
	if _, err := client.Send(context.Background(), notifier.SendParams{
		Channel:      notifier.ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "sms-group:verify",
		Vars:         map[string]any{"token": "123456"},
	}); err != nil {
		panic(err)
	}

	fmt.Println(received)
	// Output: {"channel_type":"sms","recipient":"+989120000000","group_name":"sms-group","template_name":"verify","template_vars":{"token":"123456"}}
}

func ExampleClient_BulkConcurrent() {
	var batches atomic.Int32
	client, server, err := stubServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		// The real service acknowledges each chunk with its own batch id and count.
		fmt.Fprintf(w, `{"batch_id":"batch-%d","count":%d}`, batches.Add(1), len(req.Messages))
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	messages := []notifier.BulkMessage{
		{Recipient: "+989120000001", TemplateName: "verify", Values: []string{"111111"}},
		{Recipient: "+989120000002", TemplateName: "verify", Values: []string{"222222"}},
		{Recipient: "+989120000003", TemplateName: "verify", Values: []string{"333333"}},
	}

	// Three messages with a chunk size of two become two requests, both in flight.
	result, err := client.BulkConcurrent(context.Background(),
		notifier.BulkParams{Channel: notifier.ChannelSMS, Messages: messages},
		notifier.BulkOptions{ChunkSize: 2, Concurrency: 2})
	if err != nil {
		panic(err)
	}

	fmt.Println("accepted:", result.Total, "batches:", len(result.Batches), "ok:", result.OK())
	// Output: accepted: 3 batches: 2 ok: true
}

func ExampleClient_History() {
	client, server, err := stubServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"id":"1","status":"delivered"},{"id":"2","status":"failed","error":"kavenegar: 411 missing parameters"}],"next_cursor":"eyJpZCI6IjIifQ"}`)
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()

	page, err := client.History(context.Background(), notifier.HistoryQuery{
		Limit:   2,
		Status:  notifier.StatusFailed,
		Channel: notifier.ChannelSMS,
		From:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		panic(err)
	}

	for i := range page.Items {
		fmt.Println(page.Items[i].ID, page.Items[i].Status)
	}
	fmt.Println("more:", page.HasMore())
	// Output:
	// 1 delivered
	// 2 failed
	// more: true
}

func ExampleJSON() {
	metadata, err := notifier.JSON(map[string]any{"order_id": 42, "tenant": "vibe"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(metadata))
	// Output: {"order_id":42,"tenant":"vibe"}
}

func ExampleSignatureVerifier_Handler() {
	const secret = "webhook-signing-secret"

	verifier, err := notifier.NewSignatureVerifier(secret)
	if err != nil {
		panic(err)
	}

	handler := verifier.Handler(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write(body)
	})

	// A request as the service would send it.
	payload := `{"id":"abc","status":"delivered"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	signed := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(payload))
	signed.Header.Set(notifier.SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	handler(recorder, signed)
	fmt.Println("signed:", recorder.Code)

	// The same request with a digest that does not match.
	tampered := httptest.NewRequest(http.MethodPost, "/notifier/callback", strings.NewReader(`{"id":"abc","status":"failed"}`))
	tampered.Header.Set(notifier.SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder = httptest.NewRecorder()
	handler(recorder, tampered)
	fmt.Println("tampered:", recorder.Code)

	// Output:
	// signed: 204
	// tampered: 401
}

func ExampleNewClient_withOptions() {
	client, err := notifier.NewClient("your-api-key",
		notifier.WithBaseURL("https://notifier.staging.internal"),
		notifier.WithTimeout(10*time.Second),
		notifier.WithUserAgent("trade-hub/1.4"),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(client.BaseURL())
	// Output: https://notifier.staging.internal
}
