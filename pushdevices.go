package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Push-device endpoint paths.
const (
	pathPushDevices    = "/v1/push/devices"
	pathVAPIDPublicKey = "/v1/push/vapid-public-key"
)

// PushSubscription is the browser's PushSubscription, exactly as
// registration.pushManager.subscribe() produces it and `subscription.toJSON()` serialises it.
//
// It is the one thing a Web Push send needs that the caller holds: the endpoint to POST to and
// the two keys the payload is encrypted to (RFC 8291). Register it once — the service returns a
// device id — and send to that id afterwards. Do not keep it in your own database as the value
// you send: the endpoint is a capability, and the service stores it encrypted for exactly that
// reason.
//
// The VAPID keys are not part of it. They belong to the channel and are configured once on the
// service; the browser only needs GetVAPIDPublicKey's value as its applicationServerKey.
type PushSubscription struct {
	// Endpoint is the push service URL for this subscription.
	Endpoint string `json:"endpoint"`
	// ExpirationTime is sent by some browsers and ignored by the service; expiry is detected
	// from the push service's 404/410 answer instead.
	ExpirationTime *int64 `json:"expirationTime,omitempty"`
	// Keys holds the client's public key material.
	Keys PushSubscriptionKeys `json:"keys"`
}

// PushSubscriptionKeys holds the P-256 public key and the 16-byte authentication secret of a
// subscription, as base64url strings.
type PushSubscriptionKeys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// ParsePushSubscription decodes the JSON a browser produced. It is a convenience over
// json.Unmarshal that names the shape being decoded, so the wire form cannot be mistaken for
// something the SDK accepts as a recipient.
func ParsePushSubscription(raw []byte) (*PushSubscription, error) {
	var sub PushSubscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, invalidArgument("push subscription is not valid JSON: %v", err)
	}
	return &sub, nil
}

// PushDevice is a registered browser subscription as the API reports it.
//
// The subscription itself is deliberately absent: the endpoint inside it is a capability, so it
// is write-only through this API. A caller that wants to send holds the ID.
type PushDevice struct {
	ID string `json:"id"`
	// UserRef is the tenant's own user identifier, opaque to the service.
	UserRef string `json:"user_ref"`
	// ChannelID is the vapid channel the subscription is bound to. A subscription only works
	// with that channel, because it was created with that channel's VAPID public key.
	ChannelID string `json:"channel_id,omitempty"`
	// EndpointHost is the push service host, for display (e.g. "fcm.googleapis.com").
	EndpointHost string `json:"endpoint_host"`
	IsActive     bool   `json:"is_active"`
	// FailureCount counts consecutive failed pushes since the last success.
	FailureCount  int32      `json:"failure_count"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time `json:"last_failure_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// IsUsable reports whether the device can still be sent to: the service deactivates a device
// when the push service reports its subscription gone (HTTP 404/410).
func (d PushDevice) IsUsable() bool { return d.IsActive }

// RegisterPushDeviceParams is the input to Client.RegisterPushDevice.
type RegisterPushDeviceParams struct {
	// Subscription is the browser's subscription. Required.
	Subscription PushSubscription
	// UserRef is your own user identifier for this device. Optional, but what makes
	// PushDeviceQuery.UserRef (send to all of one user's browsers) possible. Omitted on a
	// re-registration it keeps the stored binding — a frontend that registers on every visit
	// cannot silently unlink a device. Pass a different value to rebind the device, and use
	// DeletePushDevice for logout.
	UserRef string
	// ChannelID pins the vapid channel the browser subscribed through. Leave it empty unless
	// the tenant has more than one VAPID key pair: the service then uses the
	// highest-priority active vapid channel, and stores which one it used.
	ChannelID string
}

type registerPushDeviceRequest struct {
	ChannelID    string                   `json:"channel_id,omitempty"`
	UserRef      string                   `json:"user_ref,omitempty"`
	Subscription registerPushSubscription `json:"subscription"`
}

type registerPushSubscription struct {
	Endpoint       string                   `json:"endpoint"`
	ExpirationTime *int64                   `json:"expirationTime,omitempty"`
	Keys           registerSubscriptionKeys `json:"keys"`
}

type registerSubscriptionKeys struct {
	P256DH string `json:"p256dh"`
	Auth   string `json:"auth"`
}

// RegisterPushDevice registers a browser subscription and returns the device to send to.
//
// It is idempotent per subscription: calling it again with the same endpoint updates the same
// device and returns the same ID, so a frontend may call it on every visit. An empty
// Subscription.Endpoint is ErrInvalidArgument; a service with no active vapid channel is
// ErrNotFound; a tenant at its device limit is ErrConflict.
func (c *Client) RegisterPushDevice(ctx context.Context, p RegisterPushDeviceParams) (*PushDevice, error) {
	if strings.TrimSpace(p.Subscription.Endpoint) == "" {
		return nil, invalidArgument("push subscription endpoint is required")
	}
	if strings.TrimSpace(p.Subscription.Keys.P256DH) == "" || strings.TrimSpace(p.Subscription.Keys.Auth) == "" {
		return nil, invalidArgument("push subscription keys (p256dh, auth) are required")
	}

	body := registerPushDeviceRequest{
		ChannelID: strings.TrimSpace(p.ChannelID),
		UserRef:   p.UserRef,
		Subscription: registerPushSubscription{
			Endpoint:       p.Subscription.Endpoint,
			ExpirationTime: p.Subscription.ExpirationTime,
			Keys: registerSubscriptionKeys{
				P256DH: p.Subscription.Keys.P256DH,
				Auth:   p.Subscription.Keys.Auth,
			},
		},
	}

	var out PushDevice
	if err := c.doJSON(ctx, http.MethodPost, pathPushDevices, nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PushDevice fetches one device by id. It returns an error matching ErrNotFound when the id is
// unknown to the tenant.
func (c *Client) PushDevice(ctx context.Context, id string) (*PushDevice, error) {
	if strings.TrimSpace(id) == "" {
		return nil, invalidArgument("push device id is required")
	}
	var out PushDevice
	if err := c.doJSON(ctx, http.MethodGet, pathPushDevices+"/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PushDeviceQuery filters the device list.
type PushDeviceQuery struct {
	// UserRef limits the result to one of your users' devices.
	UserRef string
	// Limit is the page size. Zero uses the service default (100); the service caps it at 500
	// and reports the applied value in the page.
	Limit int
	// Offset is the number of devices to skip.
	Offset int
}

func (q PushDeviceQuery) values() url.Values {
	v := url.Values{}
	if q.UserRef != "" {
		v.Set("user_ref", q.UserRef)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	return v
}

// PushDevicePage is one page of registered devices, newest first.
type PushDevicePage struct {
	Items []PushDevice `json:"items"`
	// Limit is the page size the service applied, and Offset the one it was given: use them
	// to compute the next page rather than assuming the requested values were honoured.
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// HasMore reports whether another page may exist.
func (p PushDevicePage) HasMore() bool {
	return p.Limit > 0 && len(p.Items) >= p.Limit
}

// PushDevices returns one page of the tenant's registered devices.
//
// To notify every browser of one user, list with PushDeviceQuery.UserRef and send to each
// returned ID (or use Bulk with ChannelWebPush and those ids).
func (c *Client) PushDevices(ctx context.Context, q PushDeviceQuery) (*PushDevicePage, error) {
	var out PushDevicePage
	if err := c.doJSON(ctx, http.MethodGet, pathPushDevices, q.values(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePushDevice removes a device, so it stops receiving notifications. This is the logout
// path: without it, a signed-out browser keeps receiving the previous user's notifications.
//
// It returns an error matching ErrNotFound when the device does not exist (for example because a
// previous logout already removed it).
func (c *Client) DeletePushDevice(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return invalidArgument("push device id is required")
	}
	return c.doJSON(ctx, http.MethodDelete, pathPushDevices+"/"+url.PathEscape(id), nil, nil, nil)
}

// VAPIDKey is the sender identity a frontend subscribes with.
type VAPIDKey struct {
	ChannelID string `json:"channel_id"`
	// PublicKey is the applicationServerKey to pass to pushManager.subscribe().
	PublicKey string `json:"public_key"`
	// Subject is the sender contact the push service sees (mailto: or https: URL).
	Subject string `json:"subject"`
}

// GetVAPIDPublicKey returns the public key a browser must subscribe with, for one vapid channel
// (ChannelID empty selects the highest-priority active one).
//
// This is the call that replaces local VAPID keys: the service holds the pair, the browser
// subscribes with the public half, and the matching private half never leaves the service. It
// returns an error matching ErrNotFound when the tenant has no active vapid channel.
//
// Expose it to your frontend through your own backend rather than calling the Notifier API from
// the browser: the API key is a tenant credential.
func (c *Client) GetVAPIDPublicKey(ctx context.Context, channelID string) (*VAPIDKey, error) {
	q := url.Values{}
	if strings.TrimSpace(channelID) != "" {
		q.Set("channel_id", channelID)
	}
	var out VAPIDKey
	if err := c.doJSON(ctx, http.MethodGet, pathVAPIDPublicKey, q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendToDevice sends a plain web-push notification to a registered device.
//
// This is the typed path that a raw Send cannot give you: the recipient is a device id by
// construction, so the subscription JSON can never be passed where an id belongs. The channel is
// always ChannelWebPush. For a templated message use SendTemplateToDevice.
func (c *Client) SendToDevice(ctx context.Context, deviceID, subject, body string) (*Notification, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return nil, err
	}
	return c.Send(ctx, SendParams{
		Channel:   ChannelWebPush,
		Recipient: deviceID,
		Subject:   subject,
		Body:      body,
	})
}

// SendTemplateToDevice renders a template on the service and delivers it to a registered device.
// The template must exist for the webpush channel; p.Channel and p.Recipient are set from the
// arguments and ignored if supplied.
func (c *Client) SendTemplateToDevice(ctx context.Context, deviceID string, p TemplateParams) (*Notification, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return nil, err
	}
	p.Channel = ChannelWebPush
	p.Recipient = deviceID
	if strings.TrimSpace(p.Template) == "" {
		return nil, invalidArgument("template is required, for example \"order-ready\"")
	}
	return c.SendTemplateByName(ctx, p)
}

// DeviceRecipient returns the value to use as SendParams.Recipient for a registered device, so a
// caller that needs the generic Send (extra options, a channel group) still names the right
// thing. It reports the same argument error as SendToDevice.
func DeviceRecipient(deviceID string) (string, error) {
	if err := validateDeviceID(deviceID); err != nil {
		return "", err
	}
	return deviceID, nil
}

// validateDeviceID rejects anything that is not a device id, which in practice means catching a
// subscription JSON (or an endpoint) passed as a recipient before it becomes a failed
// notification row that looks like a provider problem.
func validateDeviceID(deviceID string) error {
	if strings.TrimSpace(deviceID) == "" {
		return invalidArgument("push device id is required; register the subscription with RegisterPushDevice first")
	}
	if looksLikeSubscription(deviceID) {
		return invalidArgument("recipient for the webpush channel is a device id, not a push subscription; " +
			"register the subscription with RegisterPushDevice and send to the id it returns")
	}
	if !looksLikeUUID(deviceID) {
		return invalidArgument("push device id %q is not a uuid; it is the id returned by RegisterPushDevice", deviceID)
	}
	return nil
}

func looksLikeSubscription(s string) bool {
	trimmed := strings.TrimSpace(s)
	return strings.HasPrefix(trimmed, "{") || strings.Contains(trimmed, "https://")
}

// looksLikeUUID is a shape check, not a parse: it exists to catch a value of the wrong kind
// (a JSON blob, an endpoint, a phone number) without pulling a uuid dependency into an SDK that
// otherwise has none.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
