package airwallex

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
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

func TestVerifySignature_Valid(t *testing.T) {
	payload := []byte(`{"id":"evt_1","name":"global_account.transaction.created"}`)
	timestamp := "1700000000"
	secret := "whsec_abc123"
	sig := signForTest(t, secret, payload, timestamp)

	if err := verifySignature(secret, payload, timestamp, sig); err != nil {
		t.Fatalf("verifySignature() error = %v, want nil", err)
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	timestamp := "1700000000"
	sig := signForTest(t, "whsec_abc123", payload, timestamp)

	if err := verifySignature("whsec_different", payload, timestamp, sig); err != ErrInvalidSignature {
		t.Fatalf("verifySignature() error = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerifySignature_TamperedPayload(t *testing.T) {
	payload := []byte(`{"id":"evt_1","amount":100}`)
	timestamp := "1700000000"
	secret := "whsec_abc123"
	sig := signForTest(t, secret, payload, timestamp)

	tampered := []byte(`{"id":"evt_1","amount":999999}`)
	if err := verifySignature(secret, tampered, timestamp, sig); err != ErrInvalidSignature {
		t.Fatalf("verifySignature() error = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerifySignature_MissingSecret(t *testing.T) {
	if err := verifySignature("", []byte("x"), "123", "deadbeef"); err != ErrMissingSecret {
		t.Fatalf("verifySignature() error = %v, want %v", err, ErrMissingSecret)
	}
}

func TestVerifySignature_InvalidHex(t *testing.T) {
	err := verifySignature("secret", []byte("x"), "123", "not-hex!!")
	if err == nil {
		t.Fatal("verifySignature() expected error for non-hex signature, got nil")
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
