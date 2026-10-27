package notifier

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxBulkMessages is the largest number of messages the service accepts in one bulk
	// request. Client.Bulk and Client.BulkConcurrent split larger slices automatically.
	MaxBulkMessages = 10000

	// DefaultBulkConcurrency is the number of chunk requests Client.BulkConcurrent keeps
	// in flight when BulkOptions.Concurrency is not set.
	DefaultBulkConcurrency = 4
)

// ChannelType identifies the medium a notification travels over. The service resolves the
// concrete provider (Kavenegar, SMS.ir, SMTP, Centrifugo, ...) from the tenant's channel
// configuration, so callers never name a provider.
type ChannelType string

// The channel types implemented by the service. A tenant can also define its own, so any
// non-empty value is accepted; these constants cover the built-in ones.
const (
	ChannelSMS      ChannelType = "sms"
	ChannelEmail    ChannelType = "email"
	ChannelWebPush  ChannelType = "webpush"
	ChannelTelegram ChannelType = "telegram"
	ChannelBale     ChannelType = "bale"
)

// Status is the coarse lifecycle state of a notification.
type Status string

// The notification states reported by the service.
const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusDelivered  Status = "delivered"
	StatusFailed     Status = "failed"
)

// IsTerminal reports whether the notification has reached a final state and will not
// change again.
func (s Status) IsTerminal() bool {
	return s == StatusDelivered || s == StatusFailed
}

// Notification is a notification record as returned by Send and the status endpoints.
//
// Fields populated only by the status endpoints (Provider, ProviderMessageID, SentAt,
// Error) are empty on the synchronous response of a send, because the service is
// asynchronous: it acknowledges the send before any provider has been contacted.
type Notification struct {
	ID        string      `json:"id"`
	Status    Status      `json:"status"`
	Channel   ChannelType `json:"channel_type"`
	Recipient string      `json:"recipient"`
	Subject   string      `json:"subject,omitempty"`
	Body      string      `json:"body"`
	SendAt    *time.Time  `json:"send_at,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`

	// Routing context, resolved at send time.
	ChannelID      string          `json:"channel_id,omitempty"`
	GroupID        string          `json:"group_id,omitempty"`
	TemplateID     string          `json:"template_id,omitempty"`
	BatchID        string          `json:"batch_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CallbackURL    string          `json:"callback_url,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`

	// Human-readable routing context, resolved best-effort when reading back.
	ChannelProvider string `json:"channel_provider,omitempty"`
	GroupName       string `json:"group_name,omitempty"`

	// Provider delivery detail, populated when a delivery log exists.
	Provider          string     `json:"provider,omitempty"`
	ProviderMessageID string     `json:"provider_message_id,omitempty"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	Error             string     `json:"error,omitempty"`

	// Simulated reports that this delivery was rehearsed rather than attempted: the provider
	// was never contacted and the message was not sent.
	Simulated bool `json:"simulated,omitempty"`

	// FanoutID and FanoutStatus are present on the primary leg of a route send.
	FanoutID     string `json:"fanout_id,omitempty"`
	FanoutStatus string `json:"fanout_status,omitempty"`
}

// IsDelivered reports whether the notification reached the recipient.
func (n *Notification) IsDelivered() bool { return n.Status == StatusDelivered }

// IsFailed reports whether delivery failed. FailureReason explains why.
func (n *Notification) IsFailed() bool { return n.Status == StatusFailed }

// IsPending reports whether the notification is still queued or being processed.
func (n *Notification) IsPending() bool {
	return n.Status == StatusQueued || n.Status == StatusProcessing
}

// FailureReason returns the service's error detail, or "" when there was none. It is
// populated for failed notifications that reached a provider, and for routing failures.
func (n *Notification) FailureReason() string { return n.Error }

// SendParams is a notification to send. Channel and Recipient are required, and one of
// Body, TemplateID or TemplateName must identify the content.
//
// A plain message sets Body. A templated message sets TemplateName (or TemplateID) plus
// either Vars (values substituted by placeholder name) or Values (values bound
// positionally to the template's declared param_names).
type SendParams struct {
	// Channel is the channel type to send over, for example ChannelSMS.
	Channel ChannelType
	// Recipient is the destination: a phone number, email address, chat id, ...
	Recipient string
	// Subject is optional; used by channels that support one, such as email.
	Subject string
	// Body is the message text for a non-templated send.
	Body string

	// TemplateName selects a template by name, resolved server-side against the tenant's
	// templates for the channel and locale. It may be written "group:template" to also
	// route through a channel group by name, or "route:name" to send through a saved
	// fan-out route. A route defines its own channels, so Channel is ignored for it.
	TemplateName string
	// TemplateID selects a template by UUID, for callers that already store it.
	TemplateID string
	// Vars are template values addressed by placeholder name.
	Vars map[string]any
	// Values are template values bound positionally to the template's declared
	// param_names, in order.
	Values []string

	// ChannelID pins the send to one configured channel instance instead of letting the
	// service pick one.
	ChannelID string
	// GroupID routes the send through a channel group by UUID.
	GroupID string
	// Group routes the send through a channel group by name. It takes precedence over a
	// "group:template" prefix in TemplateName.
	Group string
	// Route sends through a saved fan-out route by name. It takes precedence over a
	// "route:name" prefix in TemplateName. The route defines every channel, so the
	// Channel field is ignored when Route is set.
	RouteName string

	// Locale selects which localized variant of a template to render. Empty means the
	// service default.
	Locale string
	// SendAt schedules the send instead of dispatching it immediately.
	SendAt *time.Time
	// CallbackURL receives the HMAC-signed delivery callback for this notification.
	CallbackURL string
	// IdempotencyKey makes the send safe to repeat. A route replay returns the existing
	// fanout instead of creating a second one, so a retry cannot deliver twice.
	IdempotencyKey string
	// Metadata is arbitrary JSON stored with the notification and echoed back when it is
	// read. Use JSON to build it from a Go value.
	Metadata json.RawMessage

	// Simulated rehearses the send instead of delivering it: the service runs its whole
	// pipeline — routing, group selection, template rendering, provider-template mapping — but
	// never contacts the provider. The notification is reported as delivered by "simulator", so
	// it shows up in the panel like any other send. Use it to exercise an integration without
	// touching templates, groups or channel configuration. Notification.Simulated reports the
	// delivery as the service recorded it.
	Simulated bool
}

// TemplateParams describes a send that uses a template by name, which is the recommended
// way to send: the service renders the template, so the caller never builds message text
// and localization stays server-side.
//
//	client.SendTemplateByName(ctx, notifier.TemplateParams{
//		Channel:   notifier.ChannelSMS,
//		Recipient: phone,
//		Template:  "verify",          // or "sms-group:verify" to route via a group
//		Values:    []string{token},   // bound to the template's param_names in order
//	})
type TemplateParams struct {
	// Channel is the channel type to send over.
	Channel ChannelType
	// Recipient is the destination address.
	Recipient string
	// Template is the template name, optionally prefixed with a channel group name as
	// "group:template" or addressed as a saved route with "route:name".
	Template string
	// Values are bound positionally to the template's declared param_names.
	Values []string
	// Vars are template values addressed by placeholder name. Use this instead of Values
	// when the template's placeholder names are known and param_names is not declared.
	Vars map[string]any
	// Group routes through a channel group by name, overriding a "group:template" prefix.
	Group string

	// Locale selects a localized template variant; empty means the service default.
	Locale string
	// SendAt schedules the send instead of dispatching it immediately.
	SendAt *time.Time
	// CallbackURL receives the HMAC-signed delivery callback for this notification.
	CallbackURL string
	// IdempotencyKey makes the send safe to repeat.
	IdempotencyKey string
	// Metadata is arbitrary JSON stored alongside the notification.
	Metadata json.RawMessage

	// Simulated rehearses the send without dispatching it. See SendParams.Simulated.
	Simulated bool
}

// BulkMessage is one recipient inside a bulk send.
type BulkMessage struct {
	// Recipient is the destination address; required.
	Recipient string
	// Subject is optional message subject.
	Subject string
	// Body is the message text for a non-templated message.
	Body string
	// TemplateName selects a template by name, optionally "group:template".
	TemplateName string
	// TemplateID selects a template by UUID.
	TemplateID string
	// Values are bound positionally to the template's declared param_names.
	Values []string
	// Vars are template values addressed by placeholder name.
	Vars map[string]any
	// Group routes this message through a channel group by name.
	Group string
	// GroupID routes this message through a channel group by UUID.
	GroupID string
	// CallbackURL receives the delivery callback for this message.
	CallbackURL string
	// Metadata is arbitrary JSON stored with the notification.
	Metadata json.RawMessage
	// Simulated overrides the batch-level flag for this message. Leave nil to inherit it.
	Simulated *bool
}

// BulkParams is a bulk send: one channel type and many recipients.
type BulkParams struct {
	// Channel is the channel type shared by every message.
	Channel ChannelType
	// Messages are the recipients. At most MaxBulkMessages are sent per request; larger
	// slices are chunked automatically.
	Messages []BulkMessage
	// Simulated rehearses every message in the batch without dispatching it, unless a message
	// overrides it. See SendParams.Simulated.
	Simulated bool
}

// Simulate returns a pointer to b, for overriding BulkParams.Simulated on a single message.
//
//	client.Bulk(ctx, notifier.BulkParams{
//		Channel:   notifier.ChannelSMS,
//		Simulated: true,
//		Messages: []notifier.BulkMessage{
//			{Recipient: phone, Body: "hi"},                       // simulated
//			{Recipient: other, Body: "real", Simulated: notifier.Simulate(false)}, // really sent
//		},
//	})
func Simulate(b bool) *bool { return &b }

// BulkOptions tunes how BulkConcurrent splits and schedules the work.
type BulkOptions struct {
	// ChunkSize is the number of messages per request. Zero, negative or larger than
	// MaxBulkMessages means MaxBulkMessages.
	ChunkSize int
	// Concurrency is how many chunk requests may be in flight at once. Zero or negative
	// means DefaultBulkConcurrency.
	Concurrency int
}

// BulkBatch is the service's acknowledgement of one accepted chunk request.
type BulkBatch struct {
	// BatchID groups the messages of this request in the service.
	BatchID string
	// Count is how many messages the service accepted.
	Count int
	// Offset is the index of the first message of this chunk in the input slice.
	Offset int
}

// BulkFailure records a chunk request that failed. The messages in that chunk, and only
// those, were not accepted.
type BulkFailure struct {
	// Offset is the index of the first message of the failed chunk in the input slice.
	Offset int
	// Size is the number of messages in the failed chunk.
	Size int
	// Err is the underlying error, carrying *APIError for HTTP failures.
	Err error
}

// BulkResult is the outcome of a bulk send. A partial result is normal: the service
// acknowledges each chunk separately, so some chunks can be accepted while others fail.
type BulkResult struct {
	// Batches are the accepted chunks, ordered as they were sent.
	Batches []BulkBatch
	// Total is the number of messages accepted across all batches.
	Total int
	// Failures are the chunks that were rejected. Empty on full success.
	Failures []BulkFailure
}

// OK reports whether every message was accepted.
func (r *BulkResult) OK() bool { return len(r.Failures) == 0 }

// Err returns a single error joining every chunk failure, or nil when all chunks were
// accepted. It is the same value Bulk and BulkConcurrent return alongside the result.
// Match an individual chunk's cause with errors.Is/errors.As through the join.
func (r *BulkResult) Err() error {
	if len(r.Failures) == 0 {
		return nil
	}
	errs := make([]error, 0, len(r.Failures))
	for i := range r.Failures {
		f := &r.Failures[i]
		errs = append(errs, &bulkChunkError{offset: f.Offset, size: f.Size, err: f.Err})
	}
	return errors.Join(errs...)
}

// bulkChunkError annotates a chunk failure with the range of messages it covers.
type bulkChunkError struct {
	offset int
	size   int
	err    error
}

func (e *bulkChunkError) Error() string {
	return "notifier: bulk chunk at offset " + strconv.Itoa(e.offset) +
		" (" + strconv.Itoa(e.size) + " messages): " + e.err.Error()
}

func (e *bulkChunkError) Unwrap() error { return e.err }

// HistoryQuery filters the notification history. Every field is optional; the zero value
// asks for the service's default page.
type HistoryQuery struct {
	// Limit is the page size. Zero means the service default.
	Limit int
	// After is the opaque cursor from the previous page's NextCursor.
	After string
	// Status filters by notification state.
	Status Status
	// Channel filters by channel type.
	Channel ChannelType
	// Recipient searches recipients, case-insensitively.
	Recipient string
	// ChannelID filters by the channel instance that handled the notification.
	ChannelID string
	// GroupID filters by channel group UUID.
	GroupID string
	// GroupName filters by channel group name.
	GroupName string
	// From is the inclusive lower bound on the notification creation time.
	From time.Time
	// To is the inclusive upper bound on the notification creation time.
	To time.Time
}

// values renders the query as URL query parameters, omitting empty fields.
func (q HistoryQuery) values() url.Values {
	const maxFields = 10
	out := make(url.Values, maxFields)
	if q.Limit > 0 {
		out.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.After != "" {
		out.Set("after", q.After)
	}
	if q.Status != "" {
		out.Set("status", string(q.Status))
	}
	if q.Channel != "" {
		out.Set("channel_type", string(q.Channel))
	}
	if q.Recipient != "" {
		out.Set("recipient", q.Recipient)
	}
	if q.ChannelID != "" {
		out.Set("channel_id", q.ChannelID)
	}
	if q.GroupID != "" {
		out.Set("group_id", q.GroupID)
	}
	if q.GroupName != "" {
		out.Set("group_name", q.GroupName)
	}
	if !q.From.IsZero() {
		out.Set("from", q.From.UTC().Format(time.RFC3339))
	}
	if !q.To.IsZero() {
		out.Set("to", q.To.UTC().Format(time.RFC3339))
	}
	return out
}

// HistoryPage is one page of notification history.
type HistoryPage struct {
	// Items are the notifications in this page, newest first.
	Items []Notification `json:"items"`
	// NextCursor fetches the next page. Empty when this is the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// HasMore reports whether another page is available.
func (p *HistoryPage) HasMore() bool { return p.NextCursor != "" }

// JSON marshals v into raw JSON for a Metadata field, returning an error instead of
// panicking on unsupported values.
//
//	md, err := notifier.JSON(map[string]any{"order_id": 42})
//	res, err := client.Send(ctx, notifier.SendParams{ /* ... */ Metadata: md})
func JSON(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// splitGroupTemplate splits a "group:template" name. A name without a usable colon is
// returned unchanged with an empty group, so plain template names pass through.
func splitGroupTemplate(name string) (group, template string) {
	if i := strings.IndexByte(name, ':'); i > 0 && i < len(name)-1 {
		return name[:i], name[i+1:]
	}
	return "", name
}
