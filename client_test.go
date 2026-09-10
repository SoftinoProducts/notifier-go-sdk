package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "test-api-key"

// captured records what the SDK actually put on the wire.
type captured struct {
	mu     sync.Mutex
	method string
	target string // path + raw query
	header http.Header
	body   []byte
	calls  int
}

func (c *captured) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.target = r.URL.RequestURI()
	c.header = r.Header.Clone()
	c.body = body
	c.calls++
}

func (c *captured) snapshot() (method, target string, header http.Header, body []byte, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.target, c.header, c.body, c.calls
}

// jsonBody decodes the captured request body into a map for assertions.
func (c *captured) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	_, _, _, body, _ := c.snapshot()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("request body is not a JSON object: %v (body=%q)", err, body)
	}
	return out
}

// newTestClient returns a client wired to a test server running handler.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := NewClient(testAPIKey, WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// respondWith builds a handler that records the request and replies with a fixed body.
func respondWith(cap *captured, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.record(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// notificationJSON is a representative status-endpoint response.
const notificationJSON = `{
	"id": "9f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
	"status": "delivered",
	"channel_type": "sms",
	"recipient": "+989120000000",
	"body": "Your code is 123456",
	"provider": "kavenegar",
	"provider_message_id": "1234567890",
	"created_at": "2026-09-10T10:00:00Z",
	"updated_at": "2026-09-10T10:00:05Z"
}`

func TestNewClient_ValidatesAPIKey(t *testing.T) {
	for name, key := range map[string]string{"empty": "", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(key); err == nil {
				t.Fatal("expected an error for a blank API key")
			}
		})
	}
}

func TestNewClient_ValidatesBaseURL(t *testing.T) {
	tests := map[string]string{
		"empty":        "",
		"no scheme":    "notifier-api.vibe.ir",
		"bad scheme":   "ftp://notifier-api.vibe.ir",
		"no host":      "https://",
		"control char": "https://notifier\x7f",
		"unparseable":  "https://notifier%zz",
	}
	for name, baseURL := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient("key", WithBaseURL(baseURL)); err == nil {
				t.Fatalf("expected an error for base URL %q", baseURL)
			}
		})
	}
}

func TestNewClient_NormalizesBaseURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://notifier-api.vibe.ir/", "https://notifier-api.vibe.ir"},
		{" https://notifier-api.vibe.ir ", "https://notifier-api.vibe.ir"},
		{"http://localhost:8080///", "http://localhost:8080"},
		{"https://host/api", "https://host/api"},
	}
	for _, tc := range tests {
		client, err := NewClient("key", WithBaseURL(tc.in))
		if err != nil {
			t.Fatalf("NewClient(%q): %v", tc.in, err)
		}
		if got := client.BaseURL(); got != tc.want {
			t.Errorf("BaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewClient_Defaults(t *testing.T) {
	client, err := NewClient("key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.BaseURL() != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", client.BaseURL(), DefaultBaseURL)
	}
	if client.http.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", client.http.Timeout, DefaultTimeout)
	}
	if client.http.Transport != defaultTransport() {
		t.Error("expected the shared default transport to be reused")
	}
	if !strings.HasPrefix(client.userAgent, "notifier-go-sdk/"+Version) {
		t.Errorf("User-Agent = %q, want the notifier-go-sdk prefix", client.userAgent)
	}
}

func TestNewClient_WithTimeoutOnCustomClientDoesNotMutateIt(t *testing.T) {
	original := &http.Client{Timeout: 0}
	client, err := NewClient("key", WithHTTPClient(original), WithTimeout(3*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.http.Timeout != 3*time.Second {
		t.Errorf("client timeout = %v, want 3s", client.http.Timeout)
	}
	if original.Timeout != 0 {
		t.Errorf("the caller's client was mutated: Timeout = %v, want 0", original.Timeout)
	}
	if client.http == original {
		t.Error("expected a copy of the caller's client, not the same pointer")
	}
}

func TestNewClient_WithHTTPClientIsUsed(t *testing.T) {
	cap := &captured{}
	srv := httptest.NewServer(respondWith(cap, http.StatusAccepted, `{"id":"1","status":"queued"}`))
	defer srv.Close()

	// A client with a custom RoundTripper proves the option reaches the transport.
	var seen bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = true
		return http.DefaultTransport.RoundTrip(r)
	})
	client, err := NewClient("key", WithBaseURL(srv.URL), WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.SendText(context.Background(), ChannelSMS, "+98912", "hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if !seen {
		t.Error("the supplied RoundTripper was not used")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// urlParseQuery extracts the query parameters from a recorded request target.
func urlParseQuery(target string) (url.Values, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return u.Query(), nil
}

func TestClient_SetsStandardHeaders(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, `{"id":"1","status":"queued"}`))

	if _, err := client.SendText(context.Background(), ChannelSMS, "+98912", "hi"); err != nil {
		t.Fatalf("SendText: %v", err)
	}

	method, target, header, _, _ := cap.snapshot()
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if target != "/v1/notifications" {
		t.Errorf("target = %s, want /v1/notifications", target)
	}
	if got := header.Get("X-API-Key"); got != testAPIKey {
		t.Errorf("X-API-Key = %q, want %q", got, testAPIKey)
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if got := header.Get("User-Agent"); !strings.HasPrefix(got, "notifier-go-sdk/") {
		t.Errorf("User-Agent = %q, want the notifier-go-sdk prefix", got)
	}
}

func TestClient_WithUserAgent(t *testing.T) {
	cap := &captured{}
	srv := httptest.NewServer(respondWith(cap, http.StatusOK, `{}`))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL), WithUserAgent("trade-hub/2.1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.History(context.Background(), HistoryQuery{}); err != nil {
		t.Fatalf("History: %v", err)
	}
	_, _, header, _, _ := cap.snapshot()
	if got := header.Get("User-Agent"); got != "trade-hub/2.1" {
		t.Errorf("User-Agent = %q, want trade-hub/2.1", got)
	}
}

func TestClient_RespectsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err = client.Status(ctx, "some-id")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Status error = %v, want context.Canceled", err)
	}
}

func TestClient_HonoursContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := client.Status(ctx, "id"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Status error = %v, want context.DeadlineExceeded", err)
	}
}

// TestClient_ConcurrentUse exercises the client the way production does: one shared
// client, many goroutines. Run with -race to make this meaningful.
func TestClient_ConcurrentUse(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"1","status":"queued"}`)
	}))
	defer srv.Close()

	client, err := NewClient("key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = client.Send(context.Background(), SendParams{
				Channel:   ChannelSMS,
				Recipient: "+989120000000",
				Body:      "hi",
				Metadata:  json.RawMessage(`{"worker":1}`),
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != workers {
		t.Errorf("server saw %d calls, want %d", calls, workers)
	}
}

func TestSplitGroupTemplate(t *testing.T) {
	tests := []struct {
		in        string
		wantGroup string
		wantTmpl  string
	}{
		{"verify", "", "verify"},
		{"vibe:salerequest", "vibe", "salerequest"},
		{"a:b:c", "a", "b:c"},
		{":verify", "", ":verify"},
		{"group:", "", "group:"},
		{"", "", ""},
		{"verify:", "", "verify:"},
	}
	for _, tc := range tests {
		group, tmpl := splitGroupTemplate(tc.in)
		if group != tc.wantGroup || tmpl != tc.wantTmpl {
			t.Errorf("splitGroupTemplate(%q) = (%q, %q), want (%q, %q)",
				tc.in, group, tmpl, tc.wantGroup, tc.wantTmpl)
		}
	}
}

func TestJSON_HelperRejectsUnsupportedValues(t *testing.T) {
	if _, err := JSON(make(chan int)); err == nil {
		t.Error("expected an error for an unmarshalable value")
	}
	raw, err := JSON(map[string]int{"order_id": 42})
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if string(raw) != `{"order_id":42}` {
		t.Errorf("JSON = %s, want {\"order_id\":42}", raw)
	}
	if raw, err := JSON(nil); err != nil || raw != nil {
		t.Errorf("JSON(nil) = (%v, %v), want (nil, nil)", raw, err)
	}
}
