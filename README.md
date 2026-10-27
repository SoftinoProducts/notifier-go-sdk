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

```go
_, err := client.Status(ctx, id)

var apiErr *notifier.APIError
switch {
case errors.Is(err, notifier.ErrNotFound):
    // unknown id
case errors.Is(err, notifier.ErrRateLimited):
    time.Sleep(err.(*notifier.APIError).RetryAfter) // or errors.As
case errors.As(err, &apiErr):
    log.Printf("%s %s -> %d: %s (request id %s)",
        apiErr.Method, apiErr.Path, apiErr.StatusCode, apiErr.Message, apiErr.RequestID)
}
```

`APIError` also carries `Retryable()`, and the `X-Request-Id` from the response so a support request
can be tied to the exact server-side log line.

**The SDK never retries a send on its own.** Repeating a request risks delivering the message twice,
which is worse than not delivering it at all. If you want retries, set `IdempotencyKey` — the
service then answers `409 Conflict` instead of accepting a duplicate — and retry on
`APIError.Retryable()` yourself.

---

## Receiving callbacks

Notifier signs delivery callbacks with HMAC-SHA256 over the exact request body and sends the digest
in `X-Notifier-Signature` as `sha256=<hex>`. `SignatureVerifier` authenticates them and restores the
body for your handler:

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

Requests with a missing, malformed or wrong signature get a `401` and never reach your handler. The
comparison is constant time. For a body you already have (a queue consumer, another framework), use
the one-shot `notifier.VerifySignature(secret, body, header)`.

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
    notifier.WithUserAgent("trade-hub/1.4"),                    // default: notifier-go-sdk/<version> <go version>
)
```

A `Client` is safe for concurrent use and holds no resources that need closing: build **one per API
key** at startup and share it. `NewClient` returns an error for a blank API key or an unusable base
URL, so misconfiguration fails at boot rather than on the first message.

---

## Concurrency and performance

The client is designed for the way services actually call it: many goroutines, one client.

- Every call takes a `context.Context`; cancellation and deadlines propagate into the HTTP request.
- Requests reuse a process-wide `http.Transport` derived from `http.DefaultTransport` with
  `MaxIdleConnsPerHost` raised from the stdlib's 2 to 32 — the stdlib default serializes concurrent
  calls to the same API host. Pass `WithHTTPClient` to opt out.
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

The unit tests run against `httptest` servers and need no network. The live tests hit a real
deployment and are **skipped** unless a key is configured:

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

The `"group:template"` shorthand, positional `template_params`, and the "explicit group wins over the
prefix" rule all behave exactly as they do in the Java SDK.

---

## License

Proprietary; owned by **SoftinoProducts**. Terms defined by the owner.
