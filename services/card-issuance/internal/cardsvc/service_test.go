package cardsvc

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
)

type fakeLedger struct {
	tenant   *ledgerpb.Tenant
	allowed  bool
	headroom int64
	checkErr error
	applyErr error

	// mu guards appliedEntries: Round 24's own concurrency test fires
	// genuinely concurrent ApplyEntry calls against this fake, so a
	// plain unsynchronized append here would itself be a data race
	// under `go test -race`, independent of whatever cardsvc.Service
	// does or doesn't lock.
	mu             sync.Mutex
	appliedEntries []*ledgerpb.ApplyEntryRequest
}

func (f *fakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	return f.tenant, nil
}

func (f *fakeLedger) CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledgerpb.CheckReserveFloorResponse, error) {
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return &ledgerpb.CheckReserveFloorResponse{Allowed: f.allowed, HeadroomMinorUnits: f.headroom}, nil
}

func (f *fakeLedger) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	f.mu.Lock()
	f.appliedEntries = append(f.appliedEntries, req)
	f.mu.Unlock()
	return &ledgerpb.ApplyEntryResponse{Entry: &ledgerpb.LedgerEntry{Id: "entry_test"}}, nil
}

func (f *fakeLedger) entries() []*ledgerpb.ApplyEntryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*ledgerpb.ApplyEntryRequest, len(f.appliedEntries))
	copy(out, f.appliedEntries)
	return out
}

func TestIssueCard_Success(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 10000,
	}
	svc := New(aw, ledger)

	card, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 5000,
		LimitInterval:   "daily",
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}
	if card.GetStatus() != commonpb.CardStatus_CARD_STATUS_ACTIVE {
		t.Errorf("Status = %v, want ACTIVE", card.GetStatus())
	}
	if len(ledger.appliedEntries) != 1 {
		t.Fatalf("len(appliedEntries) = %d, want 1", len(ledger.appliedEntries))
	}
	if ledger.appliedEntries[0].GetType() != commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD {
		t.Errorf("entry type = %v, want HOLD", ledger.appliedEntries[0].GetType())
	}

	got, err := svc.GetCard(context.Background(), card.GetId())
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.GetCardholderName() != "Ada Lovelace" {
		t.Errorf("CardholderName = %q, want Ada Lovelace", got.GetCardholderName())
	}
}

func TestIssueCard_InsufficientHeadroomDoesNotPersistOrHold(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  false,
		headroom: 100,
	}
	svc := New(aw, ledger)

	_, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Grace Hopper",
		LimitMinorUnits: 5000,
		Currency:        "USD",
	})
	if err == nil {
		t.Fatal("IssueCard() expected error for insufficient headroom, got nil")
	}
	if len(ledger.appliedEntries) != 0 {
		t.Errorf("len(appliedEntries) = %d, want 0", len(ledger.appliedEntries))
	}
	cards, _ := svc.ListCards(context.Background(), "tenant_1")
	if len(cards) != 0 {
		t.Errorf("len(cards) = %d, want 0 (card should not be persisted)", len(cards))
	}
}

func TestIssueCard_CurrencyMismatchRejectedUpfront(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 10000,
	}
	svc := New(aw, ledger)

	// A zero-limit card never reaches CheckReserveFloor/ApplyEntry at
	// all (see IssueCard's own early-return for LimitMinorUnits == 0),
	// which is exactly the case that let a mismatched --currency slip
	// through before this test existed: ApplyEntry's currency check
	// (the only thing that used to catch this) is never even called.
	_, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 0,
		Currency:        "EUR",
	})
	if err == nil {
		t.Fatal("IssueCard() expected error for currency mismatch, got nil")
	}
	cards, _ := svc.ListCards(context.Background(), "tenant_1")
	if len(cards) != 0 {
		t.Errorf("len(cards) = %d, want 0 (mismatched-currency card should never be created)", len(cards))
	}

	// Confirm no Airwallex card was ever created for the rejected
	// request either (not just that card-issuance's own metadata store
	// stayed empty): Mock's card IDs are a monotonic counter, so a
	// subsequent valid issuance getting id 1 proves the rejected call
	// above never reached aw.IssueCard.
	validCard, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 0,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() with matching currency error = %v", err)
	}
	if got, want := validCard.GetAirwallexCardId(), "aw_card_mock_000001"; got != want {
		t.Errorf("AirwallexCardId = %q, want %q (a prior mismatched-currency call must not have created an Airwallex card)", got, want)
	}
}

func TestAdjustCardLimit_IncreaseChecksHeadroom(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 10000,
	}
	svc := New(aw, ledger)

	card, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 1000,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	updated, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
		CardId:             card.GetId(),
		NewLimitMinorUnits: 3000,
		Reason:             "tenant requested increase",
	})
	if err != nil {
		t.Fatalf("AdjustCardLimit() error = %v", err)
	}
	if updated.GetLimitMinorUnits() != 3000 {
		t.Errorf("LimitMinorUnits = %d, want 3000", updated.GetLimitMinorUnits())
	}
	// One HOLD at issuance + one HOLD for the +2000 increase.
	if len(ledger.appliedEntries) != 2 {
		t.Fatalf("len(appliedEntries) = %d, want 2", len(ledger.appliedEntries))
	}
}

func TestAdjustCardLimit_DecreaseReleasesHoldAndMarksRationed(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 10000,
	}
	svc := New(aw, ledger)

	card, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 3000,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	updated, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
		CardId:             card.GetId(),
		NewLimitMinorUnits: 1000,
		Reason:             "bridge rationing",
	})
	if err != nil {
		t.Fatalf("AdjustCardLimit() error = %v", err)
	}
	if updated.GetStatus() != commonpb.CardStatus_CARD_STATUS_RATIONED {
		t.Errorf("Status = %v, want RATIONED after a limit decrease", updated.GetStatus())
	}
	last := ledger.appliedEntries[len(ledger.appliedEntries)-1]
	if last.GetType() != commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE {
		t.Errorf("last entry type = %v, want RELEASE", last.GetType())
	}
	if last.GetAmount().GetMinorUnits() != 2000 {
		t.Errorf("release amount = %d, want 2000", last.GetAmount().GetMinorUnits())
	}
}

func TestAdjustCardLimit_NotFound(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{}
	svc := New(aw, ledger)

	_, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
		CardId:             "does-not-exist",
		NewLimitMinorUnits: 100,
	})
	if err == nil {
		t.Fatal("AdjustCardLimit() expected error for unknown card, got nil")
	}
}

// TestAdjustCardLimit_ConcurrentIncreasesStayConsistentWithLedger is
// Round 24's regression test for the race lockCard's doc comment
// documents and this round's BUILD-LOG/plan entry reproduced live
// against the real running stack: two concurrent AdjustCardLimit calls
// on the SAME card, both increasing the limit, must leave
// card.LimitMinorUnits exactly equal to the card's original limit plus
// the sum of every HOLD amount ledger-core actually applied for it —
// never diverging to just one call's absolute new_limit_minor_units
// while the other's delta silently vanishes from the cached field
// (even though ledger-core itself still holds it, unreachably, since
// CloseCard/a later decrease only ever release
// card.GetLimitMinorUnits()).
func TestAdjustCardLimit_ConcurrentIncreasesStayConsistentWithLedger(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 1_000_000,
	}
	svc := New(aw, ledger)

	const initialLimit = int64(100_000)
	card, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Concurrent Adjuster",
		LimitMinorUnits: initialLimit,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	newLimits := []int64{200_000, 300_000}
	var wg sync.WaitGroup
	errs := make([]error, len(newLimits))
	for i, newLimit := range newLimits {
		wg.Add(1)
		go func(idx int, limit int64) {
			defer wg.Done()
			_, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
				CardId:             card.GetId(),
				NewLimitMinorUnits: limit,
				Reason:             "concurrent-increase",
			})
			errs[idx] = err
		}(i, newLimit)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("AdjustCardLimit() call %d error = %v", i, err)
		}
	}

	// Sum every HOLD (issuance + both increases) and every RELEASE
	// ledger-core actually saw for this card's reference — this is
	// what the tenant's real held balance for this card is, regardless
	// of what cardsvc's own cached field says.
	var netHeld int64
	for _, e := range ledger.entries() {
		switch e.GetType() {
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD:
			netHeld += e.GetAmount().GetMinorUnits()
		case commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE:
			netHeld -= e.GetAmount().GetMinorUnits()
		}
	}

	got, err := svc.GetCard(context.Background(), card.GetId())
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.GetLimitMinorUnits() != netHeld {
		t.Errorf("card.LimitMinorUnits = %d, want %d (== sum of every HOLD/RELEASE ledger-core actually applied for this card) — "+
			"a mismatch means real held funds are now unreachable by any future AdjustCardLimit/CloseCard on this card",
			got.GetLimitMinorUnits(), netHeld)
	}
}

func TestListCards_FiltersByTenant(t *testing.T) {
	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 10000,
	}
	svc := New(aw, ledger)

	if _, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "A", LimitMinorUnits: 100, Currency: "USD",
	}); err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}
	if _, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId: "tenant_2", CardholderName: "B", LimitMinorUnits: 100, Currency: "USD",
	}); err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	cards, err := svc.ListCards(context.Background(), "tenant_1")
	if err != nil {
		t.Fatalf("ListCards() error = %v", err)
	}
	if len(cards) != 1 || cards[0].GetTenantId() != "tenant_1" {
		t.Errorf("ListCards(tenant_1) = %+v, want exactly one tenant_1 card", cards)
	}
}

// TestPersist_ConcurrentDifferentCardsDoesNotLoseEitherUpdate reproduces
// persist()'s lost-update race: AdjustCardLimit on card A and
// AdjustCardLimit on card B are two DIFFERENT cards, so cardLocks
// deliberately lets them run concurrently — but persist() rewrites the
// ENTIRE on-disk file from a full snapshot every time it runs, so
// whichever call's disk write lands LAST wins, regardless of which
// call's snapshot was actually more recent. Without persistMu
// serializing persist()'s snapshot-then-write end to end, card A's
// persist() call can take its snapshot BEFORE card B's own update
// lands, then (paused here via beforeSaveForTest) finish writing that
// stale snapshot to disk AFTER card B's own persist() call has already
// written the correct, fully up-to-date file — permanently reverting
// card B's already-"successful" limit change the moment it hits disk,
// even though ledger-core's real Held balance already reflects it.
//
// Confirmed via falsification: temporarily removing persistMu.Lock()/
// Unlock() from persist() makes this test fail on every run (card B's
// persisted limit reverts to its pre-adjustment value); restoring them
// makes it pass reliably.
func TestPersist_ConcurrentDifferentCardsDoesNotLoseEitherUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cards.json")

	aw := airwallex.NewMock()
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  true,
		headroom: 1_000_000,
	}
	svc := New(aw, ledger)
	if err := svc.EnablePersistence(path); err != nil {
		t.Fatalf("EnablePersistence() error = %v", err)
	}

	cardA, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "Card A", LimitMinorUnits: 1000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard(A) error = %v", err)
	}
	cardB, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "Card B", LimitMinorUnits: 1000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard(B) error = %v", err)
	}

	var hookCalls int32
	release := make(chan struct{})
	reachedHook := make(chan struct{})
	svc.beforeSaveForTest = func() {
		// Only the FIRST call through persist() (card A's own, below)
		// pauses here — card B's own persist() call must proceed
		// straight through once it gets its turn, exactly as it would
		// in production.
		if atomic.AddInt32(&hookCalls, 1) == 1 {
			close(reachedHook)
			<-release
		}
	}

	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		if _, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
			CardId: cardA.GetId(), NewLimitMinorUnits: 2000,
		}); err != nil {
			t.Errorf("AdjustCardLimit(A) error = %v", err)
		}
	}()

	<-reachedHook // card A's persist() has snapshotted and is now paused.

	var wgB sync.WaitGroup
	wgB.Add(1)
	go func() {
		defer wgB.Done()
		if _, err := svc.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
			CardId: cardB.GetId(), NewLimitMinorUnits: 3000,
		}); err != nil {
			t.Errorf("AdjustCardLimit(B) error = %v", err)
		}
	}()

	close(release) // let card A's paused persist() finish writing.
	wgA.Wait()
	wgB.Wait()

	loaded, err := loadCardsFile(path)
	if err != nil {
		t.Fatalf("loadCardsFile() error = %v", err)
	}
	if got := loaded[cardA.GetId()].GetLimitMinorUnits(); got != 2000 {
		t.Errorf("persisted card A limit = %d, want 2000", got)
	}
	if got := loaded[cardB.GetId()].GetLimitMinorUnits(); got != 3000 {
		t.Errorf("persisted card B limit = %d, want 3000 (card B's already-successful "+
			"adjustment was lost from disk by card A's concurrent, stale persist() write)",
			got)
	}
}
