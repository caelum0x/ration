package ration

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeGateway implements gatewaypb.GatewayServer with no network and no
// real services behind it, so this SDK's wire-level behavior (request
// shape in, response shape out) can be tested without standing up
// services/gateway and its four backing services.
type fakeGateway struct {
	gatewaypb.UnimplementedGatewayServer

	issueCardCalls []*gatewaypb.IssueCardRequest
	streamEvent    *gatewaypb.ActivityEvent
	streamErr      error
}

func (f *fakeGateway) IssueCard(_ context.Context, req *gatewaypb.IssueCardRequest) (*gatewaypb.Card, error) {
	f.issueCardCalls = append(f.issueCardCalls, req)
	return &gatewaypb.Card{
		Id:              "card_1",
		TenantId:        req.GetTenantId(),
		CardholderName:  req.GetCardholderName(),
		LimitMinorUnits: req.GetLimitMinorUnits(),
		LimitInterval:   req.GetLimitInterval(),
		Currency:        req.GetCurrency(),
	}, nil
}

func (f *fakeGateway) AdjustCardLimit(_ context.Context, req *gatewaypb.AdjustCardLimitRequest) (*gatewaypb.Card, error) {
	return &gatewaypb.Card{Id: req.GetCardId(), LimitMinorUnits: req.GetNewLimitMinorUnits()}, nil
}

func (f *fakeGateway) GetCard(_ context.Context, req *gatewaypb.GetCardRequest) (*gatewaypb.Card, error) {
	return &gatewaypb.Card{Id: req.GetCardId()}, nil
}

func (f *fakeGateway) ListCards(_ context.Context, req *gatewaypb.ListCardsRequest) (*gatewaypb.ListCardsResponse, error) {
	return &gatewaypb.ListCardsResponse{Cards: []*gatewaypb.Card{
		{Id: "card_1", TenantId: req.GetTenantId()},
		{Id: "card_2", TenantId: req.GetTenantId()},
	}}, nil
}

func (f *fakeGateway) RationBridgeFunding(_ context.Context, req *gatewaypb.RationBridgeFundingRequest) (*gatewaypb.RationBridgeFundingResponse, error) {
	return &gatewaypb.RationBridgeFundingResponse{
		GrantedMinorUnits: req.GetRequestedMinorUnits() / 2,
		FullyGranted:      false,
		Explanation:       "reserve floor headroom only covers half the request",
	}, nil
}

func (f *fakeGateway) SimulateDeposit(_ context.Context, req *gatewaypb.SimulateDepositRequest) (*gatewaypb.SimulateDepositResponse, error) {
	if req.GetTenantId() == "" {
		return nil, errors.New("tenant_id is required")
	}
	return &gatewaypb.SimulateDepositResponse{DepositId: "dep_1"}, nil
}

func (f *fakeGateway) StreamActivity(req *gatewaypb.StreamActivityRequest, stream gatewaypb.Gateway_StreamActivityServer) error {
	if f.streamErr != nil {
		return f.streamErr
	}
	if f.streamEvent != nil {
		if err := stream.Send(f.streamEvent); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func newTestClient(t *testing.T, fake *fakeGateway) *Client {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	gatewaypb.RegisterGatewayServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewFromConn(conn)
}

func TestClient_IssueCard(t *testing.T) {
	fake := &fakeGateway{}
	client := newTestClient(t, fake)
	ctx := context.Background()

	card, err := client.IssueCard(ctx, "tenant_1", "Jane Doe", 10000, "monthly", "USD")
	if err != nil {
		t.Fatalf("IssueCard: %v", err)
	}
	if card.GetId() != "card_1" || card.GetTenantId() != "tenant_1" || card.GetCardholderName() != "Jane Doe" {
		t.Fatalf("unexpected card: %+v", card)
	}
	if len(fake.issueCardCalls) != 1 || fake.issueCardCalls[0].GetLimitMinorUnits() != 10000 {
		t.Fatalf("expected 1 IssueCard call with limit 10000, got %+v", fake.issueCardCalls)
	}
}

func TestClient_AdjustCardLimit(t *testing.T) {
	client := newTestClient(t, &fakeGateway{})
	card, err := client.AdjustCardLimit(context.Background(), "card_9", 5000, "customer requested increase")
	if err != nil {
		t.Fatalf("AdjustCardLimit: %v", err)
	}
	if card.GetId() != "card_9" || card.GetLimitMinorUnits() != 5000 {
		t.Fatalf("unexpected card: %+v", card)
	}
}

func TestClient_GetCard(t *testing.T) {
	client := newTestClient(t, &fakeGateway{})
	card, err := client.GetCard(context.Background(), "card_42")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if card.GetId() != "card_42" {
		t.Fatalf("unexpected card id: %q", card.GetId())
	}
}

func TestClient_ListCards(t *testing.T) {
	client := newTestClient(t, &fakeGateway{})
	cards, err := client.ListCards(context.Background(), "tenant_7")
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(cards) != 2 || cards[0].GetTenantId() != "tenant_7" {
		t.Fatalf("unexpected cards: %+v", cards)
	}
}

func TestClient_RequestBridgeFunding(t *testing.T) {
	client := newTestClient(t, &fakeGateway{})
	result, err := client.RequestBridgeFunding(context.Background(), "tenant_1", 1000, "card settlement due")
	if err != nil {
		t.Fatalf("RequestBridgeFunding: %v", err)
	}
	if result.GrantedMinorUnits != 500 || result.FullyGranted {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Explanation == "" {
		t.Fatal("expected a non-empty explanation")
	}
}

func TestClient_SimulateDeposit(t *testing.T) {
	client := newTestClient(t, &fakeGateway{})
	ctx := context.Background()

	id, err := client.SimulateDeposit(ctx, "tenant_1", 25000, 0, false)
	if err != nil {
		t.Fatalf("SimulateDeposit: %v", err)
	}
	if id != "dep_1" {
		t.Fatalf("unexpected deposit id: %q", id)
	}

	if _, err := client.SimulateDeposit(ctx, "", 25000, 0, false); err == nil {
		t.Fatal("expected an error for an empty tenant_id")
	}
}

func TestClient_StreamActivity(t *testing.T) {
	fake := &fakeGateway{streamEvent: &gatewaypb.ActivityEvent{Id: "evt_1", TenantId: "tenant_1", Kind: "card", Summary: "issued card_1"}}
	client := newTestClient(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errs, err := client.StreamActivity(ctx, "tenant_1")
	if err != nil {
		t.Fatalf("StreamActivity: %v", err)
	}

	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events channel closed before delivering an event")
		}
		if ev.GetId() != "evt_1" || ev.GetSummary() != "issued card_1" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case err := <-errs:
		t.Fatalf("unexpected stream error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an activity event")
	}

	cancel()
	// Draining both channels until events closes proves the goroutine
	// exits on ctx cancellation instead of leaking.
	for range events {
	}
}
