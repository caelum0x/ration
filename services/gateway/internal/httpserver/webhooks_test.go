package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/airwallex"
)

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
	aw := airwallex.NewMock(secret)
	ledger := &fakeLedger{
		deposits: []*ledgerpb.Deposit{
			{Id: "dep_1", TenantId: "tenant_1", ExpectedAmount: &commonpb.Money{MinorUnits: 50000, Currency: "USD"}, ExpectedAtUnix: 1000},
		},
	}
	bus := activity.NewBus()
	ch, unsubscribe := bus.Subscribe("tenant_1")
	defer unsubscribe()

	h := NewHandler(aw, ledger, bus, nil)

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
	timestamp := "1700000000"
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

func TestHandleAirwallexWebhook_InvalidSignature(t *testing.T) {
	aw := airwallex.NewMock("whsec_test")
	ledger := &fakeLedger{}
	h := NewHandler(aw, ledger, nil, nil)

	payload := []byte(`{"id":"evt_1","name":"global_account.transaction.created","data":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
	req.Header.Set("x-timestamp", "1700000000")
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
	aw := airwallex.NewMock(secret)
	ledger := &fakeLedger{} // no deposits at all
	h := NewHandler(aw, ledger, nil, nil)

	payload, _ := json.Marshal(map[string]any{
		"id":   "evt_1",
		"name": "global_account.transaction.created",
		"data": map[string]any{
			"tenant_id":          "tenant_1",
			"amount_minor_units": 50000,
		},
	})
	timestamp := "1700000000"
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
	aw := airwallex.NewMock(secret)
	h := NewHandler(aw, &fakeLedger{}, nil, nil)

	payload := []byte(`{"id":"evt_1","name":"issuing.card.updated","data":{}}`)
	timestamp := "1700000000"
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
