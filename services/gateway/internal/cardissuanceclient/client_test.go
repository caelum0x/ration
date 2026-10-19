package cardissuanceclient

import (
	"context"
	"fmt"
	"net"
	"testing"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeCardIssuanceServer is a minimal real gRPC server implementing
// enough of CardIssuanceServer to exercise Client's RPC wrapping (request/
// response mapping) over an actual gRPC connection (via bufconn, no real
// network socket), rather than mocking Client itself.
type fakeCardIssuanceServer struct {
	cardissuancepb.UnimplementedCardIssuanceServer
	cards      map[string]*cardissuancepb.Card
	nextCardID int
}

func (f *fakeCardIssuanceServer) IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error) {
	if f.cards == nil {
		f.cards = map[string]*cardissuancepb.Card{}
	}
	f.nextCardID++
	c := &cardissuancepb.Card{
		Id:              fmt.Sprintf("card_%d", f.nextCardID),
		TenantId:        req.GetTenantId(),
		CardholderName:  req.GetCardholderName(),
		LimitMinorUnits: req.GetLimitMinorUnits(),
		Currency:        req.GetCurrency(),
	}
	f.cards[c.Id] = c
	return c, nil
}

func (f *fakeCardIssuanceServer) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	c, ok := f.cards[req.GetCardId()]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	c.LimitMinorUnits = req.GetNewLimitMinorUnits()
	return c, nil
}

func (f *fakeCardIssuanceServer) GetCard(ctx context.Context, req *cardissuancepb.GetCardRequest) (*cardissuancepb.Card, error) {
	return f.cards[req.GetCardId()], nil
}

func (f *fakeCardIssuanceServer) ListCards(ctx context.Context, req *cardissuancepb.ListCardsRequest) (*cardissuancepb.ListCardsResponse, error) {
	var out []*cardissuancepb.Card
	for _, c := range f.cards {
		if c.GetTenantId() == req.GetTenantId() {
			out = append(out, c)
		}
	}
	return &cardissuancepb.ListCardsResponse{Cards: out}, nil
}

func (f *fakeCardIssuanceServer) RationBridgeFunding(ctx context.Context, req *cardissuancepb.RationBridgeFundingRequest) (*cardissuancepb.RationBridgeFundingResponse, error) {
	return &cardissuancepb.RationBridgeFundingResponse{
		GrantedMinorUnits: req.GetRequestedMinorUnits(),
		FullyGranted:      true,
		Explanation:       "granted in full (fake)",
	}, nil
}

func newTestClient(t *testing.T, server *fakeCardIssuanceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	cardissuancepb.RegisterCardIssuanceServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return NewFromConn(conn)
}

func TestClient_IssueCardAndGetCard(t *testing.T) {
	client := newTestClient(t, &fakeCardIssuanceServer{})

	card, err := client.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 5000,
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}
	if card.GetId() == "" {
		t.Fatal("IssueCard() returned empty card id")
	}

	got, err := client.GetCard(context.Background(), card.GetId())
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.GetCardholderName() != "Ada Lovelace" {
		t.Errorf("CardholderName = %q, want Ada Lovelace", got.GetCardholderName())
	}
}

func TestClient_AdjustCardLimit(t *testing.T) {
	server := &fakeCardIssuanceServer{}
	client := newTestClient(t, server)

	card, err := client.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
		TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}

	updated, err := client.AdjustCardLimit(context.Background(), &cardissuancepb.AdjustCardLimitRequest{
		CardId: card.GetId(), NewLimitMinorUnits: 2000, Reason: "test",
	})
	if err != nil {
		t.Fatalf("AdjustCardLimit() error = %v", err)
	}
	if updated.GetLimitMinorUnits() != 2000 {
		t.Errorf("LimitMinorUnits = %d, want 2000", updated.GetLimitMinorUnits())
	}
}

func TestClient_ListCards(t *testing.T) {
	server := &fakeCardIssuanceServer{}
	client := newTestClient(t, server)

	for i := 0; i < 2; i++ {
		if _, err := client.IssueCard(context.Background(), &cardissuancepb.IssueCardRequest{
			TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD",
		}); err != nil {
			t.Fatalf("IssueCard() error = %v", err)
		}
	}

	cards, err := client.ListCards(context.Background(), "tenant_1")
	if err != nil {
		t.Fatalf("ListCards() error = %v", err)
	}
	if len(cards) != 2 {
		t.Errorf("len(cards) = %d, want 2", len(cards))
	}
}

func TestClient_RationBridgeFunding(t *testing.T) {
	client := newTestClient(t, &fakeCardIssuanceServer{})

	resp, err := client.RationBridgeFunding(context.Background(), &cardissuancepb.RationBridgeFundingRequest{
		TenantId: "tenant_1", RequestedMinorUnits: 5000, Reason: "payout delayed",
	})
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if resp.GetGrantedMinorUnits() != 5000 || !resp.GetFullyGranted() {
		t.Errorf("resp = %+v, want granted=5000 fully_granted=true", resp)
	}
}

func TestDial_RejectsEmptyAddr(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Fatal("Dial(\"\") expected error, got nil")
	}
}

func TestClient_Close_NilConnIsNoop(t *testing.T) {
	c := &Client{}
	if err := c.Close(); err != nil {
		t.Errorf("Close() on zero-value Client error = %v, want nil", err)
	}
}
