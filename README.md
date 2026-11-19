# Notifier Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/SoftinoProducts/notifier-go-sdk.svg)](https://pkg.go.dev/github.com/SoftinoProducts/notifier-go-sdk)
[![Go 1.26.8+](https://img.shields.io/badge/Go-1.26.8%2B-00ADD8)](https://go.dev/dl/)

A Go client for the **Notifier** multi-channel notification hub — SMS, email, web-push, Bale and Telegram
behind one API, with the concrete provider (Kavenegar, SMS.ir, SMTP, Centrifugo, …) resolved by the
service from your tenant's channel configuration.

- **Zero third-party dependencies.** Only the standard library, so `go.mod` stays empty for you.
- **Idiomatic Go.** `context.Context` everywhere, functional options, typed errors, plain structs.
- **Fast.** A shared connection pool tuned for concurrency, pooled response buffers, allocation-free
  parameter rendering, and bulk sends that fan out with bounded parallelism.
- **Bounded non-blocking dispatch.** `Sender` adds HIGH/BULK queues, fixed workers, queue-full
  backpressure, safe retries and graceful drain for request-path OTP sends.
- **Idempotency and priority.** `NewIdempotencyKey`, `Idempotency-Key`/`X-Priority` headers and
  `WithMaxConnsPerHost` keep retries and bursts under control.

```go
client, err := notifier.NewClient(os.Getenv("NOTIFIER_API_KEY"))
if err != nil {
    return err
}

res, err := client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: "+989120000000",
    Template:  "verify",
    Values:    []string{"123456"}, // bound to the template's param_names in order
})
if err != nil {
    return err
}

fmt.Println(res.ID, res.Status) // 9f1a2b3c-… queued
```

---

## Install

```bash
go get github.com/SoftinoProducts/notifier-go-sdk
```

Requires **Go 1.26.8 or newer** and a Notifier API key. The default endpoint is
`https://notifier-api.vibe.ir`; point elsewhere with `WithBaseURL`.

---

## Sending with a template (recommended)

Templates are rendered server-side, so the message text and its localization stay out of your code.
The service looks the template up by `(channel, name, locale)`.

> **Prerequisite:** the template must declare its `param_names` in the same order you pass `Values`.
> If you would rather address placeholders by name, use `Vars`.

### Values positionally

```go
client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: phone,
    Template:  "verify",                     // template declares param_names ["token"]
    Values:    []string{"123456"},
})
```

### Values by name

```go
client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: phone,
    Template:  "order_notice",
    Vars:      map[string]any{"amount": 50000, "region": "Tehran"},
})
```

### Routing through a channel group: `"group:template"`

Prefix the template with a group name and the send routes through that channel group by name:

```go
client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: phone,
    Template:  "sms-group:verify",
    Values:    []string{"123456"},
})
```

Equivalent, and useful when the group name is dynamic — an explicit `Group` wins over the prefix:

```go
client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: phone,
    Template:  "verify",
    Group:     "sms-group",
    Values:    []string{"123456"},
})
```

### Telegram and Bale

Both are ordinary channels. Create a template with `channel_type: "telegram"` or
`"bale"` and declare its `param_names`, exactly as for SMS. For a **direct** send the
recipient is the messenger chat id:

```go
// Direct Telegram: recipient is the numeric chat id.
res, err := client.SendText(ctx, notifier.ChannelTelegram, "67313260", "Your code: 123456")

res, err = client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelTelegram,
    Recipient: "67313260",
    Template:  "telegram-otp",              // channel_type must be telegram
    Values:    []string{"123456"},
})

// Direct Bale: recipient is the Bale chat id.
res, err = client.SendText(ctx, notifier.ChannelBale, "123456789", "Your code: 123456")

res, err = client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelBale,
    Recipient: "123456789",
    Template:  "bale-otp",                  // channel_type must be bale
    Values:    []string{"123456"},
})
```

A direct Telegram send needs a chat id and does **not** use a binding. A route that sends
to a phone number can resolve the phone to a Telegram chat through a binding; that is the
`recipient_source: "binding"` mode described below. Bale has no binding step: use the Bale
chat id.

### Sending through a saved route: `"route:name"` (fan-out)

A saved route is a named fan-out defined in Notifier (UI or
`POST /v1/notification-routes`). It is not a channel group:

| | Channel group | Saved route |
|---|---|---|
| Purpose | failover inside one channel type | deliver over every selected target |
| Targets | channels of one type | up to five targets, possibly different types |
| Result | one notification | one child notification per target, grouped by `fanout_id` |
| SDK syntax | `"group:template"` | `"route:<name>"` |
| Bulk | allowed | rejected (`route:` is per-recipient) |

The route owns its channels, templates, recipient sources and required flags, so `Channel`
is ignored:

```go
res, err := client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS, // ignored; the route defines the channels
    Recipient: phone,
    Template:  "route:otp",
    Values:    []string{"123456"},
})
```

An explicit `RouteName` is available on `SendParams` for dynamic code; it takes
precedence over the `route:` prefix. Do not also set `TemplateName`:

```go
res, err := client.Send(ctx, notifier.SendParams{
    RouteName:      "otp",
    Recipient:      phone,
    Values:         []string{"123456"},
    IdempotencyKey: "otp-" + requestID,
})
```

A replay with the same idempotency key returns the original fanout instead of sending the
targets again.

#### Let the request choose the template

A route target can set `template_source: "request"` and omit its own template. The send then
supplies `template_name` (or `template_id`), which is resolved for that target's channel type.
This lets one generic Telegram route serve many notification categories:

```json
{
  "name": "telegram-generic",
  "targets": [
    {"channel_type": "telegram", "channel_id": "TELEGRAM_CHANNEL_ID", "template_source": "request", "recipient_source": "binding", "required": false}
  ]
}
```

```go
res, err := client.Send(ctx, notifier.SendParams{
    RouteName:    "telegram-generic",
    Recipient:    phone,
    TemplateName: "otp-login",       // resolved for the target's channel type
    Values:       []string{"123456"},
})
```

`SendTemplateByName` cannot carry a request template together with a `route:` reference — the
reference itself is a template string — so use `Send` with `RouteName` and `TemplateName`.
A send through a route without any `template_source: "request"` target rejects a request
template with `400`; a request-template target without a template is also `400`.

A route definition created once in the UI or with the API looks like this:

```json
{
  "name": "otp",
  "targets": [
    {"channel_type": "sms", "channel_id": "SMS_CHANNEL_ID", "template_id": "SMS_TEMPLATE_ID", "recipient_source": "request", "required": true},
    {"channel_type": "bale", "channel_id": "BALE_CHANNEL_ID", "template_id": "BALE_TEMPLATE_ID", "recipient_source": "request", "required": true},
    {"channel_type": "telegram", "channel_id": "TELEGRAM_CHANNEL_ID", "template_id": "TELEGRAM_TEMPLATE_ID", "recipient_source": "binding", "required": false}
  ]
}
```

- `recipient_source: "request"` sends to the route's `Recipient` (the phone number).
- `recipient_source: "binding"` is for Telegram: the service resolves the phone to a
  chat id through `channel_bindings`.
- `required: true` makes a missing Telegram binding fail the send with
  `400 no telegram binding for <phone>`. With `required: false`, that leg is reported as
  `skipped` instead.
- The route and every target template must be active, and all templates must declare the
  same number of parameters (the API validates this; a route has at most five targets).

### Binding a Telegram chat

Binding is an account-management operation, not a send; the SDK does not wrap it. Create a
one-time deep link with the REST API:

```bash
curl -X POST "https://notifier-api.vibe.ir/v1/channels/$TELEGRAM_CHANNEL_ID/bindings" \
  -H "X-API-Key: $NOTIFIER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"external_ref":"+989120000000"}'
```

The response contains `token`, `deep_link` and `expires_at`. Send the user to `deep_link`;
after they press **Start**, the phone is bound to that Telegram chat and a route send to
the phone resolves the Telegram leg automatically. The token is single-use and expires
after 15 minutes.

`GET /v1/channels/{channel_id}/bindings` returns a **bare JSON array** (no `items` envelope
and no pagination), newest first:

```json
[
  {
    "id": "8f0c0f6e-...",
    "channel_id": "53ea93ef-...",
    "external_ref": "+989120000000",
    "chat_id": "67313260",
    "created_at": "2026-10-27T11:00:00Z",
    "updated_at": "2026-10-27T11:00:00Z"
  }
]
```

Remove one by its binding `id`:

```bash
curl -X DELETE "https://notifier-api.vibe.ir/v1/channels/$TELEGRAM_CHANNEL_ID/bindings/$BINDING_ID" \
  -H "X-API-Key: $NOTIFIER_API_KEY"    # 204 No Content
```

An unknown binding id returns `404`; an invalid UUID returns `400`. Bindings are scoped to
the channel and tenant. One chat maps to one phone: a later `/start` for the same chat
rebinds it to the new phone and removes the old phone's binding.

### What the route call returns

The call returns after the child legs are created; it does not wait for delivery.
`Notification.FanoutID` and `Notification.FanoutStatus` carry the aggregate; each leg is an
independent notification:

```go
fmt.Println(res.ID)           // primary notification UUID
fmt.Println(res.Status)       // usually queued
fmt.Println(res.FanoutID)     // fanout UUID, empty on a single-channel send
fmt.Println(res.FanoutStatus) // queued | processing | delivered | failed | partial | skipped
```

Poll the primary leg with `client.Status(ctx, res.ID)`. To inspect every child, call the
fanout endpoint (the SDK does not wrap it):

```bash
curl -H "X-API-Key: $NOTIFIER_API_KEY" \
  "https://notifier-api.vibe.ir/v1/notification-fanouts/$FANOUT_ID"
```

`targets[]` reports each leg's `channel_type`, `recipient`, `status`, `notification_id` and
failure reason. The aggregate is `delivered` only when the required legs are delivered; an
optional missing binding is `skipped`, and a mix of delivered and failed legs is `partial`.
The route's children are independent notifications, so retrying one leg does not resend
the others.

Bulk sends reject `route:` because a batch is scoped to one channel type.

### Scheduling, callbacks and idempotency

`TemplateParams` and `SendParams` both accept the full set of send options:

```go
sendAt := time.Now().Add(24 * time.Hour)

client.Send(ctx, notifier.SendParams{
    Channel:        notifier.ChannelEmail,
    Recipient:      "ops@example.com",
    Subject:        "Nightly report",
    Body:           "Everything is fine.",
    SendAt:         &sendAt,                                   // schedule instead of dispatch now
    CallbackURL:    "https://your-app/notifier/callback",      // signed delivery callback
    IdempotencyKey: "nightly-2026-09-10",                      // makes a retry safe
    Locale:         "en",
    Metadata:       json.RawMessage(`{"report_id":42}`),
})
```

`Metadata` is passed through byte-for-byte, so there is no encoding round trip. Build it from a Go
value with `notifier.JSON`:

```go
metadata, err := notifier.JSON(map[string]any{"order_id": 42})
```

---

## Sending without a template

`SendText` is shorthand for a plain body; `Send` covers everything, including templates by UUID:

```go
client.SendText(ctx, notifier.ChannelSMS, phone, "Your code is 123456")

client.Send(ctx, notifier.SendParams{
    Channel:    notifier.ChannelSMS,
    Recipient:  phone,
    TemplateID: "3f8b1c2d-0000-1111-2222-333344445555",
    Vars:       map[string]any{"token": "123456"},
})
```

---

## Bulk

Both bulk methods split the input into requests of at most `notifier.MaxBulkMessages` (10 000, the
service limit) and validate every message **before** the first request goes out, so a bad entry
cannot leave half the batch dispatched.

- `Bulk` sends the chunks one at a time, in order.
- `BulkConcurrent` runs a fixed worker pool of `BulkOptions.Concurrency` chunks (default
  `notifier.DefaultBulkConcurrency`), which is what you want for batches far larger than one chunk.

```go
messages := []notifier.BulkMessage{
    {Recipient: "+989120000001", TemplateName: "verify", Values: []string{"111111"}},
    {Recipient: "+989120000002", TemplateName: "verify", Values: []string{"222222"}},
}

result, err := client.BulkConcurrent(ctx,
    notifier.BulkParams{Channel: notifier.ChannelSMS, Messages: messages},
    notifier.BulkOptions{Concurrency: 4})

fmt.Println(result.Total, "accepted in", len(result.Batches), "batches")
for _, failure := range result.Failures {
    log.Printf("messages[%d..%d] rejected: %v",
        failure.Offset, failure.Offset+failure.Size-1, failure.Err)
}
```

A partial success is normal and is **not** an error you can ignore: `err` is non-nil when any chunk
failed, and the returned `*BulkResult` always describes what was accepted and what was not. Batches
and failures are reported in input order; the chunk requests themselves are dispatched as workers
free up, so the service may receive them out of order.

---

## Rehearsing a send (simulated)

Pass `Simulated: true` to exercise the whole path — routing, group selection, template rendering,
provider-template mapping, the status event and the panel — without delivering anything and
without touching your templates, groups or channels. The service replaces the provider call with
a simulator, so the notification is reported as delivered by provider `simulator` and its
provider message id reads `sim-sms-kavenegar-1`: the would-be provider is still named, which makes
a rehearsal useful for validating the configuration it bypassed.

```go
res, err := client.SendTemplateByName(ctx, notifier.TemplateParams{
    Channel:   notifier.ChannelSMS,
    Recipient: phone,
    Template:  "sms-group:verify", // your real template and group, unchanged
    Values:    []string{token},
    Simulated: true,               // the only difference from a real send
})
fmt.Println(res.Simulated, res.Provider) // true simulator
```

`SendParams.Simulated` and `BulkParams.Simulated` behave the same way. In a bulk send a single
message can opt out with `Simulated: notifier.Simulate(false)`.

```go
client.Bulk(ctx, notifier.BulkParams{
    Channel:   notifier.ChannelSMS,
    Simulated: true,
    Messages: []notifier.BulkMessage{
        {Recipient: a, TemplateName: "verify", Values: []string{"111111"}},   // rehearsed
        {Recipient: b, TemplateName: "verify", Values: []string{"222222"},
         Simulated: notifier.Simulate(false)},                                // really sent
    },
})
```

Worth knowing: a rehearsal still creates a notification, counted in analytics and visible in the
panel, with `provider = simulator`. It is not a dry run that leaves no trace. `Notification.Simulated`
reports whether the delivery was rehearsed, as the service recorded it.

---

## Checking status

Notifier is **asynchronous**: a successful send means the notification was *accepted and queued*.
The provider has not been contacted yet, so the acknowledgement carries `StatusQueued` and no
provider detail.

```go
res, err := client.SendText(ctx, notifier.ChannelSMS, phone, "hello")

// Poll by the Notifier UUID returned by the send.
current, err := client.Status(ctx, res.ID)
switch {
case current.IsDelivered():
    // arrived
case current.IsFailed():
    log.Println("failed:", current.FailureReason())
case current.IsPending():
    // queued or processing — poll again
}
```

Because sends are asynchronous, the provider's own message id is **not** available at send time.
Use the UUID from the send, and `StatusByProviderMessageID` only for ids you obtained later (from a
callback or a provider report):

```go
current, err := client.StatusByProviderMessageID(ctx, "1234567890")
```

---

## Web Push (browser notifications)

A real Web Push notification reaches the browser through its push service — even while the page is
closed — which needs the browser's subscription (`endpoint`, `p256dh`, `auth`) and a **VAPID** key
pair. Both are handled for you: the subscription is registered once and referenced afterwards by a
**device id**, and the key pair lives on the `webpush` channel with `provider: "vapid"`.

**`recipient` for that channel is a device id, not the subscription.** Register the subscription,
then send to the id:

```go
// 1. Register the subscription your frontend collected. This is idempotent per endpoint, so a
//    frontend may call it on every visit.
device, err := client.RegisterPushDevice(ctx, notifier.RegisterPushDeviceParams{
    Subscription: notifier.PushSubscription{
        Endpoint: sub.Endpoint,
        Keys:     notifier.PushSubscriptionKeys{P256DH: sub.Keys.P256dh, Auth: sub.Keys.Auth},
    },
    // Your own user id; lets you list that user's devices later. Omitted on a re-registration
    // it keeps the stored binding, so registering on every visit never unlinks a device.
    UserRef: "user-42",
})
if err != nil {
    return err // ErrNotFound: no active vapid channel. ErrConflict: device limit reached.
}

// 2. Send. The typed helper fixes the channel and the recipient, so a subscription JSON can
//    never be passed where a device id belongs.
_, err = client.SendToDevice(ctx, device.ID, "Build finished", "deploy #42 is live")

// A templated message works the same way.
_, err = client.SendTemplateToDevice(ctx, device.ID, notifier.TemplateParams{
    Template: "order-ready",
    Values:   []string{"1234"},
})
```

Every browser of one user, and the logout path:

```go
page, err := client.PushDevices(ctx, notifier.PushDeviceQuery{UserRef: "user-42", Limit: 100})
for _, d := range page.Items {
    if d.IsUsable() { // false once the push service reported the subscription gone
        _, _ = client.SendToDevice(ctx, d.ID, "Build finished", "deploy #42 is live")
    }
}

// On logout — otherwise the next user of this browser keeps getting the previous one's pushes.
err = client.DeletePushDevice(ctx, device.ID)
```

Your frontend must subscribe with the **service's** public key, not one you generate locally:

```go
key, err := client.GetVAPIDPublicKey(ctx, "") // {"channel_id","public_key","subject"}
// Expose key.PublicKey to the browser (through your own backend) and pass it as
// applicationServerKey to pushManager.subscribe().
```

If you need the generic `Send` (extra options, a channel group), get the recipient through
`notifier.DeviceRecipient(device.ID)`: it validates the value and refuses a subscription JSON with a
message that says what to do instead. And if your frontend posts you the raw JSON,
`notifier.ParsePushSubscription(raw)` decodes it.

Full contract, rationale and migration steps: `notifier-api/acknowledgement/webpush.md`.

---

## History

```go
page, err := client.History(ctx, notifier.HistoryQuery{
    Limit:     50,
    Status:    notifier.StatusFailed,
    Channel:   notifier.ChannelSMS,
    Recipient: "+98912",
    From:      time.Now().Add(-24 * time.Hour),
})

for i := range page.Items {
    fmt.Println(page.Items[i].ID, page.Items[i].Status)
}

if page.HasMore() {
    next, err := client.History(ctx, notifier.HistoryQuery{Limit: 50, After: page.NextCursor})
    _ = next
}
```

`HistoryAll` follows the cursors for you:

```go
err := client.HistoryAll(ctx, notifier.HistoryQuery{
    From: time.Now().Add(-time.Hour),
}, func(n *notifier.Notification) error {
    return index(n) // returning an error stops the walk
})
```

---

## Error handling

Errors are typed. `errors.Is` classifies them and `errors.As` reaches the HTTP detail.

| Sentinel | Meaning |
|---|---|
| `ErrInvalidArgument` | The SDK rejected the arguments locally. No request was sent. |
| `ErrBadRequest` | 400/422 — the service rejected the request. |
| `ErrUnauthorized` | 401 — missing, revoked or wrong API key. |
| `ErrForbidden` | 403 — the key is valid but not allowed here. |
| `ErrNotFound` | 404 — unknown notification or resource. |
| `ErrConflict` | 409 — most often a reused `IdempotencyKey`. |
| `ErrRateLimited` | 429 — see `APIError.RetryAfter`. |
| `ErrUnavailable` | 503 — no channel available to route the message. |
| `ErrServer` | Any other 5xx. |
| `ErrQueueFull` | `Sender.Enqueue*` found the selected lane full; nothing was queued. Use `errors.As` for `*QueueFullError.RetryAfter`. |
| `ErrSenderClosed` | The `Sender` has been closed and no longer accepts work. |
| `ErrDrainTimeout` | `Sender.Close` reached its context deadline; remaining work was failed, not stranded. |

```go
_, err := client.Status(ctx, id)

var apiErr *notifier.APIError
switch {
case errors.Is(err, notifier.ErrNotFound):
    // unknown id
case errors.Is(err, notifier.ErrRateLimited):
    var rateErr *notifier.APIError
    if errors.As(err, &rateErr) && rateErr.RetryAfter > 0 {
        // back off for rateErr.RetryAfter
    }
case errors.As(err, &apiErr):
    log.Printf("%s %s -> %d: %s (request id %s)",
        apiErr.Method, apiErr.Path, apiErr.StatusCode, apiErr.Message, apiErr.RequestID)
}
```

`APIError` also carries `Retryable()`, `RetryableSend(idempotencyKey)`, and the `X-Request-Id` so a
support request can be tied to the exact server-side log line. `APIError.Message` is the service's
`{"error": ...}` text or a generic `HTTP <status>`; **raw response bytes are never copied**, because
a rendered notification body can contain an OTP. `IsPreSendError(err)` identifies transport
failures that happened before a request could reach the hub (DNS, dial, TLS handshake), which are
the only transport failures safe to retry without an idempotency key.

`Client.Send`, `Client.SendTemplateByName` and `Client.Bulk` never retry on their own: repeating a
request risks delivering the message twice, which is worse than not delivering it at all. The
optional `Sender` applies a safe policy (see
[Non-blocking dispatch](#non-blocking-dispatch-sender)).

---

## Receiving callbacks

Notifier signs delivery callbacks with HMAC-SHA256 over **`<timestamp>.<raw body>`** and sends the
digest in `X-Notifier-Signature` as `sha256=<hex>`, with the timestamp (Unix seconds) in
`X-Notifier-Timestamp`. `SignatureVerifier` authenticates them and restores the body for your
handler:

```go
verifier, err := notifier.NewSignatureVerifier(os.Getenv("NOTIFIER_WEBHOOK_SECRET"))
if err != nil {
    return err
}

http.Handle("/notifier/callback", verifier.Handler(func(w http.ResponseWriter, r *http.Request) {
    var event notifier.CallbackEvent
    if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
        http.Error(w, "bad payload", http.StatusBadRequest)
        return
    }
    // event.NotificationID is the leg that changed; for a route child it is the
    // child's notification id, and event.FanoutID groups the legs of one send.
    fmt.Println(event.NotificationID, event.FanoutID, event.Channel, event.Status)
    w.WriteHeader(http.StatusNoContent)
}))
```

Requests with a missing, malformed or wrong signature get a `401` and never reach your handler, as
does a request whose signed timestamp is outside the tolerance window (`5m` by default, adjustable
with `WithTimestampTolerance`). The comparison is constant time. For a body you already have (a
queue consumer, another framework), use the one-shot
`notifier.VerifySignature(secret, body, timestamp, header)`.

**The digest covers the bytes exactly as they arrived.** Verify before decoding the JSON: decoding
and re-encoding changes whitespace and field order, and the signature will never match. A receiver
that signs the body alone (without the timestamp) is rejected — that scheme was the old contract and
it let a captured callback be replayed forever.

The signed material is:

```text
HMAC-SHA256(webhook_secret, "<X-Notifier-Timestamp>.<raw request body>")
X-Notifier-Signature: sha256=<lowercase hex>
```

A fixed vector you can use to check an implementation in any language:

```text
secret    = whsec_test_secret
timestamp = 1730000000
body      = {"notification_id":"801e8e60-4686-4cba-8068-e36c88618b16","status":"delivered","channel_type":"telegram"}
signature = 79c471e9051eb5cb40beba0463903244d673cd74a0361ab9114daece83c38114
```

`sha256=6e3cc01bb604f7cd43b6a4b6ec86456ec0853155581b3f70a03e2d9ccc6c3ba6` is the same body signed
without the timestamp; it must be refused.

**Upgrading from the previous verifier:** `VerifySignature` and `SignatureVerifier.Verify` now take
the timestamp and return an `error`. The old two-argument forms verified the body alone, which the
service never sent, so they rejected every real callback; they were removed rather than deprecated.
Existing code fails to compile, which is the intent — a version that silently kept verifying the old
way would keep returning `401` in production.

The secret is the API key's `webhook_secret`, returned **once** as `webhook_secret` when the key is
created or rotated. An older key that has no webhook secret cannot sign callbacks; rotate it and
store the new secret. A per-notification `callback_url` does not need a tenant-level webhook URL:
the status worker signs it with any tenant key that has a webhook secret.

Each **non-skipped child** of a route send produces its own callback: `notification_id` is the
child's id, `channel_type` is that leg's channel, and `fanout_id` is the route send. A
single-channel notification omits `fanout_id`. The callback is a terminal outcome
(`delivered` or `failed`), not an acknowledgement of the initial `queued` response. A
skipped optional target is never dispatched, so it produces no callback; correlating a
late binding is not possible from the original occurrence. The hub retries a callback on
any non-2xx response: three attempts, 500 ms then 1 s apart, within a 10 s timeout.

---

## Configuration

```go
client, err := notifier.NewClient(os.Getenv("NOTIFIER_API_KEY"),
    notifier.WithBaseURL("https://notifier.staging.internal"), // default: https://notifier-api.vibe.ir
    notifier.WithTimeout(10*time.Second),                      // default: 30s per round trip
    notifier.WithHTTPClient(myInstrumentedClient),              // e.g. an OpenTelemetry RoundTripper
    notifier.WithMaxConnsPerHost(64),                           // bound sockets (SDK-owned transport only)
    notifier.WithUserAgent("trade-hub/1.4"),                    // default: notifier-go-sdk/<version> <go version>
)
```

A `Client` is safe for concurrent use and holds no resources that need closing: build **one per API
key** at startup and share it. `NewClient` returns an error for a blank API key or an unusable base
URL, so misconfiguration fails at boot rather than on the first message.

---

## Non-blocking dispatch: Sender

`Client.Send` blocks the calling goroutine until the hub answers. On an HTTP handler that means the
request goroutine pays the full round trip, and on a slow or down hub it can pay up to
`DefaultTimeout` (30s). Go goroutines are cheap, but a naive

```go
go client.Send(ctx, params) // do not do this in a request handler
```

is not a solution: it creates one goroutine and one potential connection per message, has no
backpressure, and gives you no way to drain accepted work on shutdown. `Sender` is the bounded,
lifecycle-aware equivalent of the Java SDK's `*AndForget`/`*Async` path.

### Quick start

```go
client, err := notifier.NewClient(os.Getenv("NOTIFIER_API_KEY"))
if err != nil {
    return err
}
sender, err := notifier.NewSender(client, notifier.SenderOptions{
    HighWorkers: 1, BulkWorkers: 2,
    HighQueueSize: 1_000, BulkQueueSize: 10_000,
    SendTimeout: 10 * time.Second,        // total per job, including retries
    MaxRetries: 2,                        // 0 selects 2; negative disables
    RetryAmbiguous: false,                // true only after the hub honours Idempotency-Key
    Listener: myListener,                 // optional
})
if err != nil {
    return err
}
defer sender.Close(context.Background())

key, err := notifier.NewIdempotencyKey()
if err != nil {
    return err
}

// OTP / request path: returns after a non-blocking queue offer.
if err := sender.Enqueue(r.Context(), notifier.SendParams{
    Channel:        notifier.ChannelSMS,
    Recipient:      phone,
    TemplateName:   "vibe:salerequest",
    Values:         []string{otp},
    Priority:       notifier.PriorityHigh,
    IdempotencyKey: key,
}); err != nil {
    var full *notifier.QueueFullError
    if errors.As(err, &full) {
        // Nothing was queued. Wait full.RetryAfter or degrade deliberately.
    }
    return err
}
```

`Enqueue` returns only local errors: `*QueueFullError` (`errors.Is(err, notifier.ErrQueueFull)`),
`ErrSenderClosed`, an invalid-argument error, or a key-generation failure. Network failures never
block or return from `Enqueue`; they are counted as dropped and reported to the `SendListener`.

Use `EnqueueResult` when you need the notification id or a typed error without blocking:

```go
ch, err := sender.EnqueueResult(ctx, notifier.SendParams{
    Channel: notifier.ChannelSMS, Recipient: phone, Body: "hello",
})
if err != nil { return err }
go func() {
    res := <-ch // exactly one result; the Sender closes the channel afterwards
    if res.Err != nil {
        log.Printf("send failed key=%s: %v", res.IdempotencyKey, res.Err)
        return
    }
    log.Printf("queued id=%s attempts=%d", res.Notification.ID, res.Attempts)
}()
```

### API map

| Blocking | Result-bearing non-blocking | Fire-and-forget |
|---|---|---|
| `Client.Send(ctx, SendParams)` | `Sender.EnqueueResult(ctx, SendParams)` | `Sender.Enqueue(ctx, SendParams)` |
| `Client.SendTemplateByName(ctx, TemplateParams)` | convert to `SendParams` and call `EnqueueResult` | convert and call `Enqueue` |
| `Client.Bulk` / `BulkConcurrent` | already bounded by `BulkOptions.Concurrency`; no Sender lane | same |

`Sender` deliberately has no goroutine-per-message variant and no unbounded worker pool. The
worker count is the ceiling for concurrent hub requests, independent of send rate.

### Lanes, queue full, and backpressure

| Lane | Default workers | Default queue | Use |
|---|---:|---:|---|
| `PriorityHigh` | 1 | 1,000 | OTP and transactional sends |
| `PriorityBulk` (zero value) | 2 | 10,000 | everything else |

The lanes have independent queues and workers, so a bulk backlog cannot consume HIGH capacity. A
full queue is refused immediately with `*QueueFullError`, which carries the lane, capacity and
`RetryAfter`; the caller goroutine is never parked. A message is never silently dropped by the SDK:
a fire-and-forget failure increments `Stats().Dropped`, and a result-bearing send gets the error on
its channel.

### Shutdown and guarantees

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
err := sender.Close(ctx) // idempotent; ErrDrainTimeout if the deadline was too short
```

`Close` (or an optional caller-side shutdown hook) does four things:

1. rejects new `Enqueue` calls with `ErrSenderClosed`;
2. waits for accepted jobs to settle, up to the context deadline;
3. cancels in-flight requests and fails queued jobs with `ErrSenderClosed` if the deadline expires;
4. stops all worker goroutines.

Every `EnqueueResult` channel receives exactly one `SendResult`; a fire-and-forget job is counted
in `Dropped`/`Failed`. `Sender.Close` does **not** close the shared `Client` or its transport.

| Method | Caller waits | Guarantee |
|---|---|---|
| `Client.Send` | Full hub round trip | Hub acceptance; returns `*APIError`/transport error |
| `Sender.Enqueue` | Only a queue offer | At-most-once; failures counted in `Dropped` |
| `Sender.EnqueueResult` | Only a queue offer | Result channel receives hub acceptance or a typed error |
| `EnqueueResult` + `IdempotencyKey` + retry | Only a queue offer | At-least-once from the SDK; duplicate-resistant when the hub honours the key |

**Delivery to the handset is unchanged.** The hub and provider still decide when the SMS arrives;
this removes the caller-side wait and goroutine/connection pressure, not delivery latency.

### Metrics and listener

`Sender.Stats()` returns:

| Field | Meaning |
|---|---|
| `Accepted` / `Completed` / `Failed` | send counters |
| `Retries` / `Rejected` / `Dropped` | retry, queue-full, and unobserved failures |
| `InFlight` | jobs currently in a worker |
| `HighQueueDepth` / `BulkQueueDepth` | instantaneous queue depth |

`SendListener` is the Go equivalent of the Java listener: `OnAccepted`, `OnCompleted`, `OnRetry`,
`OnRejected`, `OnDropped`. Callbacks run on worker goroutines, must be fast and thread-safe, and
are recovered if they panic. `OnRejected` runs on the goroutine that called `Enqueue`, so it must
also be fast. Without a listener, use `Stats()`; with OpenTelemetry, you can also instrument the
`http.RoundTripper` passed to `WithHTTPClient`.

---

## Idempotency, priority, and retries

### Idempotency

```go
key, err := notifier.NewIdempotencyKey() // 128-bit, 32 hex chars
```

Set the key in `SendParams.IdempotencyKey` (or `TemplateParams.IdempotencyKey`). The SDK sends it
both in the JSON body and in the `Idempotency-Key` header, so the hub can deduplicate once it
honours the header. A retry with the same key cannot deliver a second message when deduplication is
active. `Sender` generates a key when the caller leaves it empty and exposes it on
`SendResult.IdempotencyKey` and the listener's `SendEvent`.

### Priority

Set `SendParams.Priority` (or `TemplateParams.Priority`) to `PriorityHigh` for OTP. The SDK sends
`X-Priority: high`; `PriorityBulk` sends `X-Priority: bulk`. `Client.Bulk` is always the bulk lane
and never batches HIGH messages. The header only has an effect after the hub splits its queue by
priority; until then it is an ignored, harmless header.

### Retries

`Client.Send` never retries. `Sender` retries only when the failure is safe:

| Failure | `Client.Send` | `Sender` |
|---|---|---|
| DNS, TCP dial, TLS handshake | no | retries (pre-send, cannot duplicate) |
| HTTP 429 | no | retries, honouring `Retry-After`, then exponential backoff with jitter |
| HTTP 408 / 5xx | no | only when `RetryAmbiguous=true` **and** an idempotency key is set |
| Read timeout / connection reset after write | no | only when `RetryAmbiguous=true` **and** an idempotency key is set |
| 4xx (except 429) | no | never |

`RetryAmbiguous` defaults to `false`. Leave it off until the hub de-duplicates on
`Idempotency-Key`; then an ambiguous failure can be retried without creating a second SMS.
`APIError.RetryableSend(key)` exposes the same rule for callers that implement their own policy,
and `IsPreSendError(err)` identifies the always-safe transport failures.

### Hub coordination

Two wire features only take effect after the hub ships its side:

1. **`Idempotency-Key` de-duplication** on `POST /v1/notifications`. Until then, set
   `RetryAmbiguous=false` so Sender never retries a 408/5xx/read-timeout/connection-reset.
2. **`X-Priority: high|bulk`** and a separate OTP lane in the hub queue. Until then the header is
   ignored; `PriorityHigh` still keeps the message on the SDK's independent HIGH lane.

For `BULK`, `Client.Bulk`/`BulkConcurrent` already batch up to `MaxBulkMessages` per request. HIGH
messages are never batched by the SDK. A dedicated hub batch endpoint for bulk is a separate
coordination item; the Go SDK does not invent one.

### Bounded connections

```go
client, err := notifier.NewClient(apiKey,
    notifier.WithMaxConnsPerHost(64),
)
```

`WithMaxConnsPerHost` caps simultaneous connections in the SDK-owned transport. It cannot be
combined with `WithHTTPClient`, because the SDK never mutates a caller-supplied client. This is a
backstop for bursts against a slow hub; `Sender` worker counts provide the primary bound for
fire-and-forget traffic.

---

## Timeouts and context

Every client call takes a `context.Context`; its deadline and cancellation apply to the whole HTTP
request, including reading the response body. `WithTimeout` adds a client-side per-round-trip cap
(default 30s) on top; whichever expires first wins. For an OTP request path, use a short context
rather than the global default:

```go
ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
defer cancel()
_, err := client.SendTemplateByName(ctx, notifier.TemplateParams{...})
```

A `Sender` job is detached from the request context (`context.WithoutCancel`) so a fire-and-forget
send is not cancelled when the HTTP handler returns; `SenderOptions.SendTimeout` (default 10s)
bounds the whole job including retries. `Close` has its own context/deadline.

---

## Concurrency and performance

The client is designed for the way services actually call it: many goroutines, one client. The
optional `Sender` adds a fixed worker inventory on top when request-path fire-and-forget is needed.

- Every call takes a `context.Context`; cancellation and deadlines propagate into the HTTP request.
- Requests reuse a process-wide `http.Transport` derived from `http.DefaultTransport` with
  `MaxIdleConnsPerHost` raised from the stdlib's 2 to 32 — the stdlib default serializes concurrent
  calls to the same API host. Pass `WithHTTPClient` to opt out, or `WithMaxConnsPerHost` to cap
  simultaneous connections in the SDK-owned transport.
- `Sender` uses fixed worker counts and bounded queues; it never creates a goroutine per message.
- Response buffers come from a `sync.Pool`. Request bodies deliberately do not: bytes backing a
  request body must stay valid past `Do` on an early response, so borrowing them from a pool would
  be a data race waiting to happen.
- Response bodies are always drained, so keep-alive connections go back to the pool.
- Parameter validation and rendering (`SendParams.wire`) allocate nothing.

Measured on an Apple M1 Pro, Go 1.26 (loopback HTTP server, `go test -bench .`):

| Benchmark | Result |
|---|---|
| `BenchmarkSend` | 146 µs/op — full send: validate, encode, round trip, decode |
| `BenchmarkSendParallel` | 35 µs/op — tuned transport |
| `BenchmarkSendParallelStockTransport` | 51 µs/op — same code on the stock transport (~31% slower) |
| `BenchmarkSendParamsWire` | 26 ns/op, **0 allocs** |
| `BenchmarkPrepareBulk` (20 000 messages) | 72 µs/op, 3 allocs |

Against the production API, `BenchmarkLive_Status` measures ~24 ms/op on a warm connection
where the first, cold call costs DNS + TCP + TLS (~1 s from a developer network). Those numbers
are network-bound and vary with the link; the local benchmarks above are the ones that describe
the SDK itself.

---

## Development

```bash
make test        # go test ./...
make race        # go test -race ./...
make lint        # gofmt check, go vet, golangci-lint
make bench       # benchmarks
make coverage    # coverage report
```

The unit tests run against `httptest` servers and need no network. They include an end-to-end
flow (`Sender` + `Client` + status + close) and race/10k-load tests, all run by `make check`.
The live tests hit a real deployment and are **skipped** unless a key is configured:

```bash
# Read-only: history plus the 404 contract.
NOTIFIER_API_KEY=... go test -run TestLive -v ./...

# Dispatching is opt-in, because it sends real messages and costs money.
NOTIFIER_LIVE_SEND=1 NOTIFIER_LIVE_RECIPIENT="+989120000000" \
NOTIFIER_LIVE_TEMPLATE=verify NOTIFIER_LIVE_VALUES=123456 \
NOTIFIER_API_KEY=... go test -run TestLive_Send -v ./...
```

---

## Migrating from `kavenegar-java` / the Notifier Java SDK

There is deliberately **no Kavenegar facade** in this SDK: new Go code should use the Notifier
surface directly. The mapping is mechanical.

| Java SDK | Go SDK |
|---|---|
| `new NotifierApi(key)` | `notifier.NewClient(key)` |
| `api.sendTemplateByName(SMS, phone, "verify", "123456")` | `client.SendTemplateByName(ctx, notifier.TemplateParams{Channel: notifier.ChannelSMS, Recipient: phone, Template: "verify", Values: []string{"123456"}})` |
| `api.sendTemplateByName(SMS, phone, "verify", varsMap)` | same, with `Vars: map[string]any{…}` |
| `api.send(type, recipient, "body")` | `client.SendText(ctx, channel, recipient, "body")` |
| `api.bulk(SMS, list)` | `client.Bulk(ctx, notifier.BulkParams{Channel: notifier.ChannelSMS, Messages: …})` |
| `api.status(uuid)` | `client.Status(ctx, uuid)` |
| `api.statusByProviderMessageId(id)` | `client.StatusByProviderMessageID(ctx, id)` |
| `api.listNotifications(query)` | `client.History(ctx, notifier.HistoryQuery{…})` |
| `catch (ApiException e)` | `errors.Is(err, notifier.ErrConflict)` / `errors.As(err, &apiErr)` |
| `SendOptions.builder()…build()` | fields on `notifier.SendParams` |
| `api.verifyLookupAndForget(...)` / `verifyLookupAsync(...)` | `sender.Enqueue(...)` / `sender.EnqueueResult(...)` with `Priority: PriorityHigh` |

### Migrating a request-path send to `Sender`

1. Build one `Client` at startup, as today.
2. Build one `Sender` with it; choose queue sizes and workers for the service, and a `SendTimeout`
   matching the OTP budget (for example 5s).
3. Replace the blocking `client.Send`/`SendTemplateByName` in the handler with `sender.Enqueue` (or
   `EnqueueResult` when the notification id is needed).
4. Set `PriorityHigh`, an `IdempotencyKey`, and (once the hub supports it) `RetryAmbiguous=true`.
5. Handle only local `*QueueFullError`/`ErrSenderClosed` on the request path; observe network
   failures through `SendListener`/`Stats`.
6. Call `sender.Close` from your application shutdown sequence; the shared `Client` needs no close.
7. Roll out one handler at a time. `Client.Send` stays available and unchanged.

> **Compatibility note:** `SendParams` and `TemplateParams` gained a `Priority` field. Existing
> code that uses **keyed** struct literals (`notifier.SendParams{Channel: ...}`) is unaffected;
> unkeyed literals must be updated. Blocking calls otherwise keep their signatures and behavior.

The `"group:template"` shorthand, positional `template_params`, and the "explicit group wins over the
prefix" rule all behave exactly as they do in the Java SDK.

---

## License

Proprietary; owned by **SoftinoProducts**. Terms defined by the owner.
