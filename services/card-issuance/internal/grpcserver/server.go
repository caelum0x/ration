// Package grpcserver implements card_issuance.proto's CardIssuance
// service by delegating directly to internal/cardsvc and internal/bridge
// — this package is the one place that would translate between wire
// types and a different internal representation, but since cardsvc and
// bridge already speak cardissuance.* wire types natively (unlike the
// old gateway, which had its own gateway.* types), every method here is
// a direct passthrough.
package grpcserver

import (
	"context"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"github.com/caelum0x/ration/services/card-issuance/internal/bridge"
	"github.com/caelum0x/ration/services/card-issuance/internal/cardsvc"
)

// Server implements cardissuancepb.CardIssuanceServer.
type Server struct {
	cardissuancepb.UnimplementedCardIssuanceServer

	cards  *cardsvc.Service
	bridge *bridge.Service
}

// New constructs a Server wiring the given services.
func New(cards *cardsvc.Service, bridgeSvc *bridge.Service) *Server {
	return &Server{cards: cards, bridge: bridgeSvc}
}

// IssueCard implements cardissuancepb.CardIssuanceServer.
func (s *Server) IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error) {
	return s.cards.IssueCard(ctx, req)
}

// AdjustCardLimit implements cardissuancepb.CardIssuanceServer.
func (s *Server) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	return s.cards.AdjustCardLimit(ctx, req)
}

// GetCard implements cardissuancepb.CardIssuanceServer.
func (s *Server) GetCard(ctx context.Context, req *cardissuancepb.GetCardRequest) (*cardissuancepb.Card, error) {
	return s.cards.GetCard(ctx, req.GetCardId())
}

// ListCards implements cardissuancepb.CardIssuanceServer.
func (s *Server) ListCards(ctx context.Context, req *cardissuancepb.ListCardsRequest) (*cardissuancepb.ListCardsResponse, error) {
	cards, err := s.cards.ListCards(ctx, req.GetTenantId())
	if err != nil {
		return nil, err
	}
	return &cardissuancepb.ListCardsResponse{Cards: cards}, nil
}

// RationBridgeFunding implements cardissuancepb.CardIssuanceServer.
func (s *Server) RationBridgeFunding(ctx context.Context, req *cardissuancepb.RationBridgeFundingRequest) (*cardissuancepb.RationBridgeFundingResponse, error) {
	result, err := s.bridge.RationBridgeFunding(ctx, req.GetTenantId(), req.GetRequestedMinorUnits(), req.GetReason())
	if err != nil {
		return nil, err
	}
	return &cardissuancepb.RationBridgeFundingResponse{
		GrantedMinorUnits: result.GrantedMinorUnits,
		FullyGranted:      result.FullyGranted,
		Explanation:       result.Explanation,
	}, nil
}
