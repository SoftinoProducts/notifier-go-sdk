package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bulkServer answers bulk requests, echoing the number of messages it received and
// recording the size of each chunk so tests can assert the split.
type bulkServer struct {
	mu sync.Mutex
	// chunks is the message count per request, in arrival order.
	chunks []int
	// failFor makes any chunk containing this recipient fail with a 500. Matching on
	// content rather than arrival order keeps the test independent of dispatch order.
	failFor  string
	inFlight int32
	maxSeen  int32
	delay    time.Duration
}

func (b *bulkServer) handler() http.HandlerFunc {
	var calls int32
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if cur := atomic.AddInt32(&b.inFlight, 1); cur > atomic.LoadInt32(&b.maxSeen) {
			atomic.StoreInt32(&b.maxSeen, cur)
		}
		defer atomic.AddInt32(&b.inFlight, -1)

		if b.delay > 0 {
			time.Sleep(b.delay)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			ChannelType string `json:"channel_type"`
			Messages    []struct {
				Recipient string `json:"recipient"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		b.mu.Lock()
		b.chunks = append(b.chunks, len(req.Messages))
		b.mu.Unlock()

		if b.failFor != "" {
			for _, m := range req.Messages {
				if m.Recipient == b.failFor {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":"internal error"}`)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"batch_id":"batch-%d","count":%d}`, n, len(req.Messages))
	}
}

func (b *bulkServer) counts() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.chunks...)
}

func newBulkClient(t *testing.T, srv *bulkServer) *Client {
	t.Helper()
	server := httptest.NewServer(srv.handler())
	t.Cleanup(server.Close)

	client, err := NewClient("key", WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// messages builds n recipients for the given channel-agnostic bulk payload.
func messages(n int) []BulkMessage {
	out := make([]BulkMessage, n)
	for i := range out {
		out[i] = BulkMessage{Recipient: fmt.Sprintf("+9891200%05d", i), Body: "hi"}
	}
	return out
}

func TestBulk_SingleChunk(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	result, err := client.Bulk(context.Background(), BulkParams{Channel: ChannelSMS, Messages: messages(3)})
	if err != nil {
		t.Fatalf("Bulk: %v", err)
	}
	if !result.OK() {
		t.Errorf("result not OK: %+v", result.Failures)
	}
	if result.Total != 3 {
		t.Errorf("Total = %d, want 3", result.Total)
	}
	if len(result.Batches) != 1 || result.Batches[0].BatchID != "batch-1" || result.Batches[0].Offset != 0 {
		t.Errorf("Batches = %+v", result.Batches)
	}
	if got := srv.counts(); len(got) != 1 || got[0] != 3 {
		t.Errorf("server received chunks %v, want [3]", got)
	}
}

// TestBulk_ChunksAboveTheServiceLimit is the real-limit test: the service rejects more
// than MaxBulkMessages per request, so the SDK must split without the caller doing it.
func TestBulk_ChunksAboveTheServiceLimit(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	const total = MaxBulkMessages + 1
	result, err := client.Bulk(context.Background(), BulkParams{Channel: ChannelSMS, Messages: messages(total)})
	if err != nil {
		t.Fatalf("Bulk: %v", err)
	}
	if result.Total != total {
		t.Errorf("Total = %d, want %d", result.Total, total)
	}
	if len(result.Batches) != 2 {
		t.Fatalf("Batches = %d, want 2", len(result.Batches))
	}
	if result.Batches[0].Offset != 0 || result.Batches[1].Offset != MaxBulkMessages {
		t.Errorf("batch offsets = %d, %d; want 0, %d",
			result.Batches[0].Offset, result.Batches[1].Offset, MaxBulkMessages)
	}
	want := []int{MaxBulkMessages, 1}
	got := srv.counts()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("server received chunks %v, want %v", got, want)
	}
}

func TestBulk_ContinuesAfterAFailedChunk(t *testing.T) {
	// With a chunk size of 2 the 5 messages split into [0,1], [2,3], [4]. Failing the
	// chunk that contains message 2 must not stop the chunk containing message 4.
	msgs := messages(5)
	srv := &bulkServer{failFor: msgs[2].Recipient}
	client := newBulkClient(t, srv)

	result, err := client.BulkConcurrent(context.Background(),
		BulkParams{Channel: ChannelSMS, Messages: msgs},
		BulkOptions{ChunkSize: 2, Concurrency: 1})
	if err == nil {
		t.Fatal("expected a partial-failure error")
	}
	if result.OK() {
		t.Fatal("result should report failures")
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly 1", result.Failures)
	}
	if result.Failures[0].Offset != 2 || result.Failures[0].Size != 2 {
		t.Errorf("failure = %+v, want offset 2 size 2", result.Failures[0])
	}
	if !errors.Is(err, ErrServer) {
		t.Errorf("error = %v, want ErrServer", err)
	}
	if len(result.Batches) != 2 || result.Total != 3 {
		t.Errorf("accepted = %d in %d batches, want 3 in 2", result.Total, len(result.Batches))
	}
}

func TestBulkConcurrent_ChunksInOrder(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	result, err := client.BulkConcurrent(context.Background(),
		BulkParams{Channel: ChannelSMS, Messages: messages(5)},
		BulkOptions{ChunkSize: 2, Concurrency: 3})
	if err != nil {
		t.Fatalf("BulkConcurrent: %v", err)
	}
	if len(result.Batches) != 3 {
		t.Fatalf("Batches = %d, want 3", len(result.Batches))
	}
	wantOffsets := []int{0, 2, 4}
	for i, want := range wantOffsets {
		if result.Batches[i].Offset != want {
			t.Errorf("batches[%d].Offset = %d, want %d", i, result.Batches[i].Offset, want)
		}
	}
	if result.Total != 5 {
		t.Errorf("Total = %d, want 5", result.Total)
	}
}

func TestBulkConcurrent_BoundsConcurrency(t *testing.T) {
	srv := &bulkServer{delay: 30 * time.Millisecond}
	client := newBulkClient(t, srv)

	const (
		chunkSize   = 1
		concurrency = 3
		total       = 12
	)
	if _, err := client.BulkConcurrent(context.Background(),
		BulkParams{Channel: ChannelSMS, Messages: messages(total)},
		BulkOptions{ChunkSize: chunkSize, Concurrency: concurrency}); err != nil {
		t.Fatalf("BulkConcurrent: %v", err)
	}

	maxSeen := atomic.LoadInt32(&srv.maxSeen)
	if maxSeen > concurrency {
		t.Errorf("max in-flight requests = %d, want at most %d", maxSeen, concurrency)
	}
	if maxSeen < 2 {
		t.Errorf("max in-flight requests = %d; the chunks were not actually parallel", maxSeen)
	}
}

func TestBulkConcurrent_DefaultsConcurrency(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	// ChunkSize 0 and Concurrency 0 must both fall back to the defaults.
	if _, err := client.BulkConcurrent(context.Background(),
		BulkParams{Channel: ChannelSMS, Messages: messages(10)}, BulkOptions{}); err != nil {
		t.Fatalf("BulkConcurrent: %v", err)
	}
	if got := srv.counts(); len(got) != 1 || got[0] != 10 {
		t.Errorf("server received chunks %v, want [10]", got)
	}
}

func TestBulkConcurrent_ChunkSizeAboveLimitIsClamped(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	_, err := client.BulkConcurrent(context.Background(),
		BulkParams{Channel: ChannelSMS, Messages: messages(4)},
		BulkOptions{ChunkSize: MaxBulkMessages * 10})
	if err != nil {
		t.Fatalf("BulkConcurrent: %v", err)
	}
	if got := srv.counts(); len(got) != 1 || got[0] != 4 {
		t.Errorf("server received chunks %v, want [4]", got)
	}
}

func TestBulkConcurrent_CancellationFailsUnstartedChunks(t *testing.T) {
	srv := &bulkServer{delay: 50 * time.Millisecond}
	client := newBulkClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	result, err := client.BulkConcurrent(ctx,
		BulkParams{Channel: ChannelSMS, Messages: messages(8)},
		BulkOptions{ChunkSize: 1, Concurrency: 1})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if result.OK() {
		t.Fatal("expected failures to be reported")
	}
	if len(result.Failures) != 8 {
		t.Errorf("Failures = %d, want all 8 chunks accounted for", len(result.Failures))
	}
	canceled := 0
	for _, f := range result.Failures {
		if errors.Is(f.Err, context.Canceled) {
			canceled++
		}
	}
	if canceled != 8 {
		t.Errorf("%d of 8 failures carry context.Canceled", canceled)
	}
}

func TestBulk_ReportsAFailedChunkAndKeepsGoing(t *testing.T) {
	// Two chunks: the first (the whole service limit) fails, the second must still be sent.
	msgs := messages(MaxBulkMessages + 1)
	srv := &bulkServer{failFor: msgs[0].Recipient}
	client := newBulkClient(t, srv)

	result, err := client.Bulk(context.Background(), BulkParams{Channel: ChannelSMS, Messages: msgs})
	if !errors.Is(err, ErrServer) {
		t.Fatalf("error = %v, want ErrServer", err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1", len(result.Failures))
	}
	if got := result.Failures[0]; got.Offset != 0 || got.Size != MaxBulkMessages {
		t.Errorf("failure = %+v, want offset 0 size %d", got, MaxBulkMessages)
	}
	if len(result.Batches) != 1 || result.Batches[0].Offset != MaxBulkMessages {
		t.Errorf("Batches = %+v, want one batch at offset %d", result.Batches, MaxBulkMessages)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want the 1 message of the surviving chunk", result.Total)
	}
	if got := srv.counts(); len(got) != 2 {
		t.Errorf("server received %v, want both chunks attempted", got)
	}
}

func TestBulk_ValidatesEveryMessageBeforeSending(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	msgs := messages(3)
	msgs[2] = BulkMessage{Recipient: "+98912"} // no body and no template

	_, err := client.Bulk(context.Background(), BulkParams{Channel: ChannelSMS, Messages: msgs})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "messages[2]") {
		t.Errorf("error should name the offending index: %v", err)
	}
	if got := srv.counts(); len(got) != 0 {
		t.Errorf("the SDK sent %v despite invalid input; validation must be complete first", got)
	}
}

func TestBulk_ValidatesInput(t *testing.T) {
	srv := &bulkServer{}
	client := newBulkClient(t, srv)

	t.Run("no channel", func(t *testing.T) {
		_, err := client.Bulk(context.Background(), BulkParams{Messages: messages(1)})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("error = %v, want ErrInvalidArgument", err)
		}
	})
	t.Run("no messages", func(t *testing.T) {
		_, err := client.Bulk(context.Background(), BulkParams{Channel: ChannelSMS})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("error = %v, want ErrInvalidArgument", err)
		}
	})
	t.Run("blank recipient", func(t *testing.T) {
		_, err := client.Bulk(context.Background(), BulkParams{
			Channel:  ChannelSMS,
			Messages: []BulkMessage{{Recipient: " ", Body: "hi"}},
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("error = %v, want ErrInvalidArgument", err)
		}
	})
	if got := srv.counts(); len(got) != 0 {
		t.Errorf("requests were sent for invalid input: %v", got)
	}
}

func TestBulkMessage_WireForm(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, `{"batch_id":"b","count":1}`))

	_, err := client.Bulk(context.Background(), BulkParams{
		Channel: ChannelSMS,
		Messages: []BulkMessage{{
			Recipient:    "+989120000000",
			TemplateName: "vibe:salerequest",
			Values:       []string{"50000"},
			CallbackURL:  "https://example.com/cb",
			Metadata:     json.RawMessage(`{"order_id":42}`),
		}},
	})
	if err != nil {
		t.Fatalf("Bulk: %v", err)
	}

	body := cap.jsonBody(t)
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["group_name"] != "vibe" || msg["template_name"] != "salerequest" {
		t.Errorf("routing = (%v, %v)", msg["group_name"], msg["template_name"])
	}
	if params, ok := msg["template_params"].([]any); !ok || len(params) != 1 || params[0] != "50000" {
		t.Errorf("template_params = %v", msg["template_params"])
	}
	if msg["callback_url"] != "https://example.com/cb" {
		t.Errorf("callback_url = %v", msg["callback_url"])
	}
	if md, ok := msg["metadata"].(map[string]any); !ok || md["order_id"] != float64(42) {
		t.Errorf("metadata = %v", msg["metadata"])
	}
	if _, present := msg["locale"]; present {
		t.Errorf("locale should be omitted when empty, got %v", msg["locale"])
	}
}

func TestPrepareBulk_ChunkBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		messages  int
		chunkSize int
		want      [][2]int // {offset, end}
	}{
		{"exact fit", 4, 2, [][2]int{{0, 2}, {2, 4}}},
		{"remainder", 5, 2, [][2]int{{0, 2}, {2, 4}, {4, 5}}},
		{"single chunk", 3, 10, [][2]int{{0, 3}}},
		{"defaults when zero", 3, 0, [][2]int{{0, 3}}},
		{"defaults when negative", 3, -1, [][2]int{{0, 3}}},
		{"defaults when above limit", 3, MaxBulkMessages + 1, [][2]int{{0, 3}}},
		{"one per chunk", 3, 1, [][2]int{{0, 1}, {1, 2}, {2, 3}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := prepareBulk(BulkParams{Channel: ChannelSMS, Messages: messages(tc.messages)}, tc.chunkSize)
			if err != nil {
				t.Fatalf("prepareBulk: %v", err)
			}
			if len(chunks) != len(tc.want) {
				t.Fatalf("chunks = %d, want %d", len(chunks), len(tc.want))
			}
			for i, want := range tc.want {
				if chunks[i].offset != want[0] || chunks[i].end != want[1] {
					t.Errorf("chunks[%d] = [%d,%d), want [%d,%d)",
						i, chunks[i].offset, chunks[i].end, want[0], want[1])
				}
			}
		})
	}
}

func TestBulkResult_ErrIsNilOnFullSuccess(t *testing.T) {
	result := &BulkResult{Batches: []BulkBatch{{BatchID: "b", Count: 2}}, Total: 2}
	if err := result.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}
