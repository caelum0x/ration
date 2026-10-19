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
}

func (f *fakeGatewayClient) RationBridgeFunding(ctx context.Context, in *gateway.RationBridgeFundingRequest, opts ...grpc.CallOption) (*gateway.RationBridgeFundingResponse, error) {
	f.rationCalls = append(f.rationCalls, in)
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
				Id:                 "decision-0",
				TenantId:           "tenant-1",
				Action:             common.AgentAction_AGENT_ACTION_RATION_BRIDGE,
				CreatedAtUnix:      1950, // within the 24h window ending at now=2000
				PolicySnapshotJson: `{"deposit_id":"dep-1"}`,
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
				PolicySnapshotJson: `{"deposit_id":"dep-1"}`,
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
