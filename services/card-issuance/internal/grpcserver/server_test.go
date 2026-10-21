package grpcserver

import (
	"context"
	"testing"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
	"github.com/caelum0x/ration/services/card-issuance/internal/cardsvc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeLedger is a minimal cardsvc.LedgerClient stub — these tests never
// exercise a path that calls it, but cardsvc.New requires one.
type fakeLedger struct{}

func (fakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	return &ledgerpb.Tenant{Id: tenantID, Currency: "USD"}, nil
}

func (fakeLedger) CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledgerpb.CheckReserveFloorResponse, error) {
	return &ledgerpb.CheckReserveFloorResponse{Allowed: true}, nil
}

func (fakeLedger) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	return &ledgerpb.ApplyEntryResponse{Entry: &ledgerpb.LedgerEntry{Id: "entry_test"}}, nil
}

// TestGetCard_NotFound_ReturnsNotFoundStatus guards against the
// precise gap documented in services/gateway/internal/httpapi/cards.go's
// handleGetCard TODO: an unknown card ID must surface as a gRPC
// status.Error(codes.NotFound, ...), not a plain wrapped error that the
// gRPC runtime downgrades to codes.Unknown (and the REST facade in turn
// serves as a 500 instead of a 404).
func TestGetCard_NotFound_ReturnsNotFoundStatus(t *testing.T) {
	cards := cardsvc.New(airwallex.NewMock(), fakeLedger{})
	srv := New(cards, nil)

	_, err := srv.GetCard(context.Background(), &cardissuancepb.GetCardRequest{CardId: "does-not-exist"})
	if err == nil {
		t.Fatal("GetCard() error = nil, want codes.NotFound")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("GetCard() error = %v, not a gRPC status error", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("GetCard() code = %v, want %v", st.Code(), codes.NotFound)
	}
}

// TestAdjustCardLimit_NotFound_ReturnsNotFoundStatus is the same
// guarantee for AdjustCardLimit, which hits the identical
// airwallex.ErrNotFound path in cardsvc.Service for an unknown card ID.
func TestAdjustCardLimit_NotFound_ReturnsNotFoundStatus(t *testing.T) {
	cards := cardsvc.New(airwallex.NewMock(), fakeLedger{})
	srv := New(cards, nil)

	_, err := srv.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
		CardId:             "does-not-exist",
		NewLimitMinorUnits: 100,
	})
	if err == nil {
		t.Fatal("AdjustCardLimit() error = nil, want codes.NotFound")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("AdjustCardLimit() error = %v, not a gRPC status error", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("AdjustCardLimit() code = %v, want %v", st.Code(), codes.NotFound)
	}
}
