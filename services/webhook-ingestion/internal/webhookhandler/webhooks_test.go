package webhookhandler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/eventbus"
)

// freshTimestamp is a valid "x-timestamp" value as of the moment it's
// called, so these HTTP-handler-level tests aren't rejected by
// internal/airwallex's clock-skew check (see webhook.go's ErrStaleTimestamp).
func freshTimestamp() string {
	return fmt.Sprintf("%d", time.Now().Unix())
}

type fakeLedger struct {
	deposits []*ledgerpb.Deposit
	marked   []*ledgerpb.MarkDepositReceivedRequest
}

func (f *fakeLedger) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	return f.deposits, nil
}

func (f *fakeLedger) MarkDepositReceived(ctx context.Context, req *ledgerpb.MarkDepositReceivedRequest) (*ledgerpb.Deposit, error) {
	f.marked = append(f.marked, req)
	return &ledgerpb.Deposit{Id: req.GetDepositId()}, nil
}

func sign(secret string, payload []byte, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHandleAirwallexWebhook_ValidTransactionCreated(t *testing.T) {
	secret := "whsec_test"
	ledger := &fakeLedger{
		deposits: []*ledgerpb.Deposit{
			{Id: "dep_1", TenantId: "tenant_1", ExpectedAmount: &commonpb.Money{MinorUnits: 50000, Currency: "USD"}, ExpectedAtUnix: 1000},
		},
	}
	bus := eventbus.NewBus()
	ch, unsubscribe := bus.Subscribe("tenant_1")
	defer unsubscribe()

	h := NewHandler(secret, ledger, bus, nil)

	payload, _ := json.Marshal(map[string]any{
		"id":   "evt_1",
		"name": "global_account.transaction.created",
		"data": map[string]any{
			"tenant_id":          "tenant_1",
			"transaction_id":     "tx_1",
			"amount_minor_units": 50000,
			"currency":           "USD",
			"occurred_at_unix":   1500,
		},
	})
	timestamp := freshTimestamp()
	sig := sign(secret, payload, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", timestamp)
	req.Header.Set("x-signature", sig)
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if len(ledger.marked) != 1 || ledger.marked[0].GetDepositId() != "dep_1" {
		t.Fatalf("marked = %+v, want one call for dep_1", ledger.marked)
	}

	select {
	case e := <-ch:
		if e.Kind != "deposit" {
			t.Errorf("Kind = %q, want deposit", e.Kind)
		}
	default:
		t.Error("expected an activity event to be published")
	}
}

// TestHandleAirwallexWebhook_ReferenceMatchRequiresAmountMatch
// reproduces a real over-crediting gap: before matchDepositByReference
// required amount equality, a transaction carrying a deposit's
// reference was credited for that deposit's FULL expected amount
// regardless of how much the transaction actually moved. Here a
// $5,000.00 deposit's reference arrives on a real $1.00 transaction —
// the deposit must NOT be marked received off of it (no other deposit
// matches $1.00 either, so this transaction has no safe match at all).
func TestHandleAirwallexWebhook_ReferenceMatchRequiresAmountMatch(t *testing.T) {
	secret := "whsec_test"
	ledger := &fakeLedger{
		deposits: []*ledgerpb.Deposit{
			{
				Id:             "dep_1",
				TenantId:       "tenant_1",
				ExternalRef:    "REF-100",
				ExpectedAmount: &commonpb.Money{MinorUnits: 500000, Currency: "USD"},
				ExpectedAtUnix: 1000,
			},
		},
	}
	h := NewHandler(secret, ledger, nil, nil)

	payload, _ := json.Marshal(map[string]any{
		"id":   "evt_1",
		"name": "global_account.transaction.created",
		"data": map[string]any{
			"tenant_id":          "tenant_1",
			"transaction_id":     "tx_1",
			"amount_minor_units": 100, // $1.00 — not dep_1's $5,000.00
			"currency":           "USD",
			"reference":          "REF-100", // but reuses dep_1's reference
			"occurred_at_unix":   1500,
		},
	})
	timestamp := freshTimestamp()
	sig := sign(secret, payload, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", timestamp)
	req.Header.Set("x-signature", sig)
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an unmatched transaction is not a hard error)", rec.Code)
	}
	if len(ledger.marked) != 0 {
		t.Fatalf("marked = %+v, want none — a $1.00 transaction must never credit a $5,000.00 deposit just because the reference string matched", ledger.marked)
	}
}

func TestHandleAirwallexWebhook_InvalidSignature(t *testing.T) {
	ledger := &fakeLedger{}
	h := NewHandler("whsec_test", ledger, nil, nil)

	payload := []byte(`{"id":"evt_1","name":"global_account.transaction.created","data":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", freshTimestamp())
	req.Header.Set("x-signature", "deadbeef")
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(ledger.marked) != 0 {
		t.Error("MarkDepositReceived should not be called for an invalid signature")
	}
}

func TestHandleAirwallexWebhook_NoMatchingDepositIsNotAnError(t *testing.T) {
	secret := "whsec_test"
	ledger := &fakeLedger{} // no deposits at all
	h := NewHandler(secret, ledger, nil, nil)

	payload, _ := json.Marshal(map[string]any{
		"id":   "evt_1",
		"name": "global_account.transaction.created",
		"data": map[string]any{
			"tenant_id":          "tenant_1",
			"amount_minor_units": 50000,
		},
	})
	timestamp := freshTimestamp()
	sig := sign(secret, payload, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", timestamp)
	req.Header.Set("x-signature", sig)
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no match should not be a hard error)", rec.Code)
	}
}

func TestHandleAirwallexWebhook_UnhandledEventTypeReturnsOK(t *testing.T) {
	secret := "whsec_test"
	h := NewHandler(secret, &fakeLedger{}, nil, nil)

	payload := []byte(`{"id":"evt_1","name":"issuing.card.updated","data":{}}`)
	timestamp := freshTimestamp()
	sig := sign(secret, payload, timestamp)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", timestamp)
	req.Header.Set("x-signature", sig)
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
