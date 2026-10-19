package airwallex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// signForTest computes a valid HMAC-SHA256 signature the same way the
// production code expects it, for use as test fixture data.
func signForTest(t *testing.T, secret string, payload []byte, timestamp string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// freshTimestamp is a valid "x-timestamp" value as of the moment it's
// called — the signature-only tests below care about the HMAC check, not
// the freshness check, so they all sign over a timestamp that is always
// within maxClockSkew of now().
func freshTimestamp() string {
	return fmt.Sprintf("%d", time.Now().Unix())
}

func TestVerifySignature_Valid(t *testing.T) {
	payload := []byte(`{"id":"evt_1","name":"global_account.transaction.created"}`)
	timestamp := freshTimestamp()
	secret := "whsec_abc123"
	sig := signForTest(t, secret, payload, timestamp)

	if err := VerifySignature(secret, payload, timestamp, sig); err != nil {
		t.Fatalf("VerifySignature() error = %v, want nil", err)
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	timestamp := freshTimestamp()
	sig := signForTest(t, "whsec_abc123", payload, timestamp)

	if err := VerifySignature("whsec_different", payload, timestamp, sig); err != ErrInvalidSignature {
		t.Fatalf("VerifySignature() error = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerifySignature_TamperedPayload(t *testing.T) {
	payload := []byte(`{"id":"evt_1","amount":100}`)
	timestamp := freshTimestamp()
	secret := "whsec_abc123"
	sig := signForTest(t, secret, payload, timestamp)

	tampered := []byte(`{"id":"evt_1","amount":999999}`)
	if err := VerifySignature(secret, tampered, timestamp, sig); err != ErrInvalidSignature {
		t.Fatalf("VerifySignature() error = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerifySignature_MissingSecret(t *testing.T) {
	if err := VerifySignature("", []byte("x"), "123", "deadbeef"); err != ErrMissingSecret {
		t.Fatalf("VerifySignature() error = %v, want %v", err, ErrMissingSecret)
	}
}

func TestVerifySignature_InvalidHex(t *testing.T) {
	err := VerifySignature("secret", []byte("x"), freshTimestamp(), "not-hex!!")
	if err == nil {
		t.Fatal("VerifySignature() expected error for non-hex signature, got nil")
	}
}

func TestVerifySignature_StaleTimestampRejected(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	secret := "whsec_abc123"
	timestamp := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	sig := signForTest(t, secret, payload, timestamp)

	err := VerifySignature(secret, payload, timestamp, sig)
	if err == nil || !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("VerifySignature() error = %v, want wrapping %v", err, ErrStaleTimestamp)
	}
}

func TestVerifySignature_FutureTimestampRejected(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	secret := "whsec_abc123"
	timestamp := fmt.Sprintf("%d", time.Now().Add(10*time.Minute).Unix())
	sig := signForTest(t, secret, payload, timestamp)

	err := VerifySignature(secret, payload, timestamp, sig)
	if err == nil || !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("VerifySignature() error = %v, want wrapping %v", err, ErrStaleTimestamp)
	}
}

func TestVerifySignature_TimestampAtSkewBoundaryAccepted(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	secret := "whsec_abc123"
	timestamp := fmt.Sprintf("%d", time.Now().Add(-4*time.Minute).Unix())
	sig := signForTest(t, secret, payload, timestamp)

	if err := VerifySignature(secret, payload, timestamp, sig); err != nil {
		t.Fatalf("VerifySignature() error = %v, want nil for a delivery within the allowed skew", err)
	}
}

func TestVerifySignature_NonNumericTimestampRejected(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	secret := "whsec_abc123"
	timestamp := "not-a-unix-timestamp"
	sig := signForTest(t, secret, payload, timestamp)

	err := VerifySignature(secret, payload, timestamp, sig)
	if err == nil || !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("VerifySignature() error = %v, want wrapping %v", err, ErrStaleTimestamp)
	}
}

func TestParseWebhookEnvelope(t *testing.T) {
	payload := []byte(`{"id":"evt_1","name":"global_account.transaction.created","created_at":"2026-10-19T00:00:00Z","data":{"tenant_id":"t1","amount_minor_units":500}}`)
	env, err := ParseWebhookEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseWebhookEnvelope() error = %v", err)
	}
	if env.Name != EventGlobalAccountTransactionCreated {
		t.Errorf("Name = %q, want %q", env.Name, EventGlobalAccountTransactionCreated)
	}

	var data GlobalAccountTransactionPayload
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.TenantID != "t1" || data.AmountMinorUnits != 500 {
		t.Errorf("data = %+v, want tenant_id=t1 amount=500", data)
	}
}
