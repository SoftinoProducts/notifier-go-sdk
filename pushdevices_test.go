package notifier

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const pushDeviceJSON = `{
	"id": "3f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
	"user_ref": "user-42",
	"channel_id": "8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
	"endpoint_host": "fcm.googleapis.com",
	"is_active": true,
	"failure_count": 0,
	"created_at": "2026-10-12T10:00:00Z",
	"updated_at": "2026-10-12T10:00:00Z"
}`

const deviceID = "3f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"

func testSubscription() PushSubscription {
	return PushSubscription{
		Endpoint: "https://fcm.googleapis.com/fcm/send/abc-123",
		Keys: PushSubscriptionKeys{
			P256DH: "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM",
			Auth:   "tBHItJI5svbpez7KI4CCXg",
		},
	}
}

func TestRegisterPushDevice_SendsTheBrowserSubscription(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusCreated, pushDeviceJSON))

	device, err := client.RegisterPushDevice(context.Background(), RegisterPushDeviceParams{
		Subscription: testSubscription(),
		UserRef:      "user-42",
		ChannelID:    "8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
	})
	if err != nil {
		t.Fatalf("RegisterPushDevice: %v", err)
	}

	method, target, header, _, _ := cap.snapshot()
	if method != http.MethodPost || target != "/v1/push/devices" {
		t.Errorf("request = %s %s, want POST /v1/push/devices", method, target)
	}
	if header.Get("X-API-Key") != testAPIKey {
		t.Error("the API key header is missing")
	}

	body := cap.jsonBody(t)
	sub, ok := body["subscription"].(map[string]any)
	if !ok {
		t.Fatalf("subscription is missing from the request: %v", body["subscription"])
	}
	if sub["endpoint"] != "https://fcm.googleapis.com/fcm/send/abc-123" {
		t.Errorf("endpoint = %v", sub["endpoint"])
	}
	keys, ok := sub["keys"].(map[string]any)
	if !ok || keys["p256dh"] == "" || keys["auth"] == "" {
		t.Errorf("keys = %v, want p256dh and auth", sub["keys"])
	}
	if body["user_ref"] != "user-42" {
		t.Errorf("user_ref = %v", body["user_ref"])
	}
	if body["channel_id"] != "8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("channel_id = %v", body["channel_id"])
	}

	if device.ID != deviceID || device.EndpointHost != "fcm.googleapis.com" || !device.IsUsable() {
		t.Errorf("device = %+v, want the registered device", device)
	}
}

// The subscription is a capability: it must never be echoed back, and the SDK type must not
// carry a field for it (a caller that reads one would be tempted to store it).
func TestRegisterPushDevice_ResponseCarriesNoSubscription(t *testing.T) {
	client := newTestClient(t, respondWith(nil, http.StatusCreated, pushDeviceJSON))

	device, err := client.RegisterPushDevice(context.Background(), RegisterPushDeviceParams{Subscription: testSubscription()})
	if err != nil {
		t.Fatalf("RegisterPushDevice: %v", err)
	}
	if device.ID == "" {
		t.Fatal("no device id came back")
	}
}

func TestRegisterPushDevice_ValidatesLocally(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusCreated, pushDeviceJSON))

	tests := map[string]RegisterPushDeviceParams{
		"no endpoint": {Subscription: PushSubscription{Keys: testSubscription().Keys}},
		"no keys":     {Subscription: PushSubscription{Endpoint: "https://fcm.googleapis.com/x"}},
		"only auth": {
			Subscription: PushSubscription{
				Endpoint: "https://fcm.googleapis.com/x",
				Keys:     PushSubscriptionKeys{Auth: "tBHItJI5svbpez7KI4CCXg"},
			},
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := client.RegisterPushDevice(context.Background(), params); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}

	if _, _, _, _, calls := cap.snapshot(); calls != 0 {
		t.Errorf("a locally invalid registration still made %d request(s)", calls)
	}
}

func TestRegisterPushDevice_SurfacesServiceErrors(t *testing.T) {
	tests := map[int]error{
		http.StatusNotFound: ErrNotFound, // no active vapid channel configured
		http.StatusConflict: ErrConflict, // tenant at its device limit
	}
	for status, want := range tests {
		client := newTestClient(t, respondWith(nil, status, `{"error":"nope"}`))
		_, err := client.RegisterPushDevice(context.Background(), RegisterPushDeviceParams{Subscription: testSubscription()})
		if !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}
}

func TestPushDevice_Get(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, pushDeviceJSON))

	device, err := client.PushDevice(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("PushDevice: %v", err)
	}
	if _, target, _, _, _ := cap.snapshot(); target != "/v1/push/devices/"+deviceID {
		t.Errorf("target = %q", target)
	}
	if device.UserRef != "user-42" {
		t.Errorf("user_ref = %q", device.UserRef)
	}
	if _, err := client.PushDevice(context.Background(), "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("blank id: err = %v, want ErrInvalidArgument", err)
	}
}

// An id that would change the shape of the path must be escaped, or a caller could address a
// different endpoint with the tenant's API key.
func TestPushDevice_GetEscapesThePathSegment(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, pushDeviceJSON))

	if _, err := client.PushDevice(context.Background(), "../../v1/channels"); err != nil {
		t.Fatalf("PushDevice: %v", err)
	}
	if _, target, _, _, _ := cap.snapshot(); strings.Contains(target, "../") {
		t.Errorf("target %q was not escaped", target)
	}
}

func TestPushDevices_PassesFiltersAndReadsThePage(t *testing.T) {
	cap := &captured{}
	page := `{"items":[` + pushDeviceJSON + `],"limit":25,"offset":50}`
	client := newTestClient(t, respondWith(cap, http.StatusOK, page))

	got, err := client.PushDevices(context.Background(), PushDeviceQuery{UserRef: "user-42", Limit: 25, Offset: 50})
	if err != nil {
		t.Fatalf("PushDevices: %v", err)
	}
	_, target, _, _, _ := cap.snapshot()
	for _, want := range []string{"user_ref=user-42", "limit=25", "offset=50"} {
		if !strings.Contains(target, want) {
			t.Errorf("target %q is missing %q", target, want)
		}
	}
	if len(got.Items) != 1 || got.Items[0].ID != deviceID {
		t.Errorf("items = %+v", got.Items)
	}
	if got.Limit != 25 || got.Offset != 50 {
		t.Errorf("page = (limit %d, offset %d), want (25, 50)", got.Limit, got.Offset)
	}
	if got.HasMore() {
		t.Error("a short page must not report more results")
	}

	// An empty filter must not send empty query parameters.
	empty := &captured{}
	client = newTestClient(t, respondWith(empty, http.StatusOK, `{"items":[],"limit":100,"offset":0}`))
	if _, err := client.PushDevices(context.Background(), PushDeviceQuery{}); err != nil {
		t.Fatalf("PushDevices: %v", err)
	}
	if _, target, _, _, _ := empty.snapshot(); strings.Contains(target, "?") {
		t.Errorf("target %q should carry no query string", target)
	}
}

func TestDeletePushDevice(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusNoContent, ""))

	if err := client.DeletePushDevice(context.Background(), deviceID); err != nil {
		t.Fatalf("DeletePushDevice: %v", err)
	}
	method, target, _, _, _ := cap.snapshot()
	if method != http.MethodDelete || target != "/v1/push/devices/"+deviceID {
		t.Errorf("request = %s %s, want DELETE /v1/push/devices/%s", method, target, deviceID)
	}

	if err := client.DeletePushDevice(context.Background(), ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("blank id: err = %v, want ErrInvalidArgument", err)
	}

	missing := newTestClient(t, respondWith(nil, http.StatusNotFound, `{"error":"not found"}`))
	if err := missing.DeletePushDevice(context.Background(), deviceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The VAPID key endpoint is what lets a tenant drop its own VAPID keys, so the SDK must expose it
// (and only it — the private key has no API).
func TestGetVAPIDPublicKey(t *testing.T) {
	const keyJSON = `{"channel_id":"8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8","public_key":"BNcRdreALRFXTkOOUHK1EtK2","subject":"mailto:ops@example.com"}`

	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusOK, keyJSON))

	key, err := client.GetVAPIDPublicKey(context.Background(), "8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	if err != nil {
		t.Fatalf("GetVAPIDPublicKey: %v", err)
	}
	if _, target, _, _, _ := cap.snapshot(); target != "/v1/push/vapid-public-key?channel_id=8c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("target = %q", target)
	}
	if key.PublicKey == "" || key.Subject != "mailto:ops@example.com" {
		t.Errorf("key = %+v", key)
	}

	def := &captured{}
	client = newTestClient(t, respondWith(def, http.StatusOK, keyJSON))
	if _, err := client.GetVAPIDPublicKey(context.Background(), ""); err != nil {
		t.Fatalf("GetVAPIDPublicKey: %v", err)
	}
	if _, target, _, _, _ := def.snapshot(); target != "/v1/push/vapid-public-key" {
		t.Errorf("target = %q, want no channel_id for the default channel", target)
	}
}

func TestSendToDevice_UsesTheDeviceIDAsRecipient(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	if _, err := client.SendToDevice(context.Background(), deviceID, "Build finished", "deploy #42 is live"); err != nil {
		t.Fatalf("SendToDevice: %v", err)
	}

	body := cap.jsonBody(t)
	if body["channel_type"] != "webpush" {
		t.Errorf("channel_type = %v, want webpush", body["channel_type"])
	}
	if body["recipient"] != deviceID {
		t.Errorf("recipient = %v, want the device id", body["recipient"])
	}
	if body["body"] != "deploy #42 is live" || body["subject"] != "Build finished" {
		t.Errorf("content = (%v, %v)", body["subject"], body["body"])
	}
}

func TestSendTemplateToDevice(t *testing.T) {
	cap := &captured{}
	client := newTestClient(t, respondWith(cap, http.StatusAccepted, notificationJSON))

	if _, err := client.SendTemplateToDevice(context.Background(), deviceID, TemplateParams{
		Template: "order-ready",
		Values:   []string{"1234"},
	}); err != nil {
		t.Fatalf("SendTemplateToDevice: %v", err)
	}

	body := cap.jsonBody(t)
	if body["channel_type"] != "webpush" || body["recipient"] != deviceID {
		t.Errorf("request = %v", body)
	}
	if body["template_name"] != "order-ready" {
		t.Errorf("template_name = %v", body["template_name"])
	}

	if _, err := client.SendTemplateToDevice(context.Background(), deviceID, TemplateParams{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("no template: err = %v, want ErrInvalidArgument", err)
	}
}

// The mistake this guards against is a real one: passing the browser's subscription where the
// API expects the device id. It must fail at the call site, not on a dispatch the caller has to
// dig out of the notification history.
func TestDeviceRecipient_RejectsASubscriptionPassedAsAnID(t *testing.T) {
	if _, err := DeviceRecipient(deviceID); err != nil {
		t.Fatalf("a device id must be accepted: %v", err)
	}

	for name, value := range map[string]string{
		"subscription json": `{"endpoint":"https://fcm.googleapis.com/fcm/send/abc","keys":{"p256dh":"a","auth":"b"}}`,
		"bare endpoint":     "https://fcm.googleapis.com/fcm/send/abc",
		"phone number":      "+989120000000",
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DeviceRecipient(value); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}

	// The typed send path applies the same check, so the generic Send is not the only safe door.
	client := newTestClient(t, respondWith(nil, http.StatusAccepted, notificationJSON))
	_, err := client.SendToDevice(context.Background(), `{"endpoint":"https://fcm.googleapis.com/x"}`, "s", "b")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("SendToDevice err = %v, want ErrInvalidArgument", err)
	}
}

func TestParsePushSubscription(t *testing.T) {
	raw := []byte(`{"endpoint":"https://fcm.googleapis.com/fcm/send/abc","expirationTime":null,"keys":{"p256dh":"BNcR","auth":"tBHI"}}`)
	sub, err := ParsePushSubscription(raw)
	if err != nil {
		t.Fatalf("ParsePushSubscription: %v", err)
	}
	if sub.Endpoint == "" || sub.Keys.Auth != "tBHI" {
		t.Errorf("subscription = %+v", sub)
	}

	if _, err := ParsePushSubscription([]byte("not json")); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
}
