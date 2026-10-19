// Package airwallex is webhook-ingestion's own copy of the gateway's
// former internal/airwallex/webhook.go (docs/adr/0004,
// docs/SCALE-EXPANSION.md section 1): HMAC-SHA256 signature verification
// and envelope parsing for Airwallex webhook deliveries. Unlike
// card-issuance's internal/airwallex package, this one has no Live/Mock
// Issuing-API client at all — webhook-ingestion never calls out to
// Airwallex, it only verifies and parses deliveries Airwallex pushes to
// it.
package airwallex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Airwallex signs webhook deliveries by HMAC-SHA256'ing the concatenation
// of the "x-timestamp" header and the raw request body with the account's
// webhook secret, hex-encoding the result, and sending it as
// "x-signature". See docs/AIRWALLEX-SETUP.md.

// ErrInvalidSignature is returned when a webhook's signature does not
// match the expected HMAC for the given secret.
var ErrInvalidSignature = errors.New("airwallex: invalid webhook signature")

// ErrMissingSecret is returned when signature verification is attempted
// without a configured webhook secret.
var ErrMissingSecret = errors.New("airwallex: webhook secret not configured")

// ErrStaleTimestamp is returned when a webhook's "x-timestamp" is further
// from the verifier's clock than maxClockSkew allows — either a genuinely
// stale/replayed delivery, or a delivery so far in the future it can't be
// trusted either. See docs/THREAT-MODEL.md's "Elevation of privilege" row:
// without this check, a captured, validly-signed webhook was replayable
// indefinitely by anyone who recorded it.
var ErrStaleTimestamp = errors.New("airwallex: webhook timestamp outside allowed clock skew")

// maxClockSkew bounds how far a delivery's "x-timestamp" may drift from
// this process's clock in either direction before it's rejected as stale,
// matching Airwallex's own documented delivery tolerance.
const maxClockSkew = 5 * time.Minute

// now is overridable in tests; defaults to time.Now.
var now = time.Now

// VerifySignature re-derives the expected HMAC-SHA256 signature for
// (timestamp + payload) under secret and compares it against signature
// using a constant-time comparison, then rejects the delivery if
// timestamp is further than maxClockSkew from the current time.
func VerifySignature(secret string, payload []byte, timestamp string, signature string) error {
	if secret == "" {
		return ErrMissingSecret
	}
	if signature == "" {
		return fmt.Errorf("%w: empty signature header", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not valid hex", ErrInvalidSignature)
	}
	expectedBytes, _ := hex.DecodeString(expected)
	if !hmac.Equal(sigBytes, expectedBytes) {
		return ErrInvalidSignature
	}

	deliveredUnix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: timestamp %q is not a valid unix time", ErrStaleTimestamp, timestamp)
	}
	skew := now().Sub(time.Unix(deliveredUnix, 0))
	if skew > maxClockSkew || skew < -maxClockSkew {
		return fmt.Errorf("%w: delivery timestamp is %s from now", ErrStaleTimestamp, skew)
	}
	return nil
}

// WebhookEventType enumerates the Airwallex webhook event names
// webhook-ingestion acts on.
type WebhookEventType string

const (
	EventGlobalAccountTransactionCreated WebhookEventType = "global_account.transaction.created"
	EventIssuingCardUpdated              WebhookEventType = "issuing.card.updated"
)

// WebhookEnvelope is the top-level shape of every Airwallex webhook
// delivery body.
type WebhookEnvelope struct {
	ID        string           `json:"id"`
	Name      WebhookEventType `json:"name"`
	CreatedAt string           `json:"created_at"`
	Data      json.RawMessage  `json:"data"`
}

// GlobalAccountTransactionPayload is the Data payload for
// EventGlobalAccountTransactionCreated — a deposit landing in a tenant's
// global account, which is how a late deposit gets marked received.
type GlobalAccountTransactionPayload struct {
	TenantID         string `json:"tenant_id"`
	TransactionID    string `json:"transaction_id"`
	AmountMinorUnits int64  `json:"amount_minor_units"`
	Currency         string `json:"currency"`
	OccurredAtUnix   int64  `json:"occurred_at_unix"`
}

// ParseWebhookEnvelope unmarshals a verified webhook body into its
// envelope. Callers must verify the signature before trusting any field.
func ParseWebhookEnvelope(payload []byte) (*WebhookEnvelope, error) {
	var env WebhookEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("airwallex: malformed webhook payload: %w", err)
	}
	return &env, nil
}
