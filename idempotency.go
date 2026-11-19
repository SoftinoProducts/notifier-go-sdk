package notifier

import (
	"crypto/rand"
	"encoding/hex"
)

// NewIdempotencyKey returns a random, opaque idempotency key.
//
// The key is 128 bits from crypto/rand, hex-encoded (32 characters). It contains no
// tenant data and is safe to log. Put it in SendParams.IdempotencyKey; the SDK sends it
// both in the body and in the Idempotency-Key header. The hub can deduplicate retries only
// once it honours that header.
//
//	key, err := notifier.NewIdempotencyKey()
//	if err != nil { ... }
//	_, err = client.Send(ctx, notifier.SendParams{..., IdempotencyKey: key})
func NewIdempotencyKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
