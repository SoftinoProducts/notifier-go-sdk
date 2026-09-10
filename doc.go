// Package notifier is a client for the Notifier multi-channel notification hub.
//
// Notifier is channel-agnostic: the caller picks a channel type (sms, email, webpush,
// bale, or any type the tenant has configured), and the service resolves the concrete
// provider from the tenant's channel configuration. This SDK never chooses a provider.
//
// # Quick start
//
//	client, err := notifier.NewClient(os.Getenv("NOTIFIER_API_KEY"))
//	if err != nil {
//		return err
//	}
//
//	res, err := client.SendTemplateByName(ctx, notifier.TemplateParams{
//		Channel:   notifier.ChannelSMS,
//		Recipient: "+989120000000",
//		Template:  "verify",
//		Values:    []string{"123456"},
//	})
//	if err != nil {
//		return err
//	}
//	fmt.Println(res.ID, res.Status)
//
// # Design
//
//   - Zero third-party dependencies. Only the standard library is used, which keeps
//     go.mod empty for consumers and the dependency graph audit surface at nothing.
//   - Every call takes a context.Context and respects its deadline and cancellation.
//   - A Client is safe for concurrent use by multiple goroutines and is intended to be
//     constructed once and reused for the lifetime of the process.
//   - Notable sends (Send, Bulk) are never retried automatically. The service is
//     asynchronous: a duplicated request means a duplicated message. Callers that want
//     retries should set SendParams.IdempotencyKey so the service can deduplicate them.
//   - Errors are typed. Use errors.Is with the sentinel errors (ErrNotFound,
//     ErrUnauthorized, ErrConflict, ErrRateLimited, ...) or errors.As with *APIError to
//     reach the HTTP status, the request id, and Retry-After.
//
// # Performance
//
// The client reuses a process-wide HTTP transport with connection pooling tuned for
// concurrent calls, recycles response buffers through a sync.Pool, and always drains
// response bodies so keep-alive connections are returned to the pool. Parameter
// validation and request rendering allocate nothing. Bulk sends are chunked to the
// service limit and can be fanned out with bounded concurrency via Client.BulkConcurrent.
//
// # Receiving callbacks
//
// Notifier signs delivery callbacks with HMAC-SHA256 in the X-Notifier-Signature header.
// Use SignatureVerifier to authenticate them, for example:
//
//	verifier, _ := notifier.NewSignatureVerifier(os.Getenv("NOTIFIER_WEBHOOK_SECRET"))
//	http.Handle("/notifier/callback", verifier.Handler(myHandler))
package notifier
