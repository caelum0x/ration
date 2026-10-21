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
	"errors"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
	"github.com/caelum0x/ration/services/card-issuance/internal/bridge"
	"github.com/caelum0x/ration/services/card-issuance/internal/cardsvc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// wrapCardLookupError translates cardsvc's plain-wrapped
// airwallex.ErrNotFound (a card ID that doesn't exist) into a proper
// gRPC status.Error(codes.NotFound, ...) before it leaves this service.
// Without this, the gRPC runtime's default error conversion downgrades
// any non-status error to codes.Unknown, which every GatewayClient
// caller (TUI, CLI, sdk/go/ration, sdk/rust/ration-client, and the
// gateway's REST facade in services/gateway/internal/httpapi/cards.go)
// then has no way to distinguish from a genuine server failure — see
// that file's handleGetCard doc comment for the exact symptom this
// closes (a missing card surfacing as HTTP 500 instead of 404).
func wrapCardLookupError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, airwallex.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return err
}

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
	card, err := s.cards.AdjustCardLimit(ctx, req)
	if err != nil {
		return nil, wrapCardLookupError(err)
	}
	return card, nil
}

// GetCard implements cardissuancepb.CardIssuanceServer.
func (s *Server) GetCard(ctx context.Context, req *cardissuancepb.GetCardRequest) (*cardissuancepb.Card, error) {
	card, err := s.cards.GetCard(ctx, req.GetCardId())
	if err != nil {
		return nil, wrapCardLookupError(err)
	}
	return card, nil
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
