// Package cardsvc implements card issuance and limit adjustment:
// Airwallex owns the actual card resource, ledger-core enforces the
// reserve floor for any funds the card limit holds against, and this
// package is the only place that persists card *metadata* (ledger-core's
// proto has no Card message — cards are a card-issuance-owned concept
// layered on top of ledger-core's tenant/balance/entry primitives).
//
// This is a direct extraction of the gateway's former internal/card
// package (docs/adr/0004, docs/SCALE-EXPANSION.md section 1) — the logic
// is unchanged, only the wire type (cardissuance.Card /
// cardissuance.IssueCardRequest, in place of gateway's own) and the
// package name (cardsvc, since "card" collides with nothing here but
// matches this service's cmd naming) differ.
package cardsvc

import (
	"context"
	"fmt"
	"sync"
	"time"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/metrics"
	"github.com/caelum0x/ration/services/card-issuance/internal/airwallex"
	"github.com/caelum0x/ration/services/card-issuance/internal/idgen"
	"google.golang.org/protobuf/proto"
)

// cardReferencePrefix tags every ledger HOLD/RELEASE entry backing a
// card's spend limit, so adjustments can reference the same hold.
const cardReferencePrefix = "card:"

// LedgerClient is the subset of ledgerclient.Client this package needs.
type LedgerClient interface {
	GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error)
	CheckReserveFloor(ctx context.Context, tenantID string, proposedDebitMinorUnits int64) (*ledgerpb.CheckReserveFloorResponse, error)
	ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error)
}

// Service issues and manages cards.
type Service struct {
	airwallex airwallex.Client
	ledger    LedgerClient

	mu    sync.RWMutex
	cards map[string]*cardissuancepb.Card

	now   func() time.Time
	newID func(prefix string) string

	// Metrics is optional (nil-safe); see bridge.Service.Metrics for why
	// this is a public field set post-construction rather than a New()
	// parameter.
	Metrics *metrics.Metrics
}

// New constructs a card Service.
func New(aw airwallex.Client, ledger LedgerClient) *Service {
	return &Service{
		airwallex: aw,
		ledger:    ledger,
		cards:     make(map[string]*cardissuancepb.Card),
		now:       time.Now,
		newID:     idgen.New,
	}
}

// IssueCard creates a card via Airwallex, verifies the tenant has enough
// reserve-floor headroom to back the requested limit, places a HOLD for
// that amount, and persists the resulting card metadata. If the reserve
// floor check fails, the just-created Airwallex card is suspended (limit
// set to zero) and an error is returned — no card is persisted, no hold
// is placed.
func (s *Service) IssueCard(ctx context.Context, req *cardissuancepb.IssueCardRequest) (*cardissuancepb.Card, error) {
	if req.GetTenantId() == "" {
		return nil, fmt.Errorf("cardsvc: tenant_id is required")
	}
	if req.GetCardholderName() == "" {
		return nil, fmt.Errorf("cardsvc: cardholder_name is required")
	}
	if req.GetLimitMinorUnits() < 0 {
		return nil, fmt.Errorf("cardsvc: limit_minor_units must be >= 0, got %d", req.GetLimitMinorUnits())
	}

	tenant, err := s.ledger.GetTenant(ctx, req.GetTenantId())
	if err != nil {
		return nil, fmt.Errorf("cardsvc: get tenant %s: %w", req.GetTenantId(), err)
	}

	currency := req.GetCurrency()
	if currency == "" {
		currency = tenant.GetCurrency()
	}

	awCard, err := s.airwallex.IssueCard(ctx, airwallex.IssueCardParams{
		TenantID:        req.GetTenantId(),
		CardholderName:  req.GetCardholderName(),
		LimitMinorUnits: req.GetLimitMinorUnits(),
		LimitInterval:   req.GetLimitInterval(),
		Currency:        currency,
	})
	if err != nil {
		return nil, fmt.Errorf("cardsvc: issue via airwallex: %w", err)
	}

	if req.GetLimitMinorUnits() > 0 {
		check, err := s.ledger.CheckReserveFloor(ctx, req.GetTenantId(), req.GetLimitMinorUnits())
		if err != nil {
			s.suspendAfterFailure(ctx, awCard.ID)
			return nil, fmt.Errorf("cardsvc: check reserve floor for %s: %w", req.GetTenantId(), err)
		}
		s.recordHeadroom(req.GetTenantId(), check.GetHeadroomMinorUnits())
		if !check.GetAllowed() {
			s.suspendAfterFailure(ctx, awCard.ID)
			return nil, fmt.Errorf(
				"cardsvc: insufficient reserve-floor headroom for tenant %s: requested limit %d minor units, headroom %d minor units",
				req.GetTenantId(), req.GetLimitMinorUnits(), check.GetHeadroomMinorUnits(),
			)
		}

		holdID := s.newID("card-hold")
		if _, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId: req.GetTenantId(),
			Type:     commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
			Amount: &commonpb.Money{
				MinorUnits: req.GetLimitMinorUnits(),
				Currency:   currency,
			},
			Reference:      cardReferencePrefix + awCard.ID,
			IdempotencyKey: holdID,
			Memo:           fmt.Sprintf("card limit hold for cardholder %s", req.GetCardholderName()),
		}); err != nil {
			s.suspendAfterFailure(ctx, awCard.ID)
			return nil, fmt.Errorf("cardsvc: apply limit hold for %s: %w", req.GetTenantId(), err)
		}
	}

	card := &cardissuancepb.Card{
		Id:              s.newID("card"),
		TenantId:        req.GetTenantId(),
		AirwallexCardId: awCard.ID,
		CardholderName:  req.GetCardholderName(),
		Status:          commonpb.CardStatus_CARD_STATUS_ACTIVE,
		LimitMinorUnits: req.GetLimitMinorUnits(),
		LimitInterval:   req.GetLimitInterval(),
		Currency:        currency,
		CreatedAtUnix:   s.now().Unix(),
	}

	s.mu.Lock()
	s.cards[card.Id] = card
	s.mu.Unlock()

	return cloneCard(card), nil
}

// suspendAfterFailure best-effort suspends an Airwallex card that was
// just created but cannot be safely backed by a ledger hold. Errors here
// are deliberately not surfaced to the caller: the primary error (the
// reserve-floor failure) is what matters, and we must not mask it with a
// cleanup failure, but we still want to try.
func (s *Service) suspendAfterFailure(ctx context.Context, awCardID string) {
	_, _ = s.airwallex.SetCardControls(ctx, awCardID, airwallex.CardControls{LimitMinorUnits: 0})
}

// AdjustCardLimit changes a card's spend limit, adjusting the backing
// ledger hold by the delta (an increase re-checks reserve-floor headroom
// and places an additional HOLD; a decrease places a RELEASE for the
// freed amount) before pushing the new limit to Airwallex.
func (s *Service) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	if req.GetNewLimitMinorUnits() < 0 {
		return nil, fmt.Errorf("cardsvc: new_limit_minor_units must be >= 0, got %d", req.GetNewLimitMinorUnits())
	}

	s.mu.Lock()
	card, ok := s.cards[req.GetCardId()]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("cardsvc: %s: %w", req.GetCardId(), airwallex.ErrNotFound)
	}

	delta := req.GetNewLimitMinorUnits() - card.GetLimitMinorUnits()
	if delta > 0 {
		check, err := s.ledger.CheckReserveFloor(ctx, card.GetTenantId(), delta)
		if err != nil {
			return nil, fmt.Errorf("cardsvc: check reserve floor for %s: %w", card.GetTenantId(), err)
		}
		s.recordHeadroom(card.GetTenantId(), check.GetHeadroomMinorUnits())
		if !check.GetAllowed() {
			return nil, fmt.Errorf(
				"cardsvc: insufficient reserve-floor headroom to raise limit for tenant %s: delta %d minor units, headroom %d minor units",
				card.GetTenantId(), delta, check.GetHeadroomMinorUnits(),
			)
		}
		if _, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId:       card.GetTenantId(),
			Type:           commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
			Amount:         &commonpb.Money{MinorUnits: delta, Currency: card.GetCurrency()},
			Reference:      cardReferencePrefix + card.GetAirwallexCardId(),
			IdempotencyKey: s.newID("card-hold-increase"),
			Memo:           req.GetReason(),
		}); err != nil {
			return nil, fmt.Errorf("cardsvc: apply limit increase hold for %s: %w", card.GetTenantId(), err)
		}
	} else if delta < 0 {
		if _, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId:       card.GetTenantId(),
			Type:           commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE,
			Amount:         &commonpb.Money{MinorUnits: -delta, Currency: card.GetCurrency()},
			Reference:      cardReferencePrefix + card.GetAirwallexCardId(),
			IdempotencyKey: s.newID("card-hold-release"),
			Memo:           req.GetReason(),
		}); err != nil {
			return nil, fmt.Errorf("cardsvc: release freed hold for %s: %w", card.GetTenantId(), err)
		}
	}

	if _, err := s.airwallex.SetCardControls(ctx, card.GetAirwallexCardId(), airwallex.CardControls{
		LimitMinorUnits: req.GetNewLimitMinorUnits(),
		LimitInterval:   card.GetLimitInterval(),
	}); err != nil {
		return nil, fmt.Errorf("cardsvc: update airwallex controls: %w", err)
	}

	s.mu.Lock()
	card.LimitMinorUnits = req.GetNewLimitMinorUnits()
	if delta < 0 {
		card.Status = commonpb.CardStatus_CARD_STATUS_RATIONED
	}
	updated := cloneCard(card)
	s.mu.Unlock()

	return updated, nil
}

// GetCard looks up a previously issued card by card-issuance's own ID.
func (s *Service) GetCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	card, ok := s.cards[cardID]
	if !ok {
		return nil, fmt.Errorf("cardsvc: %s: %w", cardID, airwallex.ErrNotFound)
	}
	return cloneCard(card), nil
}

// ListCards returns every card for a tenant (or every card if tenantID is
// empty).
func (s *Service) ListCards(ctx context.Context, tenantID string) ([]*cardissuancepb.Card, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*cardissuancepb.Card, 0, len(s.cards))
	for _, c := range s.cards {
		if tenantID != "" && c.GetTenantId() != tenantID {
			continue
		}
		out = append(out, cloneCard(c))
	}
	return out, nil
}

// recordHeadroom sets ration_reserve_floor_headroom_minor_units{tenant_id}
// if Metrics was wired in by main.go; a no-op otherwise (e.g. in unit
// tests constructing Service directly).
func (s *Service) recordHeadroom(tenantID string, headroomMinorUnits int64) {
	if s.Metrics == nil {
		return
	}
	s.Metrics.ReserveFloorHeadroom.WithLabelValues(tenantID).Set(float64(headroomMinorUnits))
}

// cloneCard returns a deep copy of c via proto.Clone — generated proto
// message structs embed a protoimpl.MessageState (which contains a
// sync.Mutex), so a plain `cp := *c` shallow copy would copy that lock
// value; proto.Clone is the safe way to hand callers their own copy
// while the store keeps the original under its mutex.
func cloneCard(c *cardissuancepb.Card) *cardissuancepb.Card {
	return proto.Clone(c).(*cardissuancepb.Card)
}
