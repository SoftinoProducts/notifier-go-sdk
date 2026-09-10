package notifier

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// benchServer is a realistic local API: it decodes the request body and answers with a
// full notification payload, so the benchmark includes JSON on both sides and a real
// HTTP round trip over loopback.
func benchServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, notificationJSON)
	}))
}

func benchClient(b *testing.B) *Client {
	b.Helper()

	server := benchServer()
	b.Cleanup(server.Close)

	client, err := NewClient("bench-key", WithBaseURL(server.URL))
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	return client
}

// BenchmarkSend measures one full send: parameter validation, JSON encoding, the HTTP
// round trip on a pooled connection, and decoding the response.
func BenchmarkSend(b *testing.B) {
	client := benchClient(b)
	params := SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "vibe:salerequest",
		Values:       []string{"50000", "Tehran"},
		Vars:         map[string]any{"amount": "50000"},
		Metadata:     json.RawMessage(`{"order_id":42}`),
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Send(ctx, params); err != nil {
			b.Fatalf("Send: %v", err)
		}
	}
}

// BenchmarkSendParallel shows what the shared, correctly-sized connection pool buys:
// throughput scales with goroutines instead of queueing behind 2 idle connections per host.
func BenchmarkSendParallel(b *testing.B) {
	client := benchClient(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := client.Send(ctx, SendParams{
				Channel:   ChannelSMS,
				Recipient: "+989120000000",
				Body:      "Your code is 123456",
			}); err != nil {
				b.Fatalf("Send: %v", err)
			}
		}
	})
}

// BenchmarkSendParallelStockTransport is the control for BenchmarkSendParallel: it leaves
// the client on the stock http.DefaultTransport, which allows only 2 idle connections per
// host and therefore serializes concurrent calls to the same API endpoint. It exists to
// quantify — and guard — the tuning done by defaultTransport.
func BenchmarkSendParallelStockTransport(b *testing.B) {
	server := benchServer()
	b.Cleanup(server.Close)

	client, err := NewClient("bench-key", WithBaseURL(server.URL), WithHTTPClient(&http.Client{}))
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := client.Send(ctx, SendParams{
				Channel:   ChannelSMS,
				Recipient: "+989120000000",
				Body:      "Your code is 123456",
			}); err != nil {
				b.Fatalf("Send: %v", err)
			}
		}
	})
}

// BenchmarkSendParamsWire is the pure-CPU part of a send: validation plus rendering the
// request body. It should not allocate.
func BenchmarkSendParamsWire(b *testing.B) {
	params := SendParams{
		Channel:      ChannelSMS,
		Recipient:    "+989120000000",
		TemplateName: "vibe:salerequest",
		Values:       []string{"50000"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := params.wire(); err != nil {
			b.Fatalf("wire: %v", err)
		}
	}
}

func BenchmarkHistoryQueryValues(b *testing.B) {
	query := HistoryQuery{
		Limit:   50,
		After:   "cursor",
		Status:  StatusFailed,
		Channel: ChannelSMS,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := query.values(); len(got) != 4 {
			b.Fatalf("values = %d entries, want 4", len(got))
		}
	}
}

func BenchmarkPrepareBulk(b *testing.B) {
	messages := make([]BulkMessage, MaxBulkMessages*2)
	for i := range messages {
		messages[i] = BulkMessage{Recipient: "+989120000000", Body: "hi"}
	}
	params := BulkParams{Channel: ChannelSMS, Messages: messages}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, err := prepareBulk(params, 0)
		if err != nil {
			b.Fatalf("prepareBulk: %v", err)
		}
		if len(chunks) != 2 {
			b.Fatalf("chunks = %d, want 2", len(chunks))
		}
	}
}

func BenchmarkSignatureVerifierVerify(b *testing.B) {
	const secret = "bench-secret"
	verifier, err := NewSignatureVerifier(secret)
	if err != nil {
		b.Fatalf("NewSignatureVerifier: %v", err)
	}

	body := []byte(`{"id":"9f1a2b3c","status":"delivered","recipient":"+989120000000"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	header := signaturePrefix + hex.EncodeToString(mac.Sum(nil))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !verifier.Verify(body, header) {
			b.Fatal("signature rejected")
		}
	}
}

// BenchmarkLive_Status measures real round trips against a configured deployment, which
// is the only way to see the effect of keep-alive on production latency. It is opt-in:
//
//	NOTIFIER_API_KEY=... go test -run XXX -bench BenchmarkLive -benchtime 20x ./...
func BenchmarkLive_Status(b *testing.B) {
	apiKey := os.Getenv("NOTIFIER_API_KEY")
	if apiKey == "" {
		b.Skip("set NOTIFIER_API_KEY to benchmark against a live deployment")
	}

	opts := []Option{}
	if base := os.Getenv("NOTIFIER_BASE_URL"); base != "" {
		opts = append(opts, WithBaseURL(base))
	}
	client, err := NewClient(apiKey, opts...)
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	page, err := client.History(ctx, HistoryQuery{Limit: 1})
	if err != nil {
		b.Fatalf("History: %v", err)
	}
	if len(page.Items) == 0 {
		b.Skip("the tenant has no notifications to read")
	}
	id := page.Items[0].ID

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.Status(ctx, id); err != nil {
			b.Fatalf("Status: %v", err)
		}
	}
}
