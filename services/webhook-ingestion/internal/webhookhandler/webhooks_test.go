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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/eventbus"
	"google.golang.org/protobuf/proto"
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

// raceLedger is a concurrency-aware fakeLedger, unlike the plain
// fakeLedger above (which always returns the same static slice
// regardless of what MarkDepositReceived has already done — fine for
// every other test in this file, all of which drive the handler
// sequentially). This feeds MarkDepositReceived's own writes back into
// what ListDeposits returns — the same live-feedback-loop fake
// services/card-issuance/internal/bridge's own concurrency regression
// test already established for the analogous TOCTOU race on
// RationBridgeFunding's velocity cap.
//
// The very first ListDeposits call sleeps briefly before returning,
// simulating the real network/DB latency a genuine ledger-core RPC
// would have — widening the TOCTOU window so a second, unserialized
// concurrent call reliably lands inside it instead of only
// occasionally under real goroutine scheduling. This is deliberately
// NOT a hard two-party rendezvous (unlike a channel-based barrier that
// waits for N arrivals before releasing any of them): with the fix's
// per-tenant lock in place, the second call never reaches ListDeposits
// until the first has fully finished (lock → list → match → mark →
// unlock), so a rendezvous that waited for both to *arrive* at
// ListDeposits before releasing either would deadlock forever against
// a correctly-serializing fix. A one-sided sleep has no such deadlock:
// it reproduces the race when nothing serializes the critical section,
// and is simply a harmless, already-resolved-by-the-lock wait when
// something does.
type raceLedger struct {
	mu       sync.Mutex
	deposits map[string]*ledgerpb.Deposit
	arrived  int32
}

func newRaceLedger(deposits []*ledgerpb.Deposit) *raceLedger {
	m := make(map[string]*ledgerpb.Deposit, len(deposits))
	for _, d := range deposits {
		m[d.GetId()] = cloneDeposit(d)
	}
	return &raceLedger{deposits: m}
}

func (r *raceLedger) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	r.mu.Lock()
	out := make([]*ledgerpb.Deposit, 0, len(r.deposits))
	for _, d := range r.deposits {
		out = append(out, cloneDeposit(d))
	}
	r.mu.Unlock()

	// The first call to reach here sleeps *after* taking its snapshot
	// (not before), simulating the network/DB latency between "read the
	// deposits" and "act on what was read" a real ledger-core RPC round
	// trip would have. Without a lock serializing the caller's whole
	// critical section, a second concurrent call can run its own
	// ListDeposits (and the match/mark that follows it) to completion
	// during this sleep, then this call still hands its caller the
	// stale, already-superseded snapshot it captured before sleeping —
	// reproducing the real race. With the fix's per-tenant lock, no
	// second call ever reaches this method until this one (and its own
	// match/mark) has fully finished, so this sleep is simply an inert
	// wait.
	if atomic.AddInt32(&r.arrived, 1) == 1 {
		time.Sleep(50 * time.Millisecond)
	}
	return out, nil
}

func (r *raceLedger) MarkDepositReceived(ctx context.Context, req *ledgerpb.MarkDepositReceivedRequest) (*ledgerpb.Deposit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.deposits[req.GetDepositId()]
	if !ok {
		return nil, fmt.Errorf("raceLedger: unknown deposit %s", req.GetDepositId())
	}
	if d.GetReceivedAtUnix() == 0 {
		d.ReceivedAtUnix = req.GetReceivedAtUnix()
		d.ExternalRef = req.GetExternalRef()
	}
	return cloneDeposit(d), nil
}

// cloneDeposit returns a deep copy of d via proto.Clone — generated
// proto message structs embed a protoimpl.MessageState (which contains
// a sync.Mutex), so a plain `cp := *d` shallow copy would copy that
// lock value; proto.Clone is the safe way to hand callers their own
// copy while raceLedger keeps the original under its own mutex. Same
// pattern services/card-issuance/internal/cardsvc's cloneCard already
// established.
func cloneDeposit(d *ledgerpb.Deposit) *ledgerpb.Deposit {
	return proto.Clone(d).(*ledgerpb.Deposit)
}

func (r *raceLedger) receivedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, d := range r.deposits {
		if d.GetReceivedAtUnix() != 0 {
			n++
		}
	}
	return n
}

// TestHandleAirwallexWebhook_ConcurrentSameAmountDepositsBothSettle
// reproduces a real fund-stranding gap, the same missing-lock class
// round 25's card-issuance fixes (lockCard/lockTenant) already closed
// for AdjustCardLimit/CloseCard and RationBridgeFunding: two genuinely
// concurrent webhook deliveries for the SAME tenant — plausible any
// time a tenant has two outstanding deposits of the same amount
// settling close together, since Go's net/http serves each request on
// its own goroutine — both call ListDeposits before either has called
// MarkDepositReceived, so both independently compute "the oldest
// not-yet-received deposit of this amount" and land on the SAME
// deposit (dep_1, the older one). ledger-core's own MarkDepositReceived
// is idempotent per deposit_id, so this never double-credits — but the
// second transaction's real, already-arrived money is then never
// matched to anything: dep_2 stays Expected forever, understating the
// tenant's available balance by its full amount, with nothing in this
// repo that ever revisits an Expected deposit once the webhook that
// should have settled it has already been consumed (and acknowledged
// 200 OK) by the wrong deposit.
func TestHandleAirwallexWebhook_ConcurrentSameAmountDepositsBothSettle(t *testing.T) {
	secret := "whsec_test"
	dep1 := &ledgerpb.Deposit{Id: "dep_1", TenantId: "tenant_1", ExpectedAmount: &commonpb.Money{MinorUnits: 50000, Currency: "USD"}, ExpectedAtUnix: 1000}
	dep2 := &ledgerpb.Deposit{Id: "dep_2", TenantId: "tenant_1", ExpectedAmount: &commonpb.Money{MinorUnits: 50000, Currency: "USD"}, ExpectedAtUnix: 2000}
	ledger := newRaceLedger([]*ledgerpb.Deposit{dep1, dep2})
	h := NewHandler(secret, ledger, nil, nil)

	makeReq := func(txID string, occurredAt int64) *http.Request {
		payload, _ := json.Marshal(map[string]any{
			"id":   "evt_" + txID,
			"name": "global_account.transaction.created",
			"data": map[string]any{
				"tenant_id":          "tenant_1",
				"transaction_id":     txID,
				"amount_minor_units": 50000,
				"currency":           "USD",
				"occurred_at_unix":   occurredAt,
			},
		})
		timestamp := freshTimestamp()
		sig := sign(secret, payload, timestamp)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", bytes.NewReader(payload))
		req.Header.Set("x-timestamp", timestamp)
		req.Header.Set("x-signature", sig)
		return req
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	txIDs := []string{"tx_1", "tx_2"}
	wg.Add(2)
	for i, txID := range txIDs {
		go func(i int, txID string) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.Mux().ServeHTTP(rec, makeReq(txID, 1500+int64(i)))
			codes[i] = rec.Code
		}(i, txID)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}

	if got := ledger.receivedCount(); got != 2 {
		t.Fatalf("receivedCount = %d, want 2 — both real transactions should each settle their own distinct deposit, not collide on the same oldest match", got)
	}
}
