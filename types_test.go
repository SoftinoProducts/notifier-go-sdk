package notifier

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStatus_IsTerminal(t *testing.T) {
	tests := map[Status]bool{
		StatusQueued:      false,
		StatusProcessing:  false,
		StatusDelivered:   true,
		StatusFailed:      true,
		Status("unknown"): false,
	}
	for status, want := range tests {
		if got := status.IsTerminal(); got != want {
			t.Errorf("Status(%q).IsTerminal() = %v, want %v", status, got, want)
		}
	}
}

func TestNotification_StatusHelpers(t *testing.T) {
	tests := []struct {
		status    Status
		delivered bool
		failed    bool
		pending   bool
	}{
		{StatusQueued, false, false, true},
		{StatusProcessing, false, false, true},
		{StatusDelivered, true, false, false},
		{StatusFailed, false, true, false},
	}
	for _, tc := range tests {
		n := &Notification{Status: tc.status}
		if n.IsDelivered() != tc.delivered {
			t.Errorf("%q: IsDelivered() = %v, want %v", tc.status, n.IsDelivered(), tc.delivered)
		}
		if n.IsFailed() != tc.failed {
			t.Errorf("%q: IsFailed() = %v, want %v", tc.status, n.IsFailed(), tc.failed)
		}
		if n.IsPending() != tc.pending {
			t.Errorf("%q: IsPending() = %v, want %v", tc.status, n.IsPending(), tc.pending)
		}
	}
}

func TestNotification_FailureReason(t *testing.T) {
	n := &Notification{Status: StatusFailed, Error: "kavenegar: 411 missing parameters"}
	if got := n.FailureReason(); got != "kavenegar: 411 missing parameters" {
		t.Errorf("FailureReason() = %q", got)
	}
	if got := (&Notification{}).FailureReason(); got != "" {
		t.Errorf("FailureReason() on a successful notification = %q, want empty", got)
	}
}

func TestHistoryPage_HasMore(t *testing.T) {
	if (&HistoryPage{}).HasMore() {
		t.Error("a page without a cursor must not report more")
	}
	if !(&HistoryPage{NextCursor: "abc"}).HasMore() {
		t.Error("a page with a cursor must report more")
	}
}

func TestBulkChunkError_DescribesTheFailingRange(t *testing.T) {
	cause := errors.New("boom")
	err := &bulkChunkError{offset: 20000, size: 5000, err: cause}

	if !errors.Is(err, cause) {
		t.Error("the chunk error must unwrap to its cause")
	}
	got := err.Error()
	for _, want := range []string{"20000", "5000", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to mention %q", got, want)
		}
	}
}

func TestBulkResult_ErrUnwrapsToTheChunkCause(t *testing.T) {
	result := &BulkResult{
		Failures: []BulkFailure{
			{Offset: 0, Size: 2, Err: errors.New("first")},
			{Offset: 2, Size: 2, Err: &APIError{StatusCode: 503, Message: "no channel available"}},
		},
	}
	err := result.Err()
	if err == nil {
		t.Fatal("expected a joined error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("errors.Is(err, ErrUnavailable) = false for %v", err)
	}
	if !strings.Contains(err.Error(), "offset 0") || !strings.Contains(err.Error(), "offset 2") {
		t.Errorf("joined error should name both chunks: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Error("errors.As should reach the underlying *APIError")
	}
}

func TestHistoryQuery_ValuesFormatsTimesAsRFC3339(t *testing.T) {
	loc := time.FixedZone("+03:30", 3*3600+1800)
	query := HistoryQuery{
		From: time.Date(2026, 9, 10, 8, 0, 0, 0, loc),
		To:   time.Date(2026, 9, 11, 8, 0, 0, 0, loc),
	}
	values := query.values()
	if got := values.Get("from"); got != "2026-09-10T04:30:00Z" {
		t.Errorf("from = %q, want the instant in UTC", got)
	}
	if got := values.Get("to"); got != "2026-09-11T04:30:00Z" {
		t.Errorf("to = %q, want the instant in UTC", got)
	}
}
