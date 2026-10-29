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
	"log/slog"
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

	// cardLocks holds one *sync.Mutex per card id, lazily created,
	// never removed (same never-shrinks lifetime as cards itself). It
	// serializes AdjustCardLimit/CloseCard end-to-end (lock → read the
	// card's current LimitMinorUnits → call Airwallex → call
	// ledger-core's ApplyEntry → write the card's new
	// LimitMinorUnits/Status → unlock) for one specific card id, while
	// leaving every other card id free to proceed concurrently. See
	// lockCard's doc comment for the race this closes — without it,
	// two concurrent mutations of the SAME card read the same stale
	// "before" limit, so ledger-core correctly applies both deltas
	// (its own apply_entry is atomic and additive) but this service's
	// own cached LimitMinorUnits field ends up holding only the LAST
	// writer's value, permanently diverging from the real cumulative
	// amount ledger-core now holds for that card's reference — funds
	// that CloseCard/AdjustCardLimit's decrease path can then never
	// fully release, because both only ever release
	// card.GetLimitMinorUnits() (the wrong, smaller cached value), not
	// whatever ledger-core actually holds.
	cardLocks sync.Map // cardID string -> *sync.Mutex

	// persistPath is the on-disk file EnablePersistence was given, or
	// "" (the default: in-memory only, exactly this package's original
	// behavior) if it was never called — every existing caller
	// (main.go, this package's own and internal/grpcserver's tests)
	// keeps working unchanged. See EnablePersistence's doc comment for
	// why a process without it loses every card on restart.
	persistPath string

	now   func() time.Time
	newID func(prefix string) string

	// Metrics is optional (nil-safe); see bridge.Service.Metrics for why
	// this is a public field set post-construction rather than a New()
	// parameter.
	Metrics *metrics.Metrics
}

// New constructs a card Service. Card metadata lives only in memory
// until EnablePersistence is called — see its doc comment for why that
// matters.
func New(aw airwallex.Client, ledger LedgerClient) *Service {
	return &Service{
		airwallex: aw,
		ledger:    ledger,
		cards:     make(map[string]*cardissuancepb.Card),
		now:       time.Now,
		newID:     idgen.New,
	}
}

// EnablePersistence makes this Service durable across process restarts:
// it loads any previously-saved card metadata from path (a single JSON
// file card-issuance owns outright — there is no WAL or SQLite here,
// unlike ledger-core) into the in-memory map, then persists that map
// back to the same file after every successful IssueCard, AdjustCardLimit,
// and CloseCard.
//
// Without this, this package's own top-of-file doc comment ("the only
// place that persists card metadata") and card_issuance.proto's service
// comment ("stateless... safe to run as N replicas") describe a
// guarantee this type never actually provided: cards lived only in an
// unexported Go map, so a card-issuance restart silently lost every
// issued card's id/cardholder/limit/status while the ledger HOLD
// backing its limit stays on ledger-core's books forever — unreachable
// by GetCard/AdjustCardLimit/CloseCard ever again, since all three look
// the card up by id first. This closes that gap for the single-process
// restart case (the one this repo has actually hit, repeatedly, from
// crash loops and resource-contention-induced restarts — see
// BUILD-LOG-2026-10-23.md's rounds 3 and 7). It does NOT make running N
// replicas of this service actually safe: each replica would still load
// and save its own independent copy of path, so two replicas behind a
// load balancer can still diverge (one issues a card the other's
// GetCard/ListCards can't see) unless path is a shared, lock-coordinated
// filesystem — that needs a real shared store (the same class of gap
// already named and intentionally left open for services/auth's
// internal/apikey.Store), not a local JSON file, and is out of scope
// here.
func (s *Service) EnablePersistence(path string) error {
	loaded, err := loadCardsFile(path)
	if err != nil {
		return err
	}

	s.mu.Lock()
	for id, card := range loaded {
		s.cards[id] = card
	}
	s.mu.Unlock()

	s.persistPath = path
	return nil
}

// persist writes the current card map to disk if EnablePersistence was
// called, or is a no-op (returns nil, touches no filesystem) otherwise
// — so the package's own tests and internal/grpcserver's tests, neither
// of which call EnablePersistence, are unaffected by this change.
func (s *Service) persist() error {
	if s.persistPath == "" {
		return nil
	}

	s.mu.RLock()
	snapshot := make(map[string]*cardissuancepb.Card, len(s.cards))
	for id, card := range s.cards {
		snapshot[id] = card
	}
	s.mu.RUnlock()

	if err := saveCardsFile(s.persistPath, snapshot); err != nil {
		return fmt.Errorf("cardsvc: persist card store: %w", err)
	}
	return nil
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
	// This system has no FX conversion anywhere (ledger-core's own
	// apply_entry/record_deposit hard-reject any amount whose currency
	// doesn't exactly equal the tenant's own currency — see
	// services/ledger-core/src/ledger.rs's CurrencyMismatch checks) —
	// every card is really a hold against the tenant's single-currency
	// balance, never a converted one. Before this check, a caller could
	// pass --currency for a different ISO code than the tenant's own and
	// IssueCard would happily create a real Airwallex card for it,
	// because the request's limit_minor_units == 0 path never calls
	// CheckReserveFloor/ApplyEntry at all, so the mismatch was never
	// exercised at creation time. The card then sat there permanently
	// stuck at a zero limit: the very next "card adjust-limit" above
	// zero fails with a confusing low-level
	// "currency mismatch: USD vs EUR" error surfaced from ledger-core's
	// ApplyEntry, with no way to ever fund that card. Reproduced live:
	// `ration card issue --currency EUR --limit 0` against a USD tenant
	// succeeds silently; the following `card adjust-limit --limit
	// 100000` on that same card then fails with exactly that error.
	// Reject upfront instead, before any Airwallex card is created, so
	// the caller gets an immediate, actionable error instead of a
	// permanently unfundable card discovered only later.
	if currency != tenant.GetCurrency() {
		return nil, fmt.Errorf(
			"cardsvc: card currency %s does not match tenant %s's currency %s (no FX conversion supported; omit --currency to use the tenant's own)",
			currency, req.GetTenantId(), tenant.GetCurrency(),
		)
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

	// Best-effort, same discipline as ledger-core's own AuditMirror
	// (docs/THREAT-MODEL.md item 3): the Airwallex card and ledger HOLD
	// above are both real and already committed, with no safe way to
	// undo either from here, so a persist failure must not fail this
	// RPC — a client that saw an error here and retried IssueCard would
	// double-issue a card and double the ledger hold against the same
	// tenant, which is strictly worse than the original gap. Logged
	// loudly instead of swallowed; the next successful persist (the
	// very next card-issuance mutation) will still pick this card up.
	if err := s.persist(); err != nil {
		slog.Default().Error("cardsvc: failed to persist card store after IssueCard",
			"error", err, "card_id", card.Id, "tenant_id", card.TenantId)
	}

	return cloneCard(card), nil
}

// lockCard returns an unlock function that must be called (typically via
// defer) to release the per-card mutex for cardID, blocking until any
// other in-flight AdjustCardLimit/CloseCard for the SAME card id has
// fully finished (including its own Airwallex call, ledger-core
// ApplyEntry call, and cached-field update).
//
// Reproduced live, without this lock, against the real running stack
// (mock Airwallex, real ledger-core): issued a card with a $1,000.00
// limit (a real HOLD), then fired two concurrent AdjustCardLimit calls
// on that same card id — one requesting $2,000.00, the other
// $3,000.00 — both sharing one gRPC connection so they reached this
// service's goroutines genuinely concurrently. Both succeeded. Both
// deltas (+$1,000.00 and +$2,000.00) were correctly, atomically applied
// by ledger-core — the tenant's real Held balance became $4,000.00 —
// but this service's own card.LimitMinorUnits ended up at whichever
// call's absolute new_limit_minor_units happened to write last
// ($2,000.00), not the $4,000.00 actually held. The next CloseCard on
// that card then released only $2,000.00 (card.GetLimitMinorUnits(),
// the wrong cached value) — leaving $2,000.00 permanently stuck in the
// tenant's Held balance with zero live cards referencing it anymore,
// since every release path in this file is keyed off a card's own
// cached LimitMinorUnits, never off ledger-core's real per-reference
// total. That $2,000.00 could never be recovered by any code path in
// this repo.
//
// Serializing the full AdjustCardLimit/CloseCard critical section per
// card id closes this: the second concurrent call now blocks until the
// first has committed its own ledger mutation AND updated
// card.LimitMinorUnits, so it always computes its delta against the
// real, current value — the two increases above would now correctly
// compound to a final cached limit of $4,000.00, matching ledger-core's
// real Held balance exactly, the same way a single caller issuing two
// sequential adjustments always has.
func (s *Service) lockCard(cardID string) (unlock func()) {
	muIface, _ := s.cardLocks.LoadOrStore(cardID, &sync.Mutex{})
	cardMu := muIface.(*sync.Mutex)
	cardMu.Lock()
	return cardMu.Unlock
}

// suspendAfterFailure best-effort suspends an Airwallex card that was
// just created but cannot be safely backed by a ledger hold. Errors here
// are deliberately not surfaced to the caller: the primary error (the
// reserve-floor failure) is what matters, and we must not mask it with a
// cleanup failure, but we still want to try.
func (s *Service) suspendAfterFailure(ctx context.Context, awCardID string) {
	_, _ = s.airwallex.SetCardControls(ctx, awCardID, airwallex.CardControls{LimitMinorUnits: 0})
}

// revertCardControls is AdjustCardLimit's mirror of suspendAfterFailure:
// best-effort reverts an Airwallex card's limit back to its
// pre-adjustment value after the corresponding ledger mutation failed, so
// Airwallex's view of the limit doesn't stay ahead of what the ledger
// actually backs. Errors here are deliberately not surfaced — the
// primary error (the ledger failure) is what matters to the caller, but
// a failed revert is still worth logging loudly so an operator can
// reconcile Airwallex's limit by hand if it happens.
func (s *Service) revertCardControls(ctx context.Context, card *cardissuancepb.Card, limitMinorUnits int64) {
	if _, err := s.airwallex.SetCardControls(ctx, card.GetAirwallexCardId(), airwallex.CardControls{
		LimitMinorUnits: limitMinorUnits,
		LimitInterval:   card.GetLimitInterval(),
	}); err != nil {
		slog.Default().Error("cardsvc: failed to revert airwallex controls after ledger failure",
			"error", err, "card_id", card.GetId(), "airwallex_card_id", card.GetAirwallexCardId())
	}
}

// AdjustCardLimit changes a card's spend limit. Same order/compensation
// discipline as IssueCard (and for the same reason): Airwallex is called
// first, and the backing ledger mutation (an increase places an
// additional HOLD after re-checking reserve-floor headroom; a decrease
// places a RELEASE for the freed amount) is only applied once Airwallex
// has accepted the new limit. If the ledger mutation then fails, the
// Airwallex limit is reverted back to its pre-adjustment value so a
// retry re-applies the ledger mutation exactly once instead of stacking
// an unbounded HOLD/RELEASE on every failed retry (the reserve-floor
// headroom leak documented in round 14 — this was previously ledger
// first, Airwallex second, with no rollback on the Airwallex failure
// path).
func (s *Service) AdjustCardLimit(ctx context.Context, req *cardissuancepb.AdjustCardLimitRequest) (*cardissuancepb.Card, error) {
	if req.GetNewLimitMinorUnits() < 0 {
		return nil, fmt.Errorf("cardsvc: new_limit_minor_units must be >= 0, got %d", req.GetNewLimitMinorUnits())
	}

	// See lockCard's doc comment: this serializes the entire method
	// (read → Airwallex → ledger → write) against any other concurrent
	// AdjustCardLimit/CloseCard for this exact card id, so the oldLimit
	// read below is never stale relative to another in-flight mutation
	// of the same card.
	unlock := s.lockCard(req.GetCardId())
	defer unlock()

	s.mu.Lock()
	card, ok := s.cards[req.GetCardId()]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("cardsvc: %s: %w", req.GetCardId(), airwallex.ErrNotFound)
	}
	if card.GetStatus() == commonpb.CardStatus_CARD_STATUS_CLOSED {
		return nil, fmt.Errorf("cardsvc: %s is closed and cannot be adjusted", req.GetCardId())
	}

	oldLimit := card.GetLimitMinorUnits()
	delta := req.GetNewLimitMinorUnits() - oldLimit
	if delta > 0 {
		// Read-only: safe to do before touching Airwallex or the ledger.
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
	}

	if _, err := s.airwallex.SetCardControls(ctx, card.GetAirwallexCardId(), airwallex.CardControls{
		LimitMinorUnits: req.GetNewLimitMinorUnits(),
		LimitInterval:   card.GetLimitInterval(),
	}); err != nil {
		return nil, fmt.Errorf("cardsvc: update airwallex controls: %w", err)
	}

	if delta > 0 {
		if _, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId:       card.GetTenantId(),
			Type:           commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
			Amount:         &commonpb.Money{MinorUnits: delta, Currency: card.GetCurrency()},
			Reference:      cardReferencePrefix + card.GetAirwallexCardId(),
			IdempotencyKey: s.newID("card-hold-increase"),
			Memo:           req.GetReason(),
		}); err != nil {
			s.revertCardControls(ctx, card, oldLimit)
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
			s.revertCardControls(ctx, card, oldLimit)
			return nil, fmt.Errorf("cardsvc: release freed hold for %s: %w", card.GetTenantId(), err)
		}
	}

	s.mu.Lock()
	card.LimitMinorUnits = req.GetNewLimitMinorUnits()
	if delta < 0 {
		card.Status = commonpb.CardStatus_CARD_STATUS_RATIONED
	}
	updated := cloneCard(card)
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		slog.Default().Error("cardsvc: failed to persist card store after AdjustCardLimit",
			"error", err, "card_id", updated.Id, "tenant_id", updated.TenantId)
	}

	return updated, nil
}

// CloseCard permanently closes a card: marks it closed via Airwallex
// first, then releases the card's full remaining ledger hold back to the
// tenant's available balance (the mirror image of IssueCard's HOLD).
// Closing an already-closed card is idempotent — it returns the card
// unchanged rather than erroring or double-releasing a hold that was
// already freed.
//
// Airwallex is called before the ledger release (the reverse of this
// method's pre-round-15 order) for the same reserve-floor-leak reason as
// AdjustCardLimit's reorder: the old order released the ledger hold
// first, so a subsequent Airwallex failure left a real card still active
// with its original limit but no backing hold, and a naive retry would
// re-release the same hold a second time. Airwallex's own close action is
// terminal — there is no "reopen" to compensate with if the ledger
// release below then fails — but that failure mode is fail-safe, not a
// leak: the card is already closed (can't spend further) and the ledger
// hold simply stays in place until a retry successfully releases it,
// instead of being leaked twice.
func (s *Service) CloseCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error) {
	// Same per-card serialization as AdjustCardLimit, and for the same
	// reason — see lockCard's doc comment. Without it, a CloseCard
	// racing a concurrent AdjustCardLimit (or another CloseCard) on the
	// same card id can read card.GetLimitMinorUnits() before the other
	// call's own cached-field update lands, releasing the wrong amount.
	unlock := s.lockCard(cardID)
	defer unlock()

	s.mu.Lock()
	card, ok := s.cards[cardID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("cardsvc: %s: %w", cardID, airwallex.ErrNotFound)
	}

	if card.GetStatus() == commonpb.CardStatus_CARD_STATUS_CLOSED {
		return cloneCard(card), nil
	}

	if _, err := s.airwallex.CloseCard(ctx, card.GetAirwallexCardId()); err != nil {
		return nil, fmt.Errorf("cardsvc: close via airwallex: %w", err)
	}

	if card.GetLimitMinorUnits() > 0 {
		if _, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
			TenantId:       card.GetTenantId(),
			Type:           commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE,
			Amount:         &commonpb.Money{MinorUnits: card.GetLimitMinorUnits(), Currency: card.GetCurrency()},
			Reference:      cardReferencePrefix + card.GetAirwallexCardId(),
			IdempotencyKey: s.newID("card-hold-close"),
			Memo:           "card closed",
		}); err != nil {
			return nil, fmt.Errorf("cardsvc: release hold on close for %s: %w", card.GetTenantId(), err)
		}
	}

	s.mu.Lock()
	card.Status = commonpb.CardStatus_CARD_STATUS_CLOSED
	card.LimitMinorUnits = 0
	updated := cloneCard(card)
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		slog.Default().Error("cardsvc: failed to persist card store after CloseCard",
			"error", err, "card_id", updated.Id, "tenant_id", updated.TenantId)
	}

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
