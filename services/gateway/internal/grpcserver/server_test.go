package grpcserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"google.golang.org/grpc/metadata"
)

// fakeLedger implements the narrow LedgerClient interface SimulateDeposit
// needs, with no gRPC and no network.
type fakeLedger struct {
	tenant *ledgerpb.Tenant

	recordedDeposits []*ledgerpb.RecordDepositRequest
}

func (f *fakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	return f.tenant, nil
}
func (f *fakeLedger) RecordDeposit(ctx context.Context, req *ledgerpb.RecordDepositRequest) (*ledgerpb.Deposit, error) {
	f.recordedDeposits = append(f.recordedDeposits, req)
	return &ledgerpb.Deposit{
		Id:             "dep_test",
		TenantId:       req.GetTenantId(),
		ExpectedAmount: req.GetExpectedAmount(),
		ExpectedAtUnix: req.GetExpectedAtUnix(),
		ExternalRef:    req.GetExternalRef(),
	}, nil
}

// fakeCardIssuance implements the narrow CardIssuanceClient interface
// server.go depends on, with no gRPC and no network — this is the one
// seam the section-1 split added (docs/SCALE-EXPANSION.md), standing in
// for a real services/card-issuance dial the way fakeLedger stands in
// for a real ledger-core dial.
type fakeCardIssuance struct {
	cards      map[string]*cardissuancepb.Card
	nextCardID int

	bridgeGrantedMinorUnits int64
	bridgeFullyGranted      bool
	bridgeExplanation       string
}

func (f *fakeCardIssuance) IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error) {
	if f.cards == nil {
		f.cards = map[string]*cardissuancepb.Card{}
	}
	f.nextCardID++
	c := &cardissuancepb.Card{
		Id:              fmt.Sprintf("card_%d", f.nextCardID),
		TenantId:        req.GetTenantId(),
		CardholderName:  req.GetCardholderName(),
		LimitMinorUnits: req.GetLimitMinorUnits(),
		LimitInterval:   req.GetLimitInterval(),
		Currency:        req.GetCurrency(),
	}
	f.cards[c.Id] = c
	return c, nil
}

func (f *fakeCardIssuance) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	c, ok := f.cards[req.GetCardId()]
	if !ok {
		return nil, fmt.Errorf("card %s not found", req.GetCardId())
	}
	c.LimitMinorUnits = req.GetNewLimitMinorUnits()
	return c, nil
}

func (f *fakeCardIssuance) GetCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error) {
	c, ok := f.cards[cardID]
	if !ok {
		return nil, fmt.Errorf("card %s not found", cardID)
	}
	return c, nil
}

func (f *fakeCardIssuance) ListCards(ctx context.Context, tenantID string) ([]*cardissuancepb.Card, error) {
	var out []*cardissuancepb.Card
	for _, c := range f.cards {
		if c.GetTenantId() == tenantID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeCardIssuance) RationBridgeFunding(ctx context.Context, req *cardissuancepb.RationBridgeFundingRequest) (*cardissuancepb.RationBridgeFundingResponse, error) {
	granted := f.bridgeGrantedMinorUnits
	if granted == 0 {
		granted = req.GetRequestedMinorUnits()
	}
	explanation := f.bridgeExplanation
	if explanation == "" {
		explanation = "granted in full (fake)"
	}
	return &cardissuancepb.RationBridgeFundingResponse{
		GrantedMinorUnits: granted,
		FullyGranted:      f.bridgeFullyGranted || granted == req.GetRequestedMinorUnits(),
		Explanation:       explanation,
	}, nil
}

func newTestServer(ledger *fakeLedger, cards *fakeCardIssuance) *Server {
	bus := activity.NewBus()
	return New(cards, ledger, bus)
}

func TestServer_IssueCardAndGetCard(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000}}
	srv := newTestServer(ledger, &fakeCardIssuance{})

	card, err := srv.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 5000,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	got, err := srv.GetCard(context.Background(), &gatewaypb.GetCardRequest{CardId: card.GetId()})
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.GetCardholderName() != "Ada Lovelace" {
		t.Errorf("CardholderName = %q, want Ada Lovelace", got.GetCardholderName())
	}

	list, err := srv.ListCards(context.Background(), &gatewaypb.ListCardsRequest{TenantId: "tenant_1"})
	if err != nil {
		t.Fatalf("ListCards() error = %v", err)
	}
	if len(list.GetCards()) != 1 {
		t.Errorf("len(cards) = %d, want 1", len(list.GetCards()))
	}
}

func TestServer_AdjustCardLimit(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"}}
	srv := newTestServer(ledger, &fakeCardIssuance{})

	card, err := srv.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	updated, err := srv.AdjustCardLimit(context.Background(), &gatewaypb.AdjustCardLimitRequest{
		CardId: card.GetId(), NewLimitMinorUnits: 2000, Reason: "raise",
	})
	if err != nil {
		t.Fatalf("AdjustCardLimit() error = %v", err)
	}
	if updated.GetLimitMinorUnits() != 2000 {
		t.Errorf("LimitMinorUnits = %d, want 2000", updated.GetLimitMinorUnits())
	}
}

func TestServer_RationBridgeFunding_PublishesActivity(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000}}
	cards := &fakeCardIssuance{bridgeGrantedMinorUnits: 4200, bridgeExplanation: "granted below full due to reserve floor"}
	bus := activity.NewBus()
	srv := New(cards, ledger, bus)

	ch, unsubscribe := bus.Subscribe("tenant_1")
	defer unsubscribe()

	resp, err := srv.RationBridgeFunding(context.Background(), &gatewaypb.RationBridgeFundingRequest{
		TenantId:            "tenant_1",
		RequestedMinorUnits: 10000,
		Reason:              "payout delayed",
	})
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if resp.GetGrantedMinorUnits() != 4200 {
		t.Errorf("GrantedMinorUnits = %d, want 4200", resp.GetGrantedMinorUnits())
	}

	select {
	case e := <-ch:
		if e.Kind != "agent_decision" {
			t.Errorf("Kind = %q, want agent_decision", e.Kind)
		}
		if e.Summary != "granted below full due to reserve floor" {
			t.Errorf("Summary = %q, want the bridge explanation", e.Summary)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for activity event")
	}
}

func TestServer_SimulateDeposit(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000}}
	srv := newTestServer(ledger, &fakeCardIssuance{})

	resp, err := srv.SimulateDeposit(context.Background(), &gatewaypb.SimulateDepositRequest{
		TenantId:         "tenant_1",
		AmountMinorUnits: 25000,
		MarkLate:         true,
	})
	if err != nil {
		t.Fatalf("SimulateDeposit() error = %v", err)
	}
	if resp.GetDepositId() == "" {
		t.Error("DepositId is empty")
	}
	if len(ledger.recordedDeposits) != 1 {
		t.Fatalf("len(recordedDeposits) = %d, want 1", len(ledger.recordedDeposits))
	}
	if ledger.recordedDeposits[0].GetExpectedAmount().GetMinorUnits() != 25000 {
		t.Errorf("expected amount = %d, want 25000", ledger.recordedDeposits[0].GetExpectedAmount().GetMinorUnits())
	}
	if ledger.recordedDeposits[0].GetExpectedAtUnix() >= time.Now().Unix() {
		t.Error("mark_late=true should backdate expected_at_unix into the past")
	}
}

func TestServer_SimulateDeposit_RejectsMissingTenant(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"}}
	srv := newTestServer(ledger, &fakeCardIssuance{})

	if _, err := srv.SimulateDeposit(context.Background(), &gatewaypb.SimulateDepositRequest{AmountMinorUnits: 100}); err == nil {
		t.Error("SimulateDeposit() with empty tenant_id expected error, got nil")
	}
}

func TestServer_SimulateDeposit_RejectsNonPositiveAmount(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"}}
	srv := newTestServer(ledger, &fakeCardIssuance{})

	if _, err := srv.SimulateDeposit(context.Background(), &gatewaypb.SimulateDepositRequest{TenantId: "tenant_1", AmountMinorUnits: 0}); err == nil {
		t.Error("SimulateDeposit() with amount=0 expected error, got nil")
	}
}

func TestServer_StreamActivity_FiltersAndStopsOnContextCancel(t *testing.T) {
	ledger := &fakeLedger{tenant: &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD"}}
	bus := activity.NewBus()
	srv := New(&fakeCardIssuance{}, ledger, bus)

	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeActivityStream{ctx: ctx, received: make(chan *gatewaypb.ActivityEvent, 10)}

	done := make(chan error, 1)
	go func() {
		done <- srv.StreamActivity(&gatewaypb.StreamActivityRequest{TenantId: "tenant_1"}, stream)
	}()

	// StreamActivity subscribes asynchronously in the goroutine above;
	// wait for the subscription to actually be registered before
	// publishing, so this test isn't racing the subscribe call.
	deadline := time.Now().Add(2 * time.Second)
	for bus.SubscriberCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for StreamActivity to subscribe")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := srv.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 100, Currency: "USD",
	}); err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	select {
	case e := <-stream.received:
		if e.Kind != "card" {
			t.Errorf("Kind = %q, want card", e.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streamed activity event")
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("StreamActivity() expected context-cancelled error, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("StreamActivity did not return after context cancellation")
	}
}

// fakeActivityStream implements gatewaypb.Gateway_StreamActivityServer
// (grpc.ServerStreamingServer[ActivityEvent], i.e. grpc.ServerStream plus
// a typed Send) for testing StreamActivity without a real gRPC
// connection. Only Context and Send are exercised by the implementation
// under test; the rest are no-ops required to satisfy the interface.
type fakeActivityStream struct {
	ctx      context.Context
	received chan *gatewaypb.ActivityEvent
}

func (f *fakeActivityStream) Send(e *gatewaypb.ActivityEvent) error {
	f.received <- e
	return nil
}
func (f *fakeActivityStream) Context() context.Context     { return f.ctx }
func (f *fakeActivityStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeActivityStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeActivityStream) SetTrailer(metadata.MD)       {}
func (f *fakeActivityStream) SendMsg(m any) error          { return nil }
func (f *fakeActivityStream) RecvMsg(m any) error          { return nil }
