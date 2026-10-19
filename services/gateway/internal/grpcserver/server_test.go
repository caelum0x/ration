package grpcserver

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/airwallex"
	"github.com/caelum0x/ration/services/gateway/internal/bridge"
	"github.com/caelum0x/ration/services/gateway/internal/card"
	"google.golang.org/grpc/metadata"
)

// fakeLedger implements every narrow LedgerClient interface used across
// card, bridge and grpcserver so a single fake can back an end-to-end
// Server in tests, with no gRPC and no network.
type fakeLedger struct {
	tenant   *ledgerpb.Tenant
	balance  *ledgerpb.Balance
	deposits []*ledgerpb.Deposit
	entries  []*ledgerpb.LedgerEntry

	recordedDeposits []*ledgerpb.RecordDepositRequest
	appliedEntries   []*ledgerpb.ApplyEntryRequest
	nextDepositID    int
}

func (f *fakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	return f.tenant, nil
}
func (f *fakeLedger) GetBalance(ctx context.Context, tenantID string) (*ledgerpb.Balance, error) {
	return f.balance, nil
}
func (f *fakeLedger) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	return f.deposits, nil
}
func (f *fakeLedger) ListEntries(ctx context.Context, tenantID string) ([]*ledgerpb.LedgerEntry, error) {
	return f.entries, nil
}
func (f *fakeLedger) CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledgerpb.CheckReserveFloorResponse, error) {
	headroom := f.balance.GetAvailableMinorUnits() - f.tenant.GetReserveFloorMinorUnits()
	return &ledgerpb.CheckReserveFloorResponse{Allowed: proposedDebitMinorUnits <= headroom, HeadroomMinorUnits: headroom}, nil
}
func (f *fakeLedger) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	f.appliedEntries = append(f.appliedEntries, req)
	return &ledgerpb.ApplyEntryResponse{Entry: &ledgerpb.LedgerEntry{Id: "entry_test"}}, nil
}
func (f *fakeLedger) RecordAgentDecision(ctx context.Context, req *ledgerpb.RecordAgentDecisionRequest) (*ledgerpb.AgentDecision, error) {
	return &ledgerpb.AgentDecision{Id: "decision_test"}, nil
}
func (f *fakeLedger) RecordDeposit(ctx context.Context, req *ledgerpb.RecordDepositRequest) (*ledgerpb.Deposit, error) {
	f.recordedDeposits = append(f.recordedDeposits, req)
	f.nextDepositID++
	return &ledgerpb.Deposit{
		Id:             "dep_test",
		TenantId:       req.GetTenantId(),
		ExpectedAmount: req.GetExpectedAmount(),
		ExpectedAtUnix: req.GetExpectedAtUnix(),
		ExternalRef:    req.GetExternalRef(),
	}, nil
}

func newTestServer(f *fakeLedger) *Server {
	aw := airwallex.NewMock("secret")
	cardSvc := card.New(aw, f)
	bridgeSvc := bridge.New(f, bridge.DefaultPolicy())
	bus := activity.NewBus()
	return New(cardSvc, bridgeSvc, f, bus)
}

func TestServer_IssueCardAndGetCard(t *testing.T) {
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: "tenant_1", AvailableMinorUnits: 100000},
	}
	srv := newTestServer(f)

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

func TestServer_RationBridgeFunding_PublishesActivity(t *testing.T) {
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: "tenant_1", AvailableMinorUnits: 5000},
		deposits: []*ledgerpb.Deposit{
			{
				Id:             "dep_1",
				TenantId:       "tenant_1",
				ExpectedAmount: &commonpb.Money{MinorUnits: 100000, Currency: "USD"},
				ExpectedAtUnix: time.Now().Add(-time.Hour).Unix(),
				Status:         commonpb.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
	}
	aw := airwallex.NewMock("secret")
	cardSvc := card.New(aw, f)
	bridgeSvc := bridge.New(f, bridge.DefaultPolicy())
	bus := activity.NewBus()
	srv := New(cardSvc, bridgeSvc, f, bus)

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
	if resp.GetGrantedMinorUnits() <= 0 {
		t.Errorf("GrantedMinorUnits = %d, want > 0", resp.GetGrantedMinorUnits())
	}

	select {
	case e := <-ch:
		if e.Kind != "agent_decision" {
			t.Errorf("Kind = %q, want agent_decision", e.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for activity event")
	}
}

func TestServer_SimulateDeposit(t *testing.T) {
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: "tenant_1", AvailableMinorUnits: 100000},
	}
	srv := newTestServer(f)

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
	if len(f.recordedDeposits) != 1 {
		t.Fatalf("len(recordedDeposits) = %d, want 1", len(f.recordedDeposits))
	}
	if f.recordedDeposits[0].GetExpectedAmount().GetMinorUnits() != 25000 {
		t.Errorf("expected amount = %d, want 25000", f.recordedDeposits[0].GetExpectedAmount().GetMinorUnits())
	}
	if f.recordedDeposits[0].GetExpectedAtUnix() >= time.Now().Unix() {
		t.Error("mark_late=true should backdate expected_at_unix into the past")
	}
}

func TestServer_StreamActivity_FiltersAndStopsOnContextCancel(t *testing.T) {
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: "tenant_1", Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: "tenant_1", AvailableMinorUnits: 100000},
	}
	aw := airwallex.NewMock("secret")
	cardSvc := card.New(aw, f)
	bridgeSvc := bridge.New(f, bridge.DefaultPolicy())
	bus := activity.NewBus()
	srv := New(cardSvc, bridgeSvc, f, bus)

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
