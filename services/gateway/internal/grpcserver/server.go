// Package grpcserver implements gateway.proto's Gateway service,
// wiring together internal/card, internal/bridge, internal/activity and
// the ledger client. It is the one place that translates between wire
// types (pkg/pb/gateway) and the internal service APIs.
package grpcserver

import (
	"context"
	"fmt"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/bridge"
	"github.com/caelum0x/ration/services/gateway/internal/card"
	"github.com/caelum0x/ration/services/gateway/internal/idgen"
)

// LedgerClient is the subset of ledgerclient.Client SimulateDeposit
// needs (card/bridge take their own narrower interfaces).
type LedgerClient interface {
	RecordDeposit(ctx context.Context, req *ledgerpb.RecordDepositRequest) (*ledgerpb.Deposit, error)
	GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error)
}

// Server implements gatewaypb.GatewayServer.
type Server struct {
	gatewaypb.UnimplementedGatewayServer

	cards  *card.Service
	bridge *bridge.Service
	ledger LedgerClient
	bus    *activity.Bus

	newID func(prefix string) string
}

// New constructs a Server wiring the given services and event bus.
func New(cards *card.Service, bridgeSvc *bridge.Service, ledger LedgerClient, bus *activity.Bus) *Server {
	return &Server{
		cards:  cards,
		bridge: bridgeSvc,
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

// IssueCard implements gatewaypb.GatewayServer.
func (s *Server) IssueCard(ctx context.Context, req *gatewaypb.IssueCardRequest) (*gatewaypb.Card, error) {
	c, err := s.cards.IssueCard(ctx, req)
	if err != nil {
		return nil, err
	}
	s.publish(req.GetTenantId(), "card", fmt.Sprintf("issued card %s for %s, limit %d %s", c.GetId(), req.GetCardholderName(), c.GetLimitMinorUnits(), c.GetCurrency()))
	return c, nil
}

// AdjustCardLimit implements gatewaypb.GatewayServer.
func (s *Server) AdjustCardLimit(ctx context.Context, req *gatewaypb.AdjustCardLimitRequest) (*gatewaypb.Card, error) {
	c, err := s.cards.AdjustCardLimit(ctx, req)
	if err != nil {
		return nil, err
	}
	s.publish(c.GetTenantId(), "card", fmt.Sprintf("adjusted card %s limit to %d %s (%s)", c.GetId(), c.GetLimitMinorUnits(), c.GetCurrency(), req.GetReason()))
	return c, nil
}

// GetCard implements gatewaypb.GatewayServer.
func (s *Server) GetCard(ctx context.Context, req *gatewaypb.GetCardRequest) (*gatewaypb.Card, error) {
	return s.cards.GetCard(ctx, req.GetCardId())
}

// ListCards implements gatewaypb.GatewayServer.
func (s *Server) ListCards(ctx context.Context, req *gatewaypb.ListCardsRequest) (*gatewaypb.ListCardsResponse, error) {
	cards, err := s.cards.ListCards(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	return &gatewaypb.ListCardsResponse{Cards: cards}, nil
}

// RationBridgeFunding implements gatewaypb.GatewayServer.
func (s *Server) RationBridgeFunding(ctx context.Context, req *gatewaypb.RationBridgeFundingRequest) (*gatewaypb.RationBridgeFundingResponse, error) {
	result, err := s.bridge.RationBridgeFunding(ctx, req.GetTenantId(), req.GetRequestedMinorUnits(), req.GetReason())
	if err != nil {
		return nil, err
	}
	s.publish(req.GetTenantId(), "agent_decision", result.Explanation)
	return &gatewaypb.RationBridgeFundingResponse{
		GrantedMinorUnits: result.GrantedMinorUnits,
		FullyGranted:      result.FullyGranted,
		Explanation:       result.Explanation,
	}, nil
}

// SimulateDeposit implements gatewaypb.GatewayServer. It is a demo/judging
// hook (see services/cli's `ration simulate-deposit`): it records an
// expected deposit with ledger-core, optionally backdated so
// ListLateDeposits immediately reports it as late.
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
