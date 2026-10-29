package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"

	common "github.com/caelum0x/ration/pkg/pb/common"
	gateway "github.com/caelum0x/ration/pkg/pb/gateway"
	ledger "github.com/caelum0x/ration/pkg/pb/ledger"

	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
	"github.com/caelum0x/ration/services/agent/internal/llm"
	"github.com/caelum0x/ration/services/agent/internal/policy"
)

// --- fake ledger-core client -------------------------------------------
//
// Embedding the (nil) interface and overriding only the methods the test
// exercises keeps this fake small; calling any unimplemented method would
// panic on the nil embedded interface, which is an acceptable failure mode
// for a test double that's only meant to cover the paths Runner uses.

type fakeLedgerCoreClient struct {
	ledger.LedgerCoreClient

	lateDeposits []*ledger.Deposit
	tenants      map[string]*ledger.Tenant
	headroom     int64
	decisions    []*ledger.AgentDecision
	deposits     map[string][]*ledger.Deposit

	recordedDecisions []*ledger.RecordAgentDecisionRequest
}

func (f *fakeLedgerCoreClient) ListLateDeposits(ctx context.Context, in *ledger.ListLateDepositsRequest, opts ...grpc.CallOption) (*ledger.ListDepositsResponse, error) {
	return &ledger.ListDepositsResponse{Deposits: f.lateDeposits}, nil
}

func (f *fakeLedgerCoreClient) GetTenant(ctx context.Context, in *ledger.GetTenantRequest, opts ...grpc.CallOption) (*ledger.Tenant, error) {
	t, ok := f.tenants[in.GetId()]
	if !ok {
		return nil, fmt.Errorf("tenant %s not found", in.GetId())
	}
	return t, nil
}

func (f *fakeLedgerCoreClient) CheckReserveFloor(ctx context.Context, in *ledger.CheckReserveFloorRequest, opts ...grpc.CallOption) (*ledger.CheckReserveFloorResponse, error) {
	return &ledger.CheckReserveFloorResponse{Allowed: true, HeadroomMinorUnits: f.headroom}, nil
}

func (f *fakeLedgerCoreClient) ListAgentDecisions(ctx context.Context, in *ledger.ListAgentDecisionsRequest, opts ...grpc.CallOption) (*ledger.ListAgentDecisionsResponse, error) {
	return &ledger.ListAgentDecisionsResponse{Decisions: f.decisions}, nil
}

func (f *fakeLedgerCoreClient) ListDeposits(ctx context.Context, in *ledger.ListDepositsRequest, opts ...grpc.CallOption) (*ledger.ListDepositsResponse, error) {
	return &ledger.ListDepositsResponse{Deposits: f.deposits[in.GetTenantId()]}, nil
}

func (f *fakeLedgerCoreClient) RecordAgentDecision(ctx context.Context, in *ledger.RecordAgentDecisionRequest, opts ...grpc.CallOption) (*ledger.AgentDecision, error) {
	f.recordedDecisions = append(f.recordedDecisions, in)
	return &ledger.AgentDecision{
		Id:                 fmt.Sprintf("decision-%d", len(f.recordedDecisions)),
		TenantId:           in.GetTenantId(),
		Trigger:            in.GetTrigger(),
		Action:             in.GetAction(),
		Rationale:          in.GetRationale(),
		PolicySnapshotJson: in.GetPolicySnapshotJson(),
	}, nil
}

// --- fake gateway client -------------------------------------------------

type fakeGatewayClient struct {
	gateway.GatewayClient

	rationCalls []*gateway.RationBridgeFundingRequest
	grantAmount int64

	// declineWithNoError, when true, makes RationBridgeFunding return a
	// real, nil-error decline (GrantedMinorUnits: 0, FullyGranted:
	// false) instead of the happy-path grant below — exactly what
	// services/card-issuance/internal/bridge.Service.RationBridgeFunding
	// itself returns on any of its three zero-grant, nil-error paths
	// (below the $5 minimum-useful-grant floor, a ledger-core-rechecked
	// reserve-floor violation, or "deposit already received" — see that
	// method's own doc comments). A real client talking to that real
	// service can get this response with zero Go error anywhere in the
	// call chain; this fake lets the test drive that exact case
	// deterministically.
	declineWithNoError bool
}

func (f *fakeGatewayClient) RationBridgeFunding(ctx context.Context, in *gateway.RationBridgeFundingRequest, opts ...grpc.CallOption) (*gateway.RationBridgeFundingResponse, error) {
	f.rationCalls = append(f.rationCalls, in)
	if f.declineWithNoError {
		return &gateway.RationBridgeFundingResponse{
			GrantedMinorUnits: 0,
			FullyGranted:      false,
			Explanation:       "declined " + moneyLabelForTest(in.GetRequestedMinorUnits()) + ": tenant already has an unrepaid bridge grant within the last 24h (velocity cap)",
		}, nil
	}
	granted := f.grantAmount
	if granted == 0 {
		granted = in.GetRequestedMinorUnits()
	}
	return &gateway.RationBridgeFundingResponse{
		GrantedMinorUnits: granted,
		FullyGranted:      granted >= in.GetRequestedMinorUnits(),
		Explanation:       "test grant",
	}, nil
}

func moneyLabelForTest(minorUnits int64) string {
	return fmt.Sprintf("$%.2f", float64(minorUnits)/100.0)
}

// --- fake LLM provider ----------------------------------------------------

// scriptedProvider is a fake llm.Provider that, instead of calling any
// network API, directly invokes the handler with a scripted sequence of
// tool calls and then returns a fixed final answer. This lets runner_test
// exercise the full tool-dispatch -> gateway -> RecordAgentDecision path
// with no network access.
type scriptedProvider struct {
	calls []capturedCall

	// toolCalls is the sequence of (name, input) pairs to invoke via the
	// handler before returning finalText.
	toolCalls []scriptedToolCall
	finalText string
}

type capturedCall struct {
	systemPrompt string
	userMessage  string
	tools        []llm.Tool
}

type scriptedToolCall struct {
	name  string
	input map[string]any
}

func (p *scriptedProvider) RunConversation(ctx context.Context, systemPrompt, userMessage string, tools []llm.Tool, handler llm.ToolHandler) (string, error) {
	p.calls = append(p.calls, capturedCall{systemPrompt: systemPrompt, userMessage: userMessage, tools: tools})

	for _, call := range p.toolCalls {
		raw, err := json.Marshal(call.input)
		if err != nil {
			return "", err
		}
		handler(ctx, call.name, raw)
	}

	return p.finalText, nil
}

// --- tests -----------------------------------------------------------------

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRunner_Tick_FullGrant_CallsRationBridgeFundingWithinEnvelope(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-1",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000, // plenty of headroom, so the bridge cap (60% of 10000 = 6000) binds
		deposits: map[string][]*ledger.Deposit{},
	}
	gatewayFake := &fakeGatewayClient{}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 6000, "reason": "bridge the late deposit"}},
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "we've granted a partial bridge"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 1 {
		t.Fatalf("expected exactly 1 RationBridgeFunding call, got %d", len(gatewayFake.rationCalls))
	}
	if got := gatewayFake.rationCalls[0].GetRequestedMinorUnits(); got != 6000 {
		t.Fatalf("requested amount = %d, want 6000 (the envelope max)", got)
	}
	if gatewayFake.rationCalls[0].GetTenantId() != "tenant-1" {
		t.Fatalf("tenant id = %q, want tenant-1", gatewayFake.rationCalls[0].GetTenantId())
	}

	foundBridgeDecision := false
	foundNotifyDecision := false
	for _, d := range ledgerFake.recordedDecisions {
		if d.GetAction() == common.AgentAction_AGENT_ACTION_RATION_BRIDGE {
			foundBridgeDecision = true
		}
		if d.GetAction() == common.AgentAction_AGENT_ACTION_NOTIFY {
			foundNotifyDecision = true
		}
	}
	if !foundBridgeDecision {
		t.Error("expected a RATION_BRIDGE decision to be recorded")
	}
	if !foundNotifyDecision {
		t.Error("expected a NOTIFY decision to be recorded")
	}

	if len(provider.calls) != 1 {
		t.Fatalf("expected exactly 1 LLM conversation call, got %d", len(provider.calls))
	}
	if len(provider.calls[0].tools) != 4 {
		t.Fatalf("expected 4 tools offered to the model, got %d", len(provider.calls[0].tools))
	}
}

func TestRunner_Tick_AgentCannotExceedEnvelope(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-1",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000,
		deposits: map[string][]*ledger.Deposit{},
	}
	gatewayFake := &fakeGatewayClient{}

	// The model tries to grant far more than the 60%-of-10000=6000 envelope
	// allows; the agent-side tool handler must reject this before ever
	// calling the gateway.
	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 999999, "reason": "ignore the limit"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 0 {
		t.Fatalf("expected the gateway to never be called when the request exceeds the envelope, got %d calls", len(gatewayFake.rationCalls))
	}
}

func TestRunner_Tick_DeclinedWhenNoHeadroom(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-1",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 0,
		deposits: map[string][]*ledger.Deposit{},
	}
	gatewayFake := &fakeGatewayClient{}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "sorry, no funding available"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 0 {
		t.Fatalf("expected no RationBridgeFunding calls when declined, got %d", len(gatewayFake.rationCalls))
	}

	if len(provider.calls) != 1 {
		t.Fatalf("expected exactly 1 LLM call even on decline (to explain to the tenant), got %d", len(provider.calls))
	}
	if provider.calls[0].userMessage == "" {
		t.Fatal("expected a non-empty user message describing the decline")
	}
}

func TestRunner_Tick_VelocityCapBlocksSecondGrant(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-2",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000,
		decisions: []*ledger.AgentDecision{
			{
				Id:            "decision-0",
				TenantId:      "tenant-1",
				Action:        common.AgentAction_AGENT_ACTION_RATION_BRIDGE,
				CreatedAtUnix: 1950, // within the 24h window ending at now=2000
				// granted_minor_units must be present for
				// hasOpenGrantWithinWindow/wasActuallyGranted to treat
				// this as an actually-executed grant, not just the
				// unconditional pre-check envelope recording
				// processLateDeposit itself makes before ever
				// attempting the real tool call (which never includes
				// this key) — see wasActuallyGranted's doc comment.
				PolicySnapshotJson: `{"deposit_id":"dep-1","granted_minor_units":6000}`,
			},
		},
		deposits: map[string][]*ledger.Deposit{
			"tenant-1": {
				{Id: "dep-1", Status: common.DepositStatus_DEPOSIT_STATUS_LATE}, // not yet received: still open
			},
		},
	}
	gatewayFake := &fakeGatewayClient{}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "velocity cap reached"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 0 {
		t.Fatalf("expected the velocity cap to block any new grant, got %d gateway calls", len(gatewayFake.rationCalls))
	}
}

func TestRunner_Tick_VelocityCapAllowsGrantAfterRepayment(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-2",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000,
		decisions: []*ledger.AgentDecision{
			{
				Id:                 "decision-0",
				TenantId:           "tenant-1",
				Action:             common.AgentAction_AGENT_ACTION_RATION_BRIDGE,
				CreatedAtUnix:      1950,
				PolicySnapshotJson: `{"deposit_id":"dep-1","granted_minor_units":6000}`,
			},
		},
		deposits: map[string][]*ledger.Deposit{
			"tenant-1": {
				{Id: "dep-1", Status: common.DepositStatus_DEPOSIT_STATUS_RECEIVED}, // repaid
			},
		},
	}
	gatewayFake := &fakeGatewayClient{}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 6000, "reason": "bridge it"}},
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "granted"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 1 {
		t.Fatalf("expected the grant to proceed once the prior grant was repaid, got %d gateway calls", len(gatewayFake.rationCalls))
	}
}

func TestRunner_Tick_NoLateDeposits_NoOp(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{}
	gatewayFake := &fakeGatewayClient{}
	provider := &scriptedProvider{finalText: "unused"}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("expected no LLM calls when there are no late deposits, got %d", len(provider.calls))
	}
}

// TestRunner_Tick_GatewayDeclineWithNoErrorRecordedAsDecline reproduces a
// real business-logic/audit-trail bug in
// services/agent/internal/llm/tools.go's handleRationBridgeFunding: it
// tags EVERY nil-error gateway response as AGENT_ACTION_RATION_BRIDGE,
// even when GrantedMinorUnits is 0. The real
// card-issuance/internal/bridge.Service.RationBridgeFunding this calls
// through the gateway has three documented paths that return exactly
// that shape — GrantedMinorUnits: 0, FullyGranted: false, err: nil — a
// below-minimum computed grant (e.g. the agent's own envelope and the
// gateway's own independent, ledger-entry-based velocity check can
// genuinely disagree about whether a tenant is velocity-capped, since
// they consult two different sources of truth: this package's
// ListAgentDecisions-based hasOpenGrantWithinWindow vs. bridge.Service's
// own ListEntries-based scan for unreleased "bridge:"-prefixed HOLDs),
// a ledger-core-rechecked reserve-floor violation, or "deposit already
// received". None of these is a Go error.
//
// This single-tenant, single-tick scenario demonstrates the bug in
// isolation (no race, no multi-tick timing needed): the gateway
// genuinely declines with a real, valid, nil-error response, and the
// recorded decision must say so (AGENT_ACTION_DECLINE), not
// AGENT_ACTION_RATION_BRIDGE — the audit trail's entire point
// (docs/AGENT-DESIGN.md: "not just 'granted $340' but...") is to tell a
// judge or operator what actually happened to the tenant's money, and
// "granted $0" dressed up as a RATION_BRIDGE action defeats that.
func TestRunner_Tick_GatewayDeclineWithNoErrorRecordedAsDecline(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-1",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000,
		deposits: map[string][]*ledger.Deposit{},
	}
	// The gateway/card-issuance's own, independent velocity check
	// declines this request with a clean, nil-error response — plausible
	// any time the gateway's own ledger-entry-based check and this
	// runner's decision-history-based check diverge (e.g. a bridge HOLD
	// applied through a path other than this runner, such as a direct
	// REST/CLI call, or a prior RecordAgentDecision call that itself
	// failed after its own ApplyEntry already committed).
	gatewayFake := &fakeGatewayClient{declineWithNoError: true}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 6000, "reason": "bridge the late deposit"}},
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "no funding available right now"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	if len(gatewayFake.rationCalls) != 1 {
		t.Fatalf("expected exactly 1 RationBridgeFunding call, got %d", len(gatewayFake.rationCalls))
	}

	var ratioBridgeDecision, declineDecision *ledger.RecordAgentDecisionRequest
	for _, d := range ledgerFake.recordedDecisions {
		if d.GetTrigger() != "deposit_late" {
			continue
		}
		switch d.GetAction() {
		case common.AgentAction_AGENT_ACTION_RATION_BRIDGE:
			if ratioBridgeDecision == nil || d.GetPolicySnapshotJson() != "" {
				// Keep the one from the actual tool-call result (has
				// granted_minor_units in its snapshot), not the
				// unconditional pre-check envelope recording
				// processLateDeposit itself makes before the tool call.
				var parsed struct {
					GrantedMinorUnits *int64 `json:"granted_minor_units"`
				}
				if json.Unmarshal([]byte(d.GetPolicySnapshotJson()), &parsed) == nil && parsed.GrantedMinorUnits != nil {
					ratioBridgeDecision = d
				}
			}
		case common.AgentAction_AGENT_ACTION_DECLINE:
			var parsed struct {
				GrantedMinorUnits *int64 `json:"granted_minor_units"`
			}
			if json.Unmarshal([]byte(d.GetPolicySnapshotJson()), &parsed) == nil && parsed.GrantedMinorUnits != nil {
				declineDecision = d
			}
		}
	}

	if ratioBridgeDecision != nil {
		t.Fatalf("a zero-grant, nil-error gateway response was recorded as AGENT_ACTION_RATION_BRIDGE (snapshot=%s) — it moved zero money and must be AGENT_ACTION_DECLINE", ratioBridgeDecision.GetPolicySnapshotJson())
	}
	if declineDecision == nil {
		t.Fatal("expected the zero-grant gateway response to be recorded as AGENT_ACTION_DECLINE with granted_minor_units in its snapshot, found none")
	}
}

// TestRunner_Tick_GatewayDeclineWithNoErrorDoesNotBlockLaterGrant proves
// the real downstream consequence of the bug above, at the Runner level:
// a zero-grant decline, if it were ever wrongly recorded as
// AGENT_ACTION_RATION_BRIDGE (what tools.go did before the fix), would
// permanently velocity-cap the tenant via hasOpenGrantWithinWindow —
// which treats any RATION_BRIDGE-tagged decision whose snapshot has a
// "granted_minor_units" key (present here, as 0, since the map always
// includes it) as "actually executed" (wasActuallyGranted) and then
// fails closed ("can't tell which deposit this grant was against")
// whenever depositIDFromSnapshot can't resolve one — exactly round 6's
// original "one grant, ever, permanently velocity-caps the tenant" bug,
// reached here via a wrong action tag instead of a missing deposit_id.
//
// This runs Tick twice: tick 1 reproduces the zero-grant decline (same
// as the test above) and feeds its own recorded decision back into the
// fake ledger exactly as a real ledger-core would persist it; tick 2
// introduces a brand-new late deposit for the SAME tenant and asserts
// the gateway is actually called — i.e. the correctly-tagged decline
// from tick 1 does not block it.
func TestRunner_Tick_GatewayDeclineWithNoErrorDoesNotBlockLaterGrant(t *testing.T) {
	ledgerFake := &fakeLedgerCoreClient{
		lateDeposits: []*ledger.Deposit{
			{
				Id:             "dep-1",
				TenantId:       "tenant-1",
				ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
				ExpectedAtUnix: 1000,
				Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
			},
		},
		tenants: map[string]*ledger.Tenant{
			"tenant-1": {Id: "tenant-1", Name: "Acme Co", Currency: "USD", ReserveFloorMinorUnits: 1000},
		},
		headroom: 1_000_000,
		deposits: map[string][]*ledger.Deposit{
			"tenant-1": {
				{Id: "dep-1", Status: common.DepositStatus_DEPOSIT_STATUS_LATE},
			},
		},
	}
	gatewayFake := &fakeGatewayClient{declineWithNoError: true}

	provider := &scriptedProvider{
		toolCalls: []scriptedToolCall{
			{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 6000, "reason": "bridge the late deposit"}},
			{name: llm.ToolNotifyTenant, input: map[string]any{"message": "no funding available right now"}},
		},
		finalText: "done",
	}

	runner := NewRunner(ledgerclient.New(ledgerFake), gatewayFake, provider, policy.DefaultConfig(), newTestLogger())
	runner.Now = func() time.Time { return time.Unix(2000, 0) }

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1 returned error: %v", err)
	}
	if len(gatewayFake.rationCalls) != 1 {
		t.Fatalf("tick 1: expected exactly 1 RationBridgeFunding call, got %d", len(gatewayFake.rationCalls))
	}

	// Feed tick 1's own recorded decisions back into the fake ledger's
	// decision history, exactly as a real ledger-core would persist and
	// later return them via ListAgentDecisions.
	for i, d := range ledgerFake.recordedDecisions {
		ledgerFake.decisions = append(ledgerFake.decisions, &ledger.AgentDecision{
			Id:                 fmt.Sprintf("decision-%d", i),
			TenantId:           d.GetTenantId(),
			Trigger:            d.GetTrigger(),
			Action:             d.GetAction(),
			Rationale:          d.GetRationale(),
			PolicySnapshotJson: d.GetPolicySnapshotJson(),
			CreatedAtUnix:      1960, // within the 24h window ending at tick 2's now=2000
		})
	}

	// Tick 2: a brand-new late deposit for the same tenant, switch the
	// gateway back to granting normally, and confirm it's actually
	// called — i.e. tick 1's decline did not velocity-cap this tenant.
	gatewayFake.declineWithNoError = false
	ledgerFake.lateDeposits = []*ledger.Deposit{
		{
			Id:             "dep-2",
			TenantId:       "tenant-1",
			ExpectedAmount: &common.Money{MinorUnits: 10000, Currency: "USD"},
			ExpectedAtUnix: 1500,
			Status:         common.DepositStatus_DEPOSIT_STATUS_LATE,
		},
	}
	provider.toolCalls = []scriptedToolCall{
		{name: llm.ToolRationBridgeFunding, input: map[string]any{"amount_minor_units": 6000, "reason": "bridge the second late deposit"}},
		{name: llm.ToolNotifyTenant, input: map[string]any{"message": "granted"}},
	}

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2 returned error: %v", err)
	}
	if len(gatewayFake.rationCalls) != 2 {
		t.Fatalf("tick 2: expected the second deposit's grant to proceed (tick 1's decline must not velocity-cap this tenant), got %d total gateway calls", len(gatewayFake.rationCalls))
	}
}
