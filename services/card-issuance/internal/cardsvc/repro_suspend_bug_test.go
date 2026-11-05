package cardsvc

import (
	"context"
	"errors"
	"testing"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
)

// failingSuspendClient wraps a real airwallex.Client and forces every
// SetCardControls call to fail, simulating a transient Airwallex outage
// at exactly the moment suspendAfterFailure tries to zero out a card's
// limit after a reserve-floor failure.
type failingSuspendClient struct {
	airwallex.Client
	lastCardID string
}

func (f *failingSuspendClient) SetCardControls(ctx context.Context, cardID string, controls airwallex.CardControls) (*airwallex.Card, error) {
	f.lastCardID = cardID
	return nil, errors.New("simulated airwallex outage during suspend")
}

func TestReproSuspendAfterFailureSilentlyLeavesCardLiveAtFullLimit(t *testing.T) {
	realMock := airwallex.NewMock()
	aw := &failingSuspendClient{Client: realMock}
	ledger := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"},
		allowed:  false, // reserve-floor check fails -> suspendAfterFailure path
		headroom: 100,
	}
	svc := New(aw, ledger)

	_, err := svc.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Grace Hopper",
		LimitMinorUnits: 500000, // $5,000.00
		Currency:        "USD",
	})
	if err == nil {
		t.Fatal("IssueCard() expected error for insufficient headroom, got nil")
	}
	if aw.lastCardID == "" {
		t.Fatal("SetCardControls was never called — suspend path not exercised")
	}

	// The real Airwallex-side card (via the wrapped mock) is queried
	// directly, bypassing cardsvc's own in-memory map entirely (which
	// never even learned this card's id, since IssueCard returned an
	// error before constructing a *cardissuancepb.Card). This is the
	// operator's actual exposure: a real, live card at Airwallex with
	// its full requested spend limit still active.
	liveCard, err := realMock.GetCard(context.Background(), aw.lastCardID)
	if err != nil {
		t.Fatalf("GetCard() on the real (mock) Airwallex side: %v", err)
	}
	if liveCard.LimitMinorUnits == 0 {
		t.Fatalf("BUG NOT REPRODUCED: live card limit is 0 (suspended) despite SetCardControls failing")
	}
	t.Logf("BUG REPRODUCED: live Airwallex card %s still has limit %d minor units after a reserve-floor failure, because suspendAfterFailure's error was silently discarded (_, _ = ...)", aw.lastCardID, liveCard.LimitMinorUnits)
}
