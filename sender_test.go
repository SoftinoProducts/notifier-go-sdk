package notifier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSender(t *testing.T, client *Client, opts SenderOptions) *Sender {
	t.Helper()
	sender, err := NewSender(client, opts)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sender.Close(ctx)
	})
	return sender
}

func waitResult(t *testing.T, ch <-chan SendResult) SendResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SendResult")
		return SendResult{}
	}
}

func TestSender_HeadersPriorityAndGeneratedKey(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, err := NewClient(testAPIKey, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	sender := newSender(t, client, SenderOptions{HighWorkers: 1, BulkWorkers: 1})

	high, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel:        ChannelSMS,
		Recipient:      "+989120000000",
		Body:           "hi",
		Priority:       PriorityHigh,
		IdempotencyKey: "otp:sale-1",
	})
	if err != nil {
		t.Fatalf("EnqueueResult: %v", err)
	}
	res := waitResult(t, high)
	if res.Err != nil {
		t.Fatalf("high send: %v", res.Err)
	}
	if res.IdempotencyKey != "otp:sale-1" {
		t.Errorf("result key = %q, want caller key", res.IdempotencyKey)
	}

	bulk, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000001", Body: "bulk",
	})
	if err != nil {
		t.Fatalf("EnqueueResult bulk: %v", err)
	}
	bulkRes := waitResult(t, bulk)
	if bulkRes.Err != nil {
		t.Fatalf("bulk send: %v", bulkRes.Err)
	}
	if bulkRes.IdempotencyKey == "" {
		t.Fatal("Sender must generate an idempotency key when none is supplied")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 2 {
		t.Fatalf("requests = %d, want 2", len(headers))
	}
	if got := headers[0].Get("X-Priority"); got != string(PriorityHigh) {
		t.Errorf("high X-Priority = %q", got)
	}
	if got := headers[0].Get("Idempotency-Key"); got != "otp:sale-1" {
		t.Errorf("high Idempotency-Key = %q", got)
	}
	if !strings.Contains(bodies[0], `"idempotency_key":"otp:sale-1"`) {
		t.Errorf("body missing idempotency_key: %s", bodies[0])
	}
	if got := headers[1].Get("X-Priority"); got != string(PriorityBulk) {
		t.Errorf("bulk X-Priority = %q", got)
	}
	if got := headers[1].Get("Idempotency-Key"); got != bulkRes.IdempotencyKey {
		t.Errorf("bulk header key = %q, result key = %q", got, bulkRes.IdempotencyKey)
	}
}

func TestSender_QueueFullIsTypedAndDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()
	defer close(release)

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{
		HighWorkers: 1, BulkWorkers: 1,
		HighQueueSize: 1, BulkQueueSize: 1,
		SendTimeout: 2 * time.Second,
	})

	if _, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "one", Priority: PriorityHigh,
	}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start the first request")
	}
	if _, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000001", Body: "two", Priority: PriorityHigh,
	}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	var full error
	for i := 0; i < 10 && full == nil; i++ {
		_, err := sender.EnqueueResult(context.Background(), SendParams{
			Channel: ChannelSMS, Recipient: "+989120000002", Body: "three", Priority: PriorityHigh,
		})
		if err != nil {
			full = err
		}
	}
	if !errors.Is(full, ErrQueueFull) {
		t.Fatalf("queue full error = %v, want ErrQueueFull", full)
	}
	var qerr *QueueFullError
	if !errors.As(full, &qerr) {
		t.Fatalf("error = %T, want *QueueFullError", full)
	}
	if qerr.Priority != PriorityHigh || qerr.Capacity != 1 || qerr.RetryAfter <= 0 {
		t.Errorf("QueueFullError = %+v", qerr)
	}
	if got := sender.Stats().Rejected; got != 1 {
		t.Errorf("Rejected = %d, want 1", got)
	}
}

func TestSender_HighPriorityIsNotStarvedByBulk(t *testing.T) {
	bulkStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Priority") == string(PriorityBulk) {
			select {
			case bulkStarted <- struct{}{}:
			default:
			}
			time.Sleep(300 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{
		HighWorkers: 1, BulkWorkers: 1,
		HighQueueSize: 10, BulkQueueSize: 10,
	})

	bulk, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "bulk",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-bulkStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk request did not start")
	}
	start := time.Now()
	high, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000001", Body: "otp", Priority: PriorityHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-high:
		if res.Err != nil {
			t.Fatalf("high send: %v", res.Err)
		}
		if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
			t.Errorf("high send waited %s behind bulk", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("high send was starved by bulk work")
	}
	waitResult(t, bulk)
}

func TestSender_Retries429HonouringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	listener := &recordingListener{}
	sender := newSender(t, client, SenderOptions{
		MaxRetries: 1, RetryBaseDelay: 1 * time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
		Listener: listener,
	})
	ch, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := waitResult(t, ch)
	if res.Err != nil {
		t.Fatalf("send: %v", res.Err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
	if got := sender.Stats().Retries; got != 1 {
		t.Errorf("Retries = %d, want 1", got)
	}
	events := listener.snapshot()
	if events.accepted != 1 || events.completed != 1 || events.retries != 1 {
		t.Errorf("listener events = %+v, want accepted=1 completed=1 retries=1", events)
	}
}

func TestSender_DoesNotRetryAmbiguousWithoutOptIn(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{MaxRetries: 2, RetryBaseDelay: time.Millisecond})
	ch, _ := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	})
	res := waitResult(t, ch)
	if res.Err == nil {
		t.Fatal("expected a 503 failure")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 (no ambiguous retry)", got)
	}
	if got := sender.Stats().Retries; got != 0 {
		t.Errorf("Retries = %d, want 0", got)
	}
}

func TestSender_RetriesAmbiguousWithOptInAndSameKey(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{
		MaxRetries: 1, RetryAmbiguous: true,
		RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	})
	ch, _ := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi", IdempotencyKey: "stable-key",
	})
	res := waitResult(t, ch)
	if res.Err != nil {
		t.Fatalf("send: %v", res.Err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != "stable-key" || keys[1] != "stable-key" {
		t.Errorf("idempotency keys = %v, want the same key twice", keys)
	}
}

func TestSender_RetriesPreSendTransportFailures(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	client, err := NewClient(testAPIKey, WithBaseURL("http://"+addr))
	if err != nil {
		t.Fatal(err)
	}
	sender := newSender(t, client, SenderOptions{
		MaxRetries: 2, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	})
	ch, _ := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	})
	res := waitResult(t, ch)
	if res.Err == nil {
		t.Fatal("expected a connection failure")
	}
	if !IsPreSendError(res.Err) {
		t.Errorf("error %v should classify as a safe pre-send failure", res.Err)
	}
	if got := sender.Stats().Retries; got != 2 {
		t.Errorf("Retries = %d, want 2", got)
	}
}

func TestSender_CloseDrainsAcceptedResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{BulkWorkers: 2, BulkQueueSize: 100})
	var chans []<-chan SendResult
	for i := 0; i < 20; i++ {
		ch, err := sender.EnqueueResult(context.Background(), SendParams{
			Channel: ChannelSMS, Recipient: fmt.Sprintf("+9891200%05d", i), Body: "hi",
		})
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	if err := sender.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, ch := range chans {
		res := waitResult(t, ch)
		if res.Err != nil {
			t.Errorf("result after drain: %v", res.Err)
		}
	}
	stats := sender.Stats()
	if stats.Accepted != 20 || stats.Completed != 20 || stats.Failed != 0 {
		t.Errorf("stats after drain = %+v", stats)
	}
	if err := sender.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if err := sender.Enqueue(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "after close",
	}); !errors.Is(err, ErrSenderClosed) {
		t.Errorf("Enqueue after close = %v, want ErrSenderClosed", err)
	}
	if got := sender.Stats().Rejected; got != 1 {
		t.Errorf("Rejected after closed enqueue = %d, want 1", got)
	}
}

func TestSender_CloseTimeoutFailsRemainingResults(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender, err := NewSender(client, SenderOptions{
		HighWorkers: 1, BulkWorkers: 1,
		HighQueueSize: 10, BulkQueueSize: 10,
		SendTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	var chans []<-chan SendResult
	for i := 0; i < 5; i++ {
		ch, err := sender.EnqueueResult(context.Background(), SendParams{
			Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi", Priority: PriorityHigh,
		})
		if err != nil {
			t.Fatal(err)
		}
		chans = append(chans, ch)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sender.Close(ctx); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("Close = %v, want ErrDrainTimeout", err)
	}
	close(release)
	for _, ch := range chans {
		res := waitResult(t, ch)
		if res.Err == nil {
			t.Error("expected a closed/timeout error, got success")
		}
	}
	stats := sender.Stats()
	if stats.Accepted != 5 || stats.Completed+stats.Failed != 5 {
		t.Errorf("stats after forced close = %+v", stats)
	}
}

func TestSender_TenThousandEnqueuesStayBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	base := runtime.NumGoroutine()
	sender, err := NewSender(client, SenderOptions{
		BulkWorkers: 4, BulkQueueSize: 20_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10_000; i++ {
		if err := sender.Enqueue(context.Background(), SendParams{
			Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
		}); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if got := sender.Stats().Accepted; got != 10_000 {
		t.Errorf("Accepted = %d, want 10000", got)
	}
	if got := runtime.NumGoroutine(); got > base+200 {
		t.Errorf("goroutines = %d (base %d); the Sender created per-message goroutines", got, base)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sender.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stats := sender.Stats()
	if stats.Accepted != 10_000 || stats.Completed+stats.Failed != 10_000 {
		t.Errorf("stats = %+v", stats)
	}
}

type recordingListener struct {
	mu          sync.Mutex
	accepted    int
	completed   int
	retries     int
	rejected    int
	dropped     int
	lastOutcome SendOutcome
}

func (l *recordingListener) OnAccepted(SendEvent) {
	l.mu.Lock()
	l.accepted++
	l.mu.Unlock()
}

func (l *recordingListener) OnCompleted(_ SendEvent, outcome SendOutcome, _ time.Duration, _ error) {
	l.mu.Lock()
	l.completed++
	l.lastOutcome = outcome
	l.mu.Unlock()
}

func (l *recordingListener) OnRetry(SendEvent, int, time.Duration, error) {
	l.mu.Lock()
	l.retries++
	l.mu.Unlock()
}

func (l *recordingListener) OnRejected(SendEvent, error) {
	l.mu.Lock()
	l.rejected++
	l.mu.Unlock()
}

func (l *recordingListener) OnDropped(SendEvent, error) {
	l.mu.Lock()
	l.dropped++
	l.mu.Unlock()
}

type listenerSnapshot struct {
	accepted    int
	completed   int
	retries     int
	rejected    int
	dropped     int
	lastOutcome SendOutcome
}

func (l *recordingListener) snapshot() listenerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return listenerSnapshot{
		accepted: l.accepted, completed: l.completed, retries: l.retries,
		rejected: l.rejected, dropped: l.dropped, lastOutcome: l.lastOutcome,
	}
}

func TestSender_FireAndForgetFailureIsDroppedAndObserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	listener := &recordingListener{}
	sender := newSender(t, client, SenderOptions{Listener: listener, MaxRetries: -1})

	if err := sender.Enqueue(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stats := sender.Stats()
		if stats.Failed == 1 && stats.Dropped == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stats := sender.Stats()
	if stats.Failed != 1 || stats.Dropped != 1 {
		t.Errorf("stats = %+v, want failed=1 dropped=1", stats)
	}
	events := listener.snapshot()
	if events.accepted != 1 || events.completed != 1 || events.dropped != 1 {
		t.Errorf("listener = %+v", events)
	}
	if events.lastOutcome != OutcomeDropped {
		t.Errorf("outcome = %q, want dropped", events.lastOutcome)
	}
}

func TestSender_EnqueueRejectsInvalidParamsBeforeQueueing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{})
	err := sender.Enqueue(context.Background(), SendParams{Channel: ChannelSMS, Recipient: "+989120000000"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid params reached the server")
	}
}

func TestSender_ListenerPanicIsContained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(notificationJSON))
	}))
	defer srv.Close()

	client, _ := NewClient(testAPIKey, WithBaseURL(srv.URL))
	sender := newSender(t, client, SenderOptions{
		Listener: panicListener{},
	})
	ch, err := sender.EnqueueResult(context.Background(), SendParams{
		Channel: ChannelSMS, Recipient: "+989120000000", Body: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := waitResult(t, ch); res.Err != nil {
		t.Fatalf("send failed because the listener panicked: %v", res.Err)
	}
}

type panicListener struct{}

func (panicListener) OnAccepted(SendEvent) {}

func (panicListener) OnCompleted(SendEvent, SendOutcome, time.Duration, error) { panic("listener") }

func (panicListener) OnRetry(SendEvent, int, time.Duration, error) { panic("listener") }

func (panicListener) OnRejected(SendEvent, error) { panic("listener") }

func (panicListener) OnDropped(SendEvent, error) { panic("listener") }
