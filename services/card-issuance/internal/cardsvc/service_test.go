package cardsvc

import (
	"context"
	"testing"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
)

type fakeLedger struct {
	tenant         *ledgerpb.Tenant
	allowed        bool
	headroom       int64
	appliedEntries []*ledgerpb.ApplyEntryRequest
	checkErr       error
	applyErr       error
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
	f.appliedEntries = append(f.appliedEntries, req)
	return &ledgerpb.ApplyEntryResponse{Entry: &ledgerpb.LedgerEntry{Id: "entry_test"}}, nil
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
