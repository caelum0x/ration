package bridge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
)

// fakeLedger is an in-memory, fully deterministic stand-in for
// ledgerclient.Client that satisfies bridge.LedgerClient. It never makes
// a network call, so these tests exercise only the rationing math.
type fakeLedger struct {
	tenant   *ledgerpb.Tenant
	balance  *ledgerpb.Balance
	deposits []*ledgerpb.Deposit
	entries  []*ledgerpb.LedgerEntry

	appliedEntries    []*ledgerpb.ApplyEntryRequest
	recordedDecisions []*ledgerpb.RecordAgentDecisionRequest

	// applyEntryReserveFloorViolated, when set, makes the next ApplyEntry
	// call report a reserve-floor violation from ledger-core's side (the
	// defense-in-depth path).
	applyEntryReserveFloorViolated bool
	applyEntryErr                  error

	nextEntryID int
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

func (f *fakeLedger) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	f.appliedEntries = append(f.appliedEntries, req)
	if f.applyEntryErr != nil {
		return nil, f.applyEntryErr
	}
	f.nextEntryID++
	balanceAfter := f.balance.GetAvailableMinorUnits() - req.GetAmount().GetMinorUnits()
	return &ledgerpb.ApplyEntryResponse{
		Entry: &ledgerpb.LedgerEntry{
			Id:                     "entry_test",
			TenantId:               req.GetTenantId(),
			Type:                   req.GetType(),
			Amount:                 req.GetAmount(),
			BalanceAfterMinorUnits: balanceAfter,
			Reference:              req.GetReference(),
			Memo:                   req.GetMemo(),
		},
		BalanceAfter: &ledgerpb.Balance{
			TenantId:            req.GetTenantId(),
			AvailableMinorUnits: balanceAfter,
		},
		ReserveFloorViolated: f.applyEntryReserveFloorViolated,
	}, nil
}

func (f *fakeLedger) RecordAgentDecision(ctx context.Context, req *ledgerpb.RecordAgentDecisionRequest) (*ledgerpb.AgentDecision, error) {
	f.recordedDecisions = append(f.recordedDecisions, req)
	return &ledgerpb.AgentDecision{Id: "decision_test", TenantId: req.GetTenantId()}, nil
}

func newTestService(f *fakeLedger, policy Policy) *Service {
	s := New(f, policy)
	s.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	s.newID = func(prefix string) string { return prefix + "_fixed" }
	return s
}

func lateDeposit(tenantID string, expectedMinorUnits, expectedAtUnix int64) *ledgerpb.Deposit {
	return &ledgerpb.Deposit{
		Id:       "dep_1",
		TenantId: tenantID,
		ExpectedAmount: &commonpb.Money{
			MinorUnits: expectedMinorUnits,
			Currency:   "USD",
		},
		ExpectedAtUnix: expectedAtUnix,
		Status:         commonpb.DepositStatus_DEPOSIT_STATUS_LATE,
	}
}

// TestRationBridgeFunding_PartialGrant covers the case where the full
// requested amount cannot be granted (headroom is the binding
// constraint) but the computed amount clears the minimum-useful-grant
// floor, so AGENT-DESIGN.md rule 4 ("partial over decline") applies: the
// tenant gets the largest safe amount, not a flat decline.
func TestRationBridgeFunding_PartialGrant(t *testing.T) {
	tenantID := "tenant_partial"
	f := &fakeLedger{
		tenant: &ledgerpb.Tenant{
			Id:                     tenantID,
			Currency:               "USD",
			ReserveFloorMinorUnits: 20000, // $200 reserve floor
		},
		balance: &ledgerpb.Balance{
			TenantId:            tenantID,
			AvailableMinorUnits: 23400, // $234 available -> $34 headroom
		},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 100000, 1_999_000_000), // $1000 expected late deposit
		},
	}
	svc := newTestService(f, DefaultPolicy())

	// Requested $600; bridge cap = 60% of $1000 = $600; headroom = $34.
	// min(headroom, cap) = $34, which is >= the $5 floor, so this should
	// be a partial grant of exactly $34.00 (3400 minor units).
	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 60000, "payout delayed")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 3400 {
		t.Errorf("GrantedMinorUnits = %d, want 3400", result.GrantedMinorUnits)
	}
	if result.FullyGranted {
		t.Error("FullyGranted = true, want false for a partial grant")
	}
	if len(f.appliedEntries) != 1 {
		t.Fatalf("len(appliedEntries) = %d, want 1", len(f.appliedEntries))
	}
	applied := f.appliedEntries[0]
	if applied.GetAmount().GetMinorUnits() != 3400 {
		t.Errorf("applied HOLD amount = %d, want 3400", applied.GetAmount().GetMinorUnits())
	}
	if applied.GetType() != commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD {
		t.Errorf("applied entry type = %v, want HOLD", applied.GetType())
	}
	if len(f.recordedDecisions) != 1 {
		t.Fatalf("len(recordedDecisions) = %d, want 1", len(f.recordedDecisions))
	}
	if f.recordedDecisions[0].GetAction() != commonpb.AgentAction_AGENT_ACTION_RATION_BRIDGE {
		t.Errorf("decision action = %v, want AGENT_ACTION_RATION_BRIDGE", f.recordedDecisions[0].GetAction())
	}
}

// TestRationBridgeFunding_ReserveFloorBreach covers rule 1: when the
// tenant's available balance is already at or below its reserve floor,
// headroom is zero (never negative) and the request is declined outright
// — bridge funding must never eat into the platform's own capital.
func TestRationBridgeFunding_ReserveFloorBreach(t *testing.T) {
	tenantID := "tenant_breach"
	f := &fakeLedger{
		tenant: &ledgerpb.Tenant{
			Id:                     tenantID,
			Currency:               "USD",
			ReserveFloorMinorUnits: 20000, // $200 reserve floor
		},
		balance: &ledgerpb.Balance{
			TenantId:            tenantID,
			AvailableMinorUnits: 15000, // $150 available: already below the floor
		},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 100000, 1_999_000_000),
		},
	}
	svc := newTestService(f, DefaultPolicy())

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "payout delayed")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 0 {
		t.Errorf("GrantedMinorUnits = %d, want 0 (reserve floor breached)", result.GrantedMinorUnits)
	}
	if result.FullyGranted {
		t.Error("FullyGranted = true, want false")
	}
	if len(f.appliedEntries) != 0 {
		t.Errorf("len(appliedEntries) = %d, want 0 — a breach must never touch the ledger", len(f.appliedEntries))
	}
	if len(f.recordedDecisions) != 1 {
		t.Fatalf("len(recordedDecisions) = %d, want 1", len(f.recordedDecisions))
	}
	if f.recordedDecisions[0].GetAction() != commonpb.AgentAction_AGENT_ACTION_DECLINE {
		t.Errorf("decision action = %v, want AGENT_ACTION_DECLINE", f.recordedDecisions[0].GetAction())
	}
}

// TestRationBridgeFunding_FullGrant: plenty of headroom and bridge cap
// above the requested amount -> the full request is granted.
func TestRationBridgeFunding_FullGrant(t *testing.T) {
	tenantID := "tenant_full"
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 10000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 100000}, // $1000 headroom = $900
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 1000000, 1_999_000_000), // $10000 expected -> cap $6000
		},
	}
	svc := newTestService(f, DefaultPolicy())

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 50000, "payout delayed")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 50000 {
		t.Errorf("GrantedMinorUnits = %d, want 50000", result.GrantedMinorUnits)
	}
	if !result.FullyGranted {
		t.Error("FullyGranted = false, want true")
	}
}

// TestRationBridgeFunding_BelowMinimumUsefulGrant: headroom is positive
// but tiny, so the computed grant falls below the $5 minimum-useful-grant
// floor and must be declined outright rather than issued as a token hold.
func TestRationBridgeFunding_BelowMinimumUsefulGrant(t *testing.T) {
	tenantID := "tenant_tiny"
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 20000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 20300}, // $3 headroom
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 100000, 1_999_000_000),
		},
	}
	svc := newTestService(f, DefaultPolicy())

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "payout delayed")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 0 {
		t.Errorf("GrantedMinorUnits = %d, want 0 (below $5 minimum-useful-grant floor)", result.GrantedMinorUnits)
	}
	if len(f.appliedEntries) != 0 {
		t.Errorf("len(appliedEntries) = %d, want 0", len(f.appliedEntries))
	}
}

// TestRationBridgeFunding_NoLateDeposit: without an expected late
// deposit on file, there is nothing to bridge against (bridge cap is
// zero), so the request is declined regardless of headroom.
func TestRationBridgeFunding_NoLateDeposit(t *testing.T) {
	tenantID := "tenant_nodeposit"
	f := &fakeLedger{
		tenant:   &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance:  &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 1000000},
		deposits: nil,
	}
	svc := newTestService(f, DefaultPolicy())

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "no deposit on file")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 0 {
		t.Errorf("GrantedMinorUnits = %d, want 0 (no late deposit to bridge against)", result.GrantedMinorUnits)
	}
}

// TestRationBridgeFunding_VelocityCapBlocksSecondGrant: rule 3 — a prior
// bridge HOLD within the rolling window that has not been released (no
// matching RELEASE entry) blocks a second grant even though headroom and
// the bridge cap would otherwise allow it.
func TestRationBridgeFunding_VelocityCapBlocksSecondGrant(t *testing.T) {
	tenantID := "tenant_velocity"
	now := time.Unix(2_000_000_000, 0)
	priorHoldAt := now.Add(-2 * time.Hour).Unix() // well within the 24h window

	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 1000000},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 1000000, 1_999_000_000),
		},
		entries: []*ledgerpb.LedgerEntry{
			{
				Id:            "entry_prior_hold",
				TenantId:      tenantID,
				Type:          commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
				Amount:        &commonpb.Money{MinorUnits: 5000, Currency: "USD"},
				Reference:     "bridge:bridge-hold_prior",
				CreatedAtUnix: priorHoldAt,
				// No matching RELEASE entry below -> still outstanding.
			},
		},
	}
	svc := newTestService(f, DefaultPolicy())
	svc.now = func() time.Time { return now }

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "second request same day")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 0 {
		t.Errorf("GrantedMinorUnits = %d, want 0 (velocity cap should block a second outstanding grant)", result.GrantedMinorUnits)
	}
	if len(f.appliedEntries) != 0 {
		t.Errorf("len(appliedEntries) = %d, want 0", len(f.appliedEntries))
	}
}

// TestRationBridgeFunding_VelocityCapAllowsAfterRepayment: a prior bridge
// HOLD that *has* been released (fully repaid) within the window does not
// block a new grant.
func TestRationBridgeFunding_VelocityCapAllowsAfterRepayment(t *testing.T) {
	tenantID := "tenant_repaid"
	now := time.Unix(2_000_000_000, 0)
	priorHoldAt := now.Add(-2 * time.Hour).Unix()

	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 1000000},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 1000000, 1_999_000_000),
		},
		entries: []*ledgerpb.LedgerEntry{
			{
				Id:            "entry_prior_hold",
				TenantId:      tenantID,
				Type:          commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_HOLD,
				Amount:        &commonpb.Money{MinorUnits: 5000, Currency: "USD"},
				Reference:     "bridge:bridge-hold_prior",
				CreatedAtUnix: priorHoldAt,
			},
			{
				Id:            "entry_release",
				TenantId:      tenantID,
				Type:          commonpb.LedgerEntryType_LEDGER_ENTRY_TYPE_RELEASE,
				Amount:        &commonpb.Money{MinorUnits: 5000, Currency: "USD"},
				Reference:     "bridge:bridge-hold_prior",
				CreatedAtUnix: priorHoldAt + 3600,
			},
		},
	}
	svc := newTestService(f, DefaultPolicy())
	svc.now = func() time.Time { return now }

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "after repayment")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 10000 {
		t.Errorf("GrantedMinorUnits = %d, want 10000 (prior grant was repaid, velocity cap should not apply)", result.GrantedMinorUnits)
	}
}

// TestRationBridgeFunding_LedgerCoreRejectsDespitePreCheck exercises the
// defense-in-depth path: even if gateway's own math approves a grant,
// ledger-core reporting ReserveFloorViolated must still result in a hard
// decline, not a silently-wrong "granted" response.
func TestRationBridgeFunding_LedgerCoreRejectsDespitePreCheck(t *testing.T) {
	tenantID := "tenant_defense_in_depth"
	f := &fakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 1000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 1000000},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 1000000, 1_999_000_000),
		},
		applyEntryReserveFloorViolated: true,
	}
	svc := newTestService(f, DefaultPolicy())

	result, err := svc.RationBridgeFunding(context.Background(), tenantID, 10000, "edge case")
	if err != nil {
		t.Fatalf("RationBridgeFunding() error = %v", err)
	}
	if result.GrantedMinorUnits != 0 {
		t.Errorf("GrantedMinorUnits = %d, want 0 when ledger-core reports a reserve floor violation", result.GrantedMinorUnits)
	}
	if len(f.recordedDecisions) != 1 || f.recordedDecisions[0].GetAction() != commonpb.AgentAction_AGENT_ACTION_DECLINE {
		t.Errorf("expected a single DECLINE decision to be recorded, got %+v", f.recordedDecisions)
	}
}

func TestRationBridgeFunding_InvalidInput(t *testing.T) {
	f := &fakeLedger{}
	svc := newTestService(f, DefaultPolicy())

	if _, err := svc.RationBridgeFunding(context.Background(), "", 100, "x"); err == nil {
		t.Error("expected error for empty tenant_id")
	}
	if _, err := svc.RationBridgeFunding(context.Background(), "tenant", 0, "x"); err == nil {
		t.Error("expected error for zero requested_minor_units")
	}
	if _, err := svc.RationBridgeFunding(context.Background(), "tenant", -500, "x"); err == nil {
		t.Error("expected error for negative requested_minor_units")
	}
}

// concurrencyFakeLedger is a second, mutex-protected LedgerClient fake
// (separate from fakeLedger above, which is only ever driven
// sequentially by the other tests in this file) used solely by
// TestRationBridgeFunding_ConcurrentCallsRespectVelocityCap below. Its
// ApplyEntry feeds applied HOLD/RELEASE entries back into the list
// ListEntries returns and decrements the live balance — unlike
// fakeLedger's static entries field — so that a second, truly
// concurrent RationBridgeFunding call for the same tenant sees
// whatever the first call has already committed, the same way a real
// ledger-core read would. Without that live feedback loop this test
// could not distinguish "the lock closed the race" from "there was
// nothing for a second call to race against."
type concurrencyFakeLedger struct {
	mu       sync.Mutex
	tenant   *ledgerpb.Tenant
	balance  *ledgerpb.Balance
	deposits []*ledgerpb.Deposit
	entries  []*ledgerpb.LedgerEntry

	recordedDecisions []*ledgerpb.RecordAgentDecisionRequest
	nextEntryID       int
}

func (f *concurrencyFakeLedger) GetTenant(ctx context.Context, tenantID string) (*ledgerpb.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tenant, nil
}

func (f *concurrencyFakeLedger) GetBalance(ctx context.Context, tenantID string) (*ledgerpb.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &ledgerpb.Balance{
		TenantId:            f.balance.GetTenantId(),
		AvailableMinorUnits: f.balance.GetAvailableMinorUnits(),
		HeldMinorUnits:      f.balance.GetHeldMinorUnits(),
	}, nil
}

func (f *concurrencyFakeLedger) ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deposits, nil
}

func (f *concurrencyFakeLedger) ListEntries(ctx context.Context, tenantID string) ([]*ledgerpb.LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*ledgerpb.LedgerEntry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func (f *concurrencyFakeLedger) ApplyEntry(ctx context.Context, req *ledgerpb.ApplyEntryRequest) (*ledgerpb.ApplyEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextEntryID++
	balanceAfter := f.balance.GetAvailableMinorUnits() - req.GetAmount().GetMinorUnits()
	f.balance.AvailableMinorUnits = balanceAfter
	entry := &ledgerpb.LedgerEntry{
		Id:                     fmt.Sprintf("entry_%d", f.nextEntryID),
		TenantId:               req.GetTenantId(),
		Type:                   req.GetType(),
		Amount:                 req.GetAmount(),
		BalanceAfterMinorUnits: balanceAfter,
		Reference:              req.GetReference(),
		Memo:                   req.GetMemo(),
		CreatedAtUnix:          2_000_000_000,
	}
	f.entries = append(f.entries, entry)
	return &ledgerpb.ApplyEntryResponse{
		Entry:        entry,
		BalanceAfter: &ledgerpb.Balance{TenantId: req.GetTenantId(), AvailableMinorUnits: balanceAfter},
	}, nil
}

func (f *concurrencyFakeLedger) RecordAgentDecision(ctx context.Context, req *ledgerpb.RecordAgentDecisionRequest) (*ledgerpb.AgentDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordedDecisions = append(f.recordedDecisions, req)
	return &ledgerpb.AgentDecision{Id: "decision_test", TenantId: req.GetTenantId()}, nil
}

// TestRationBridgeFunding_ConcurrentCallsRespectVelocityCap: two real
// concurrent RationBridgeFunding calls for the SAME tenant, racing
// against the SAME single late deposit. Reproduced live against the
// real running stack before this fix (see RationBridgeFunding's doc
// comment): both calls read velocityBlocked=false before either HOLD
// landed, so both were granted — $12,000.00 total held against a
// $10,000.00 late deposit capped at 60%, and two simultaneous
// outstanding grants where rule 3 allows at most one. With lockTenant
// serializing the full check-then-act critical section per tenant,
// exactly one of the two calls here must see the other's committed HOLD
// and decline on the velocity cap — run with `go test -race` to also
// catch any data race in the critical section itself, not just the
// logical double-grant.
func TestRationBridgeFunding_ConcurrentCallsRespectVelocityCap(t *testing.T) {
	tenantID := "tenant_bridge_race"
	f := &concurrencyFakeLedger{
		tenant:  &ledgerpb.Tenant{Id: tenantID, Currency: "USD", ReserveFloorMinorUnits: 1_000_000},
		balance: &ledgerpb.Balance{TenantId: tenantID, AvailableMinorUnits: 10_000_000},
		deposits: []*ledgerpb.Deposit{
			lateDeposit(tenantID, 1_000_000, 1_999_000_000),
		},
	}
	svc := New(f, DefaultPolicy())
	svc.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	svc.newID = func(prefix string) string { return prefix + "_fixed" }

	const concurrentCalls = 2
	requested := int64(600_000) // = the full 60% bridge cap of the $10,000.00 late deposit
	results := make([]*Result, concurrentCalls)
	errs := make([]error, concurrentCalls)
	var wg sync.WaitGroup
	for i := 0; i < concurrentCalls; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = svc.RationBridgeFunding(context.Background(), tenantID, requested, "race")
		}(i)
	}
	wg.Wait()

	var totalGranted int64
	var grantedCount int
	for i := 0; i < concurrentCalls; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: RationBridgeFunding() error = %v", i, errs[i])
		}
		totalGranted += results[i].GrantedMinorUnits
		if results[i].GrantedMinorUnits > 0 {
			grantedCount++
		}
	}

	if totalGranted != requested {
		t.Errorf("totalGranted = %d, want %d (exactly one call should be granted the full bridge cap; the velocity cap must block the other)", totalGranted, requested)
	}
	if grantedCount != 1 {
		t.Errorf("grantedCount = %d, want 1 (exactly one of the two concurrent calls should succeed)", grantedCount)
	}
}
