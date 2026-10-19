// Package grpcserver implements gateway.proto's Gateway service as a
// thin BFF (docs/SCALE-EXPANSION.md section 1, docs/adr/0004): every
// card and rationing-math RPC is a direct fan-out to services/card-issuance
// over internal/cardissuanceclient, translated at this one edge between
// cardissuance.* wire types and gateway.* wire types (which are
// structurally identical by design — gateway.proto's Card/IssueCardRequest/
// etc. messages are unchanged from before the split, so this translation
// is a mechanical field-by-field copy, not new business logic). Deposit
// simulation still talks to ledger-core directly (it always proxied
// straight through here, with no gateway-side business logic beyond
// defaulting expected_at_unix). StreamActivity serves this process's own
// activity bus, which cmd/gateway/main.go also feeds from
// services/webhook-ingestion's StreamEvents via internal/webhookeventsclient.
package grpcserver

import (
	"context"
	"fmt"
	"time"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/idgen"
)

// CardIssuanceClient is the subset of cardissuanceclient.Client the
// gateway needs — a narrow interface so tests can supply a fake without
// standing up a real gRPC server.
type CardIssuanceClient interface {
	IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error)
	AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error)
	GetCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error)
	ListCards(ctx context.Context, tenantID string) ([]*cardissuancepb.Card, error)
	RationBridgeFunding(ctx context.Context, req *cardissuancepb.RationBridgeFundingRequest) (*cardissuancepb.RationBridgeFundingResponse, error)
}

// LedgerClient is the subset of ledgerclient.Client SimulateDeposit
// needs.
type LedgerClient interface {
	RecordDeposit(ctx context.Context, req *ledgerpb.RecordDepositRequest) (*ledgerpb.Deposit, error)
	GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error)
}

// Server implements gatewaypb.GatewayServer.
type Server struct {
	gatewaypb.UnimplementedGatewayServer

	cards  CardIssuanceClient
	ledger LedgerClient
	bus    *activity.Bus

	newID func(prefix string) string
}

// New constructs a Server wiring the given card-issuance client, ledger
// client and activity bus.
func New(cards CardIssuanceClient, ledger LedgerClient, bus *activity.Bus) *Server {
	return &Server{
		cards:  cards,
		ledger: ledger,
		bus:    bus,
		newID:  idgen.New,
	}
}

func (s *Server) publish(tenantID, kind, summary string) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(activity.NewEvent(tenantID, kind, summary))
}

// toGatewayCard copies a cardissuance.Card into gateway.proto's identical
// Card message — the two protos define the same fields by design (see
// this package's doc comment), so this is a straight field copy, not a
// translation of meaning.
func toGatewayCard(c *cardissuancepb.Card) *gatewaypb.Card {
	if c == nil {
		return nil
	}
	return &gatewaypb.Card{
		Id:              c.GetId(),
		TenantId:        c.GetTenantId(),
		AirwallexCardId: c.GetAirwallexCardId(),
		CardholderName:  c.GetCardholderName(),
		Status:          c.GetStatus(),
		LimitMinorUnits: c.GetLimitMinorUnits(),
		LimitInterval:   c.GetLimitInterval(),
		Currency:        c.GetCurrency(),
		CreatedAtUnix:   c.GetCreatedAtUnix(),
	}
}

// IssueCard implements gatewaypb.GatewayServer.
func (s *Server) IssueCard(ctx context.Context, req *gatewaypb.IssueCardRequest) (*gatewaypb.Card, error) {
	c, err := s.cards.IssueCard(ctx, &cardissuancepb.IssueCardRequest{
		TenantId:        req.GetTenantId(),
		CardholderName:  req.GetCardholderName(),
		LimitMinorUnits: req.GetLimitMinorUnits(),
		LimitInterval:   req.GetLimitInterval(),
		Currency:        req.GetCurrency(),
	})
	if err != nil {
		return nil, err
	}
	s.publish(req.GetTenantId(), "card", fmt.Sprintf("issued card %s for %s, limit %d %s", c.GetId(), req.GetCardholderName(), c.GetLimitMinorUnits(), c.GetCurrency()))
	return toGatewayCard(c), nil
}

// AdjustCardLimit implements gatewaypb.GatewayServer.
func (s *Server) AdjustCardLimit(ctx context.Context, req *gatewaypb.AdjustCardLimitRequest) (*gatewaypb.Card, error) {
	c, err := s.cards.AdjustCardLimit(ctx, &cardissuancepb.AdjustCardLimitRequest{
		CardId:             req.GetCardId(),
		NewLimitMinorUnits: req.GetNewLimitMinorUnits(),
		Reason:             req.GetReason(),
	})
	if err != nil {
		return nil, err
	}
	s.publish(c.GetTenantId(), "card", fmt.Sprintf("adjusted card %s limit to %d %s (%s)", c.GetId(), c.GetLimitMinorUnits(), c.GetCurrency(), req.GetReason()))
	return toGatewayCard(c), nil
}

// GetCard implements gatewaypb.GatewayServer.
func (s *Server) GetCard(ctx context.Context, req *gatewaypb.GetCardRequest) (*gatewaypb.Card, error) {
	c, err := s.cards.GetCard(ctx, req.GetCardId())
	if err != nil {
		return nil, err
	}
	return toGatewayCard(c), nil
}

// ListCards implements gatewaypb.GatewayServer.
func (s *Server) ListCards(ctx context.Context, req *gatewaypb.ListCardsRequest) (*gatewaypb.ListCardsResponse, error) {
	cards, err := s.cards.ListCards(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	out := make([]*gatewaypb.Card, 0, len(cards))
	for _, c := range cards {
		out = append(out, toGatewayCard(c))
	}
	return &gatewaypb.ListCardsResponse{Cards: out}, nil
}

// RationBridgeFunding implements gatewaypb.GatewayServer.
func (s *Server) RationBridgeFunding(ctx context.Context, req *gatewaypb.RationBridgeFundingRequest) (*gatewaypb.RationBridgeFundingResponse, error) {
	result, err := s.cards.RationBridgeFunding(ctx, &cardissuancepb.RationBridgeFundingRequest{
		TenantId:            req.GetTenantId(),
		RequestedMinorUnits: req.GetRequestedMinorUnits(),
		Reason:              req.GetReason(),
	})
	if err != nil {
		return nil, err
	}
	s.publish(req.GetTenantId(), "agent_decision", result.GetExplanation())
	return &gatewaypb.RationBridgeFundingResponse{
		GrantedMinorUnits: result.GetGrantedMinorUnits(),
		FullyGranted:      result.GetFullyGranted(),
		Explanation:       result.GetExplanation(),
	}, nil
}

// SimulateDeposit implements gatewaypb.GatewayServer. It is a demo/judging
// hook (see services/cli's `ration simulate-deposit`): it records an
// expected deposit with ledger-core, optionally backdated so
// ListLateDeposits immediately reports it as late. This is the one RPC
// gateway.proto exposes that the section 1 split didn't move anywhere
// else — it always talked to ledger-core directly, no card-issuance or
// webhook-ingestion involvement, and still does.
func (s *Server) SimulateDeposit(ctx context.Context, req *gatewaypb.SimulateDepositRequest) (*gatewaypb.SimulateDepositResponse, error) {
	if req.GetTenantId() == "" {
		return nil, fmt.Errorf("grpcserver: tenant_id is required")
	}
	if req.GetAmountMinorUnits() <= 0 {
		return nil, fmt.Errorf("grpcserver: amount_minor_units must be > 0")
	}

	expectedAt := req.GetExpectedAtUnix()
	if req.GetMarkLate() {
		// Force the deposit to already be overdue: one hour in the past
		// relative to "now", regardless of what expected_at_unix the
		// caller passed, so ListLateDeposits picks it up immediately.
		expectedAt = time.Now().Add(-1 * time.Hour).Unix()
	} else if expectedAt == 0 {
		expectedAt = time.Now().Add(24 * time.Hour).Unix()
	}

	tenant, err := s.ledger.GetTenant(ctx, req.GetTenantId())
	if err != nil {
		return nil, fmt.Errorf("grpcserver: get tenant %s: %w", req.GetTenantId(), err)
	}

	deposit, err := s.ledger.RecordDeposit(ctx, &ledgerpb.RecordDepositRequest{
		TenantId: req.GetTenantId(),
		ExpectedAmount: &commonpb.Money{
			MinorUnits: req.GetAmountMinorUnits(),
			Currency:   tenant.GetCurrency(),
		},
		ExpectedAtUnix: expectedAt,
		ExternalRef:    s.newID("sim-deposit"),
	})
	if err != nil {
		return nil, fmt.Errorf("grpcserver: record simulated deposit for %s: %w", req.GetTenantId(), err)
	}

	s.publish(req.GetTenantId(), "deposit", fmt.Sprintf("simulated deposit %s of %d minor units, expected_at=%d, late=%v", deposit.GetId(), req.GetAmountMinorUnits(), expectedAt, req.GetMarkLate()))

	return &gatewaypb.SimulateDepositResponse{DepositId: deposit.GetId()}, nil
}

// StreamActivity implements gatewaypb.GatewayServer.
func (s *Server) StreamActivity(req *gatewaypb.StreamActivityRequest, stream gatewaypb.Gateway_StreamActivityServer) error {
	ch, unsubscribe := s.bus.Subscribe(req.GetTenantId())
	defer unsubscribe()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&gatewaypb.ActivityEvent{
				Id:       e.ID,
				TenantId: e.TenantID,
				Kind:     e.Kind,
				Summary:  e.Summary,
				AtUnix:   e.AtUnix,
			}); err != nil {
				return err
			}
		}
	}
}
