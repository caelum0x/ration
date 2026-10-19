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

// VerifySignature re-derives the expected HMAC-SHA256 signature for
// (timestamp + payload) under secret and compares it against signature
// using a constant-time comparison.
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
