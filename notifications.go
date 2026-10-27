package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notification endpoint paths.
const (
	pathNotifications     = "/v1/notifications"
	pathBulkNotifications = "/v1/notifications/bulk"
	pathByProviderMessage = "/v1/notifications/by-provider-message-id/"
)

// errCursorStalled guards HistoryAll against a service that keeps returning the same
// cursor, which would otherwise paginate forever.
var errCursorStalled = errors.New("notifier: history cursor did not advance")

// Send dispatches one notification and returns the service's acknowledgement.
//
// The service is asynchronous: a successful Send means the notification was accepted and
// queued, not that it was delivered. The returned Status is normally StatusQueued; poll
// Status or StatusByProviderMessageID to observe the outcome.
//
// A 202 response is success. A 409 response means the IdempotencyKey was already
// accepted, which errors.Is(err, ErrConflict) reports.
func (c *Client) Send(ctx context.Context, p SendParams) (*Notification, error) {
	req, err := p.wire()
	if err != nil {
		return nil, err
	}
	var out Notification
	if err := c.doJSON(ctx, http.MethodPost, pathNotifications, nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendText sends a plain, non-templated message. It is shorthand for Send with only
// Channel, Recipient and Body set.
func (c *Client) SendText(ctx context.Context, channel ChannelType, recipient, body string) (*Notification, error) {
	return c.Send(ctx, SendParams{Channel: channel, Recipient: recipient, Body: body})
}

// SendTemplateByName sends a message rendered from a template chosen by name, which is the
// recommended send path: the service renders the template and localizes it, so callers
// only supply values.
//
// Values bind positionally to the template's declared param_names; use Vars to address
// placeholders by name instead.
func (c *Client) SendTemplateByName(ctx context.Context, p TemplateParams) (*Notification, error) {
	if strings.TrimSpace(p.Template) == "" {
		return nil, invalidArgument("template is required, for example \"verify\" or \"sms-group:verify\"")
	}
	return c.Send(ctx, SendParams{
		Channel:        p.Channel,
		Recipient:      p.Recipient,
		TemplateName:   p.Template,
		Values:         p.Values,
		Vars:           p.Vars,
		Group:          p.Group,
		Locale:         p.Locale,
		SendAt:         p.SendAt,
		CallbackURL:    p.CallbackURL,
		IdempotencyKey: p.IdempotencyKey,
		Metadata:       p.Metadata,
		Simulated:      p.Simulated,
	})
}

// Status fetches the current state of a notification by its Notifier UUID, which is the
// identifier returned by Send. Prefer this over StatusByProviderMessageID for a
// just-sent notification: the provider message id is only assigned once a provider has
// been contacted, which happens after the send has already been acknowledged.
//
// It returns an error matching ErrNotFound when the id is unknown to the tenant.
func (c *Client) Status(ctx context.Context, id string) (*Notification, error) {
	if strings.TrimSpace(id) == "" {
		return nil, invalidArgument("notification id is required")
	}
	var out Notification
	if err := c.doJSON(ctx, http.MethodGet, pathNotifications+"/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StatusByProviderMessageID fetches the state of a notification by the provider's own
// message id (for example a Kavenegar messageid recorded from a webhook or a provider
// report). The id is only known after the provider accepted the message.
func (c *Client) StatusByProviderMessageID(ctx context.Context, providerMessageID string) (*Notification, error) {
	if strings.TrimSpace(providerMessageID) == "" {
		return nil, invalidArgument("provider message id is required")
	}
	var out Notification
	path := pathByProviderMessage + url.PathEscape(providerMessageID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// History returns one page of the tenant's notification history, newest first. Pass the
// previous page's NextCursor in HistoryQuery.After to walk further back.
func (c *Client) History(ctx context.Context, q HistoryQuery) (*HistoryPage, error) {
	var out HistoryPage
	if err := c.doJSON(ctx, http.MethodGet, pathNotifications, q.values(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HistoryAll walks every page matching q and calls fn for each notification, in order,
// following cursors until the history is exhausted. Returning an error from fn stops the
// walk and returns that error.
//
// Set HistoryQuery.After to start from a specific cursor; it is overwritten as the walk
// progresses. For a bounded job, filter with From/To so the walk terminates on its own.
func (c *Client) HistoryAll(ctx context.Context, q HistoryQuery, fn func(*Notification) error) error {
	if fn == nil {
		return invalidArgument("fn is required")
	}
	for {
		page, err := c.History(ctx, q)
		if err != nil {
			return err
		}
		for i := range page.Items {
			if err := fn(&page.Items[i]); err != nil {
				return err
			}
		}
		if !page.HasMore() {
			return nil
		}
		if page.NextCursor == q.After {
			return errCursorStalled
		}
		q.After = page.NextCursor
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// sendRequest is the wire form of POST /v1/notifications.
type sendRequest struct {
	ChannelType    ChannelType     `json:"channel_type,omitempty"`
	RouteName      string          `json:"route_name,omitempty"`
	Recipient      string          `json:"recipient"`
	Subject        string          `json:"subject,omitempty"`
	Body           string          `json:"body,omitempty"`
	ChannelID      string          `json:"channel_id,omitempty"`
	GroupID        string          `json:"group_id,omitempty"`
	GroupName      string          `json:"group_name,omitempty"`
	TemplateID     string          `json:"template_id,omitempty"`
	TemplateName   string          `json:"template_name,omitempty"`
	TemplateVars   map[string]any  `json:"template_vars,omitempty"`
	TemplateParams []string        `json:"template_params,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	SendAt         *time.Time      `json:"send_at,omitempty"`
	CallbackURL    string          `json:"callback_url,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	Locale         string          `json:"locale,omitempty"`
	Simulated      bool            `json:"simulated,omitempty"`
}

// wire validates the parameters and renders them as the request body.
//
// The checks mirror the service's own rules, so a programming mistake fails immediately
// with ErrInvalidArgument instead of costing a round trip.
func (p SendParams) wire() (sendRequest, error) {
	route := p.RouteName
	templateName := p.TemplateName
	if templateName != "" {
		if strings.HasPrefix(templateName, "route:") && len(templateName) > len("route:") {
			if route == "" {
				route = templateName[len("route:"):]
			}
			templateName = ""
		}
	}
	if route == "" && p.Channel == "" {
		return sendRequest{}, invalidArgument("channel or route is required")
	}
	if strings.TrimSpace(p.Recipient) == "" {
		return sendRequest{}, invalidArgument("recipient is required")
	}
	if route == "" && p.Body == "" && p.TemplateID == "" && p.TemplateName == "" {
		return sendRequest{}, invalidArgument("one of body, template_id or template_name is required")
	}

	// "group:template" routes through a channel group by name. An explicit Group wins
	// over the prefix so a group name can be supplied dynamically. A "route:name"
	// reference is handled above and leaves templateName empty.
	group := p.Group
	if templateName != "" {
		if prefix, name := splitGroupTemplate(templateName); prefix != "" {
			if group == "" {
				group = prefix
			}
			templateName = name
		}
	}

	channelType := p.Channel
	if route != "" {
		// The route defines every channel; a leftover channel_type would be a
		// contradictory request.
		channelType = ""
	}
	return sendRequest{
		ChannelType:    channelType,
		RouteName:      route,
		Recipient:      p.Recipient,
		Subject:        p.Subject,
		Body:           p.Body,
		ChannelID:      p.ChannelID,
		GroupID:        p.GroupID,
		GroupName:      group,
		TemplateID:     p.TemplateID,
		TemplateName:   templateName,
		TemplateVars:   p.Vars,
		TemplateParams: p.Values,
		IdempotencyKey: p.IdempotencyKey,
		SendAt:         p.SendAt,
		CallbackURL:    p.CallbackURL,
		Metadata:       p.Metadata,
		Locale:         p.Locale,
		Simulated:      p.Simulated,
	}, nil
}
