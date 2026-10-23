// Package bridge implements the rationing math described in
// docs/AGENT-DESIGN.md: given a tenant's current headroom against its
// reserve floor, an expected-but-late deposit, and its recent bridge
// grant history, decide how much (if any) bridge funding can be safely
// granted right now.
//
// This is deliberately deterministic and side-effect-visible: every
// input (headroom, deposit amount, velocity state) and every output
// (granted amount, explanation) is computed here in Go, not delegated to
// an LLM. The agent service calls RationBridgeFunding over gRPC and can
// only ever receive an amount this package has already clamped to the
// rule-approved envelope — a prompt-injected or hallucinating LLM cannot
// move more money than this function allows, because this function (via
// ledgerclient.ApplyEntry) is the thing that actually asks ledger-core to
// move it.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/pkg/metrics"
	"github.com/caelum0x/ration/services/card-issuance/internal/idgen"
)

// bridgeReferencePrefix tags every bridge-grant HOLD/RELEASE ledger entry
// so the velocity check can find them again without a dedicated
// "list bridge grants" RPC on ledger-core.
//
// Every HOLD's reference is "bridge:<deposit_id>:<hold_idempotency_key>"
// — the deposit_id links the hold to the specific late deposit it is
// standing in for, so ledger-core's mark_deposit_received_with_entry can
// find and RELEASE it the instant that exact deposit clears (see
// docs/AGENT-DESIGN.md rule 3: a bridge grant is unblocked "unless the
// prior grant was fully repaid", which is now detectable by linkage, not
// just by waiting out the velocity window).
const bridgeReferencePrefix = "bridge:"

// bridgeReference builds the "bridge:<deposit_id>:<hold_idempotency_key>"
// reference tag for a HOLD entry backing depositID.
func bridgeReference(depositID, holdIdempotencyKey string) string {
	return fmt.Sprintf("%s%s:%s", bridgeReferencePrefix, depositID, holdIdempotencyKey)
}

// LedgerClient is the subset of ledgerclient.Client the bridge service
// needs. Defining it here (rather than depending on the concrete type)
// lets service_test.go exercise the policy math with a fake, with no
// gRPC, no network, and fully deterministic clocks.
type LedgerClient interface {
	GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error)
	GetBalance(ctx context.Context, tenantID string) (*ledgerpb.Balance, error)
	ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error)
	ListEntries(ctx context.Context, tenantID string) ([]*ledgerpb.LedgerEntry, error)
	ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error)
	RecordAgentDecision(ctx context.Context, req *ledgerpb.RecordAgentDecisionRequest) (*ledgerpb.AgentDecision, error)
}

// Policy holds the tunable (but never LLM-overridable) rationing
// parameters, sourced from config (RATION_BRIDGE_* env vars).
type Policy struct {
	// MaxFraction is the maximum fraction of an expected late deposit that
	// may ever be bridged in a single grant (rule 2). Default 0.6.
	MaxFraction float64
	// MinGrantMinorUnits is the minimum-useful-grant floor (rule 4):
	// computed grants below this are declined outright rather than issued
	// as a token amount. Default 500 ($5.00).
	MinGrantMinorUnits int64
	// VelocityWindow is the rolling window within which a tenant may have
	// at most one outstanding (unrepaid) bridge grant (rule 3). Default 24h.
	VelocityWindow time.Duration
}

// DefaultPolicy returns the hard-coded hackathon defaults, matching
// .env.example, for callers (tests, demos) that don't want to thread a
// full config.Config through.
func DefaultPolicy() Policy {
	return Policy{
		MaxFraction:        0.6,
		MinGrantMinorUnits: 500,
		VelocityWindow:     24 * time.Hour,
	}
}

// Service implements the RationBridgeFunding decision.
type Service struct {
	ledger LedgerClient
	policy Policy
	// now and newID are overridden in tests for determinism.
	now   func() time.Time
	newID func(prefix string) string

	// Metrics is optional (nil-safe): main.go sets it after construction
	// so RationBridgeFunding records ration_bridge_grants_total and
	// ration_reserve_floor_headroom_minor_units (docs/SCALE-EXPANSION.md
	// section 2) without every existing test's New(...) call site having
	// to thread a *metrics.Metrics through.
	Metrics *metrics.Metrics
}

// New constructs a bridge Service.
func New(ledger LedgerClient, policy Policy) *Service {
	return &Service{
		ledger: ledger,
		policy: policy,
		now:    time.Now,
		newID:  idgen.New,
	}
}

// Result is the outcome of a RationBridgeFunding decision.
type Result struct {
	GrantedMinorUnits int64
	FullyGranted      bool
	Explanation       string
}

// policySnapshot is recorded verbatim (as JSON) with every agent
// decision so a judge or the TUI's activity feed can see exactly which
// rule inputs produced the envelope, per docs/AGENT-DESIGN.md's audit
// trail requirement.
type policySnapshot struct {
	AvailableMinorUnits           int64   `json:"available_minor_units"`
	ReserveFloorMinorUnits        int64   `json:"reserve_floor_minor_units"`
	HeadroomMinorUnits            int64   `json:"headroom_minor_units"`
	ExpectedLateDepositMinorUnits int64   `json:"expected_late_deposit_minor_units"`
	MaxFraction                   float64 `json:"max_fraction"`
	BridgeCapMinorUnits           int64   `json:"bridge_cap_minor_units"`
	VelocityBlocked               bool    `json:"velocity_blocked"`
	MinGrantMinorUnits            int64   `json:"min_grant_minor_units"`
	RequestedMinorUnits           int64   `json:"requested_minor_units"`
	GrantedMinorUnits             int64   `json:"granted_minor_units"`
	ReserveFloorBreached          bool    `json:"reserve_floor_breached"`
}

// RationBridgeFunding computes and (if non-zero) applies a bridge grant
// for tenantID. It is safe to call concurrently for different tenants;
// correctness for a single tenant under concurrent calls ultimately rests
// on ledger-core's idempotent, reserve-floor-checked ApplyEntry (defense
// in depth per docs/AGENT-DESIGN.md), not on any lock in this package.
func (s *Service) RationBridgeFunding(ctx context.Context, tenantID string, requestedMinorUnits int64, reason string) (*Result, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("bridge: tenant_id is required")
	}
	if requestedMinorUnits <= 0 {
		return nil, fmt.Errorf("bridge: requested_minor_units must be > 0, got %d", requestedMinorUnits)
	}

	tenant, err := s.ledger.GetTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("bridge: get tenant %s: %w", tenantID, err)
	}
	balance, err := s.ledger.GetBalance(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("bridge: get balance for %s: %w", tenantID, err)
	}

	snap := policySnapshot{
		AvailableMinorUnits:    balance.GetAvailableMinorUnits(),
		ReserveFloorMinorUnits: tenant.GetReserveFloorMinorUnits(),
		MaxFraction:            s.policy.MaxFraction,
		MinGrantMinorUnits:     s.policy.MinGrantMinorUnits,
		RequestedMinorUnits:    requestedMinorUnits,
	}

	// Rule 1: reserve floor headroom. If available balance has already
	// fallen to or below the reserve floor, headroom is zero (never
	// negative-headroom math) and this is a reserve-floor breach — bridge
	// funding cannot touch the platform's own capital.
	headroom := snap.AvailableMinorUnits - snap.ReserveFloorMinorUnits
	if headroom < 0 {
		snap.ReserveFloorBreached = true
		headroom = 0
	}
	snap.HeadroomMinorUnits = headroom
	s.recordHeadroom(tenantID, headroom)

	// Rule 2: bridge cap. Never bridge more than MaxFraction of what's
	// actually expected from the tenant's late deposit.
	lateDeposit, depositErr := s.findRelevantLateDeposit(ctx, tenantID)
	if depositErr != nil {
		return nil, fmt.Errorf("bridge: find late deposit for %s: %w", tenantID, depositErr)
	}
	var bridgeCap int64
	if lateDeposit != nil {
		snap.ExpectedLateDepositMinorUnits = lateDeposit.GetExpectedAmount().GetMinorUnits()
		bridgeCap = int64(float64(snap.ExpectedLateDepositMinorUnits) * s.policy.MaxFraction)
	}
	snap.BridgeCapMinorUnits = bridgeCap

	// Rule 3: velocity cap. At most one outstanding (unrepaid) bridge
	// grant per tenant per rolling window.
	velocityBlocked, velocityErr := s.velocityBlocked(ctx, tenantID)
	if velocityErr != nil {
		return nil, fmt.Errorf("bridge: velocity check for %s: %w", tenantID, velocityErr)
	}
	snap.VelocityBlocked = velocityBlocked

	maxGrant := min64(headroom, bridgeCap)
	if velocityBlocked {
		maxGrant = 0
	}

	granted := min64(requestedMinorUnits, maxGrant)
	if granted < 0 {
		granted = 0
	}

	// Rule 4: partial over decline, unless below the minimum-useful-grant
	// floor, in which case decline outright with a clear reason.
	if granted < s.policy.MinGrantMinorUnits {
		snap.GrantedMinorUnits = 0
		explanation := declineExplanation(snap, lateDeposit, reason)
		if err := s.recordDecision(ctx, tenantID, reason, commonpb.AgentAction_AGENT_ACTION_DECLINE, explanation, snap); err != nil {
			return nil, err
		}
		s.recordGrant("declined")
		return &Result{GrantedMinorUnits: 0, FullyGranted: false, Explanation: explanation}, nil
	}

	snap.GrantedMinorUnits = granted
	fullyGranted := granted == requestedMinorUnits
	explanation := grantExplanation(snap, lateDeposit, fullyGranted, reason)

	// granted >= s.policy.MinGrantMinorUnits > 0 at this point (the decline
	// path above already returned), and granted is clamped to
	// min(headroom, bridgeCap), so bridgeCap > 0, which only happens when
	// lateDeposit != nil. lateDeposit.GetId() is therefore always safe to
	// use as the settlement-linkage key for this hold's reference.
	idempotencyKey := s.newID("bridge-hold")
	entryResp, err := s.ledger.ApplyEntry(ctx, &ledgerpb.ApplyEntryRequest{
		TenantId: tenantID,
		Type:     commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
		Amount: &commonpb.Money{
			MinorUnits: granted,
			Currency:   tenant.GetCurrency(),
		},
		Reference:      bridgeReference(lateDeposit.GetId(), idempotencyKey),
		IdempotencyKey: idempotencyKey,
		Memo:           fmt.Sprintf("bridge funding: %s", reason),
	})
	if err != nil {
		return nil, fmt.Errorf("bridge: apply hold entry for %s: %w", tenantID, err)
	}
	if entryResp.GetReserveFloorViolated() {
		// Defense in depth: ledger-core itself refused despite our
		// pre-check. Treat as a hard decline rather than trusting our own
		// math over the source of truth.
		snap.GrantedMinorUnits = 0
		snap.ReserveFloorBreached = true
		explanation = "ledger-core rejected the hold as a reserve-floor violation despite gateway pre-checks; declining"
		if err := s.recordDecision(ctx, tenantID, reason, commonpb.AgentAction_AGENT_ACTION_DECLINE, explanation, snap); err != nil {
			return nil, err
		}
		s.recordGrant("declined")
		return &Result{GrantedMinorUnits: 0, FullyGranted: false, Explanation: explanation}, nil
	}

	if err := s.recordDecision(ctx, tenantID, reason, commonpb.AgentAction_AGENT_ACTION_RATION_BRIDGE, explanation, snap); err != nil {
		return nil, err
	}
	if fullyGranted {
		s.recordGrant("granted")
	} else {
		s.recordGrant("partial")
	}

	return &Result{
		GrantedMinorUnits: granted,
		FullyGranted:      fullyGranted,
		Explanation:       explanation,
	}, nil
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

// recordGrant increments ration_bridge_grants_total{result} if Metrics
// was wired in by main.go; a no-op otherwise.
func (s *Service) recordGrant(result string) {
	if s.Metrics == nil {
		return
	}
	s.Metrics.BridgeGrantsTotal.WithLabelValues(result).Inc()
}

func (s *Service) recordDecision(ctx context.Context, tenantID, reason string, action commonpb.AgentAction, explanation string, snap policySnapshot) error {
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("bridge: marshal policy snapshot: %w", err)
	}
	if _, err := s.ledger.RecordAgentDecision(ctx, &ledgerpb.RecordAgentDecisionRequest{
		TenantId:           tenantID,
		Trigger:            "deposit_late",
		Action:             action,
		Rationale:          explanation,
		PolicySnapshotJson: string(snapJSON),
	}); err != nil {
		return fmt.Errorf("bridge: record agent decision for %s: %w", tenantID, err)
	}
	_ = reason // reason is folded into the memo/rationale above; kept as a param for API clarity
	return nil
}

// findRelevantLateDeposit returns the tenant's oldest currently-LATE
// deposit — the one that's actually overdue and that bridge funding is
// standing in for. Returns nil (not an error) if there is none.
func (s *Service) findRelevantLateDeposit(ctx context.Context, tenantID string) (*ledgerpb.Deposit, error) {
	deposits, err := s.ledger.ListDeposits(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var oldest *ledgerpb.Deposit
	for _, d := range deposits {
		if d.GetStatus() != commonpb.DepositStatus_DEPOSIT_STATUS_LATE {
			continue
		}
		if oldest == nil || d.GetExpectedAtUnix() < oldest.GetExpectedAtUnix() {
			oldest = d
		}
	}
	return oldest, nil
}

// velocityBlocked reports whether tenantID has an outstanding
// (unrepaid) bridge grant within the velocity window: a bridge HOLD
// entry created within the window that has no matching RELEASE entry
// referencing the same hold.
func (s *Service) velocityBlocked(ctx context.Context, tenantID string) (bool, error) {
	entries, err := s.ledger.ListEntries(ctx, tenantID)
	if err != nil {
		return false, err
	}

	released := make(map[string]bool)
	for _, e := range entries {
		if e.GetType() == commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE {
			released[e.GetReference()] = true
		}
	}

	cutoff := s.now().Add(-s.policy.VelocityWindow)
	for _, e := range entries {
		if e.GetType() != commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD {
			continue
		}
		ref := e.GetReference()
		if len(ref) < len(bridgeReferencePrefix) || ref[:len(bridgeReferencePrefix)] != bridgeReferencePrefix {
			continue
		}
		createdAt := time.Unix(e.GetCreatedAtUnix(), 0)
		if createdAt.Before(cutoff) {
			continue // outside the rolling window entirely
		}
		if !released[ref] {
			return true, nil
		}
	}
	return false, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func declineExplanation(snap policySnapshot, lateDeposit *ledgerpb.Deposit, reason string) string {
	switch {
	case snap.ReserveFloorBreached:
		return fmt.Sprintf(
			"declined %s: reserve floor breached (available %d <= reserve floor %d, headroom $0); cannot bridge against the platform's own capital",
			moneyLabel(snap.RequestedMinorUnits), snap.AvailableMinorUnits, snap.ReserveFloorMinorUnits,
		)
	case snap.VelocityBlocked:
		return fmt.Sprintf(
			"declined %s: tenant already has an unrepaid bridge grant within the last %s (velocity cap), reason=%q",
			moneyLabel(snap.RequestedMinorUnits), "24h", reason,
		)
	case lateDeposit == nil:
		return fmt.Sprintf(
			"declined %s: no late deposit on file for this tenant, nothing to bridge against",
			moneyLabel(snap.RequestedMinorUnits),
		)
	default:
		return fmt.Sprintf(
			"declined %s: computed grant of %s is below the $5 minimum-useful-grant floor (headroom %s, bridge cap %s of expected late deposit %s)",
			moneyLabel(snap.RequestedMinorUnits), moneyLabel(min64(snap.HeadroomMinorUnits, snap.BridgeCapMinorUnits)),
			moneyLabel(snap.HeadroomMinorUnits), moneyLabel(snap.BridgeCapMinorUnits), moneyLabel(snap.ExpectedLateDepositMinorUnits),
		)
	}
}

func grantExplanation(snap policySnapshot, lateDeposit *ledgerpb.Deposit, fullyGranted bool, reason string) string {
	verb := "granted"
	if !fullyGranted {
		verb = "partially granted"
	}
	return fmt.Sprintf(
		"%s %s of %s requested because headroom was %s after a %s reserve floor, bridge cap was %s (60%% of expected late deposit %s), reason=%q",
		verb, moneyLabel(snap.GrantedMinorUnits), moneyLabel(snap.RequestedMinorUnits),
		moneyLabel(snap.HeadroomMinorUnits), moneyLabel(snap.ReserveFloorMinorUnits),
		moneyLabel(snap.BridgeCapMinorUnits), moneyLabel(snap.ExpectedLateDepositMinorUnits), reason,
	)
}

func moneyLabel(minorUnits int64) string {
	return fmt.Sprintf("$%.2f", float64(minorUnits)/100.0)
}
