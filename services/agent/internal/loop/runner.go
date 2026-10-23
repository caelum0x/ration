// Package loop ties internal/policy, internal/llm, and the gateway gRPC
// client together into the agent's actual decision cycle: for every late
// deposit, compute the deterministic hard-rule envelope, hand it to the
// LLM (with tools scoped to that single tenant and that envelope), and log
// every step.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	common "github.com/caelum0x/ration/pkg/pb/common"
	gateway "github.com/caelum0x/ration/pkg/pb/gateway"
	ledger "github.com/caelum0x/ration/pkg/pb/ledger"

	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
	"github.com/caelum0x/ration/services/agent/internal/llm"
	"github.com/caelum0x/ration/services/agent/internal/policy"
)

// triggerDepositLate is the trigger string recorded on every decision made
// from a late-deposit scan, per docs/AGENT-DESIGN.md's example triggers.
const triggerDepositLate = "deposit_late"

// decisionHistoryLimit bounds how many recent agent decisions are fetched
// per tenant when computing the velocity cap.
const decisionHistoryLimit = 20

// Runner is the agent's decision loop. Construct one with NewRunner and
// call Tick on a timer from cmd/agent/main.go.
type Runner struct {
	Ledger  *ledgerclient.Client
	Gateway gateway.GatewayClient
	LLM     llm.Provider
	Policy  policy.Config
	Logger  *slog.Logger

	// Now returns the current time. Overridable in tests; defaults to
	// time.Now when nil.
	Now func() time.Time
}

// NewRunner builds a Runner with sane defaults for any unset field.
func NewRunner(ledgerClient *ledgerclient.Client, gatewayClient gateway.GatewayClient, provider llm.Provider, cfg policy.Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		Ledger:  ledgerClient,
		Gateway: gatewayClient,
		LLM:     provider,
		Policy:  cfg,
		Logger:  logger,
		Now:     time.Now,
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Tick runs one full decision cycle: list every currently-late deposit and
// run the decision pipeline for each. Errors processing one tenant are
// logged and do not stop the others from being processed.
func (r *Runner) Tick(ctx context.Context) error {
	now := r.now()

	deposits, err := r.Ledger.ListLateDeposits(ctx, now)
	if err != nil {
		return fmt.Errorf("loop: ListLateDeposits: %w", err)
	}

	r.Logger.Info("tick: scanning late deposits", "count", len(deposits))

	for _, dep := range deposits {
		if err := r.processLateDeposit(ctx, dep, now); err != nil {
			r.Logger.Error("failed to process late deposit", "tenant_id", dep.GetTenantId(), "deposit_id", dep.GetId(), "error", err)
		}
	}

	return nil
}

func (r *Runner) processLateDeposit(ctx context.Context, dep *ledger.Deposit, now time.Time) error {
	logger := r.Logger.With("tenant_id", dep.GetTenantId(), "deposit_id", dep.GetId())

	tenant, err := r.Ledger.GetTenant(ctx, dep.GetTenantId())
	if err != nil {
		return fmt.Errorf("GetTenant: %w", err)
	}

	reserveResp, err := r.Ledger.CheckReserveFloor(ctx, dep.GetTenantId(), 0)
	if err != nil {
		return fmt.Errorf("CheckReserveFloor: %w", err)
	}

	openGrant, err := r.hasOpenGrantWithinWindow(ctx, dep.GetTenantId(), now)
	if err != nil {
		// Fail closed: if we can't determine velocity state, treat it as
		// blocking rather than silently skipping the check.
		logger.Warn("failed to compute velocity state, assuming an open prior grant", "error", err)
		openGrant = true
	}

	expectedAmount := dep.GetExpectedAmount().GetMinorUnits()
	envIn := policy.EnvelopeInput{
		HeadroomMinorUnits:         reserveResp.GetHeadroomMinorUnits(),
		ExpectedDepositMinorUnits:  expectedAmount,
		RequestedMinorUnits:        expectedAmount, // the natural ask is to bridge the full late amount
		PriorGrantOpenWithinWindow: openGrant,
	}
	envelope := policy.ComputeBridgeEnvelope(r.Policy, envIn)

	logger.Info("computed bridge envelope",
		"decision", envelope.Decision,
		"max_grant_minor_units", envelope.MaxGrantMinorUnits,
		"headroom_minor_units", envelope.HeadroomMinorUnits,
		"bridge_cap_minor_units", envelope.BridgeCapMinorUnits,
		"reason", envelope.Reason,
	)

	// Record the hard-rule computation itself, independent of whatever the
	// LLM goes on to do, so the envelope that gated this decision is
	// always in the audit trail even if the LLM call fails outright.
	snapshot, _ := json.Marshal(map[string]any{
		"deposit_id":             dep.GetId(),
		"expected_amount":        expectedAmount,
		"headroom_minor_units":   envelope.HeadroomMinorUnits,
		"bridge_cap_minor_units": envelope.BridgeCapMinorUnits,
		"max_grant_minor_units":  envelope.MaxGrantMinorUnits,
		"decision":               envelope.Decision,
		"prior_grant_open":       openGrant,
	})
	if _, err := r.Ledger.RecordAgentDecision(ctx, dep.GetTenantId(), triggerDepositLate, actionForDecision(envelope.Decision), envelope.Reason, string(snapshot)); err != nil {
		logger.Error("failed to record envelope decision", "error", err)
	}

	toolCtx := llm.ToolContext{
		TenantID:           dep.GetTenantId(),
		Trigger:            triggerDepositLate,
		DepositID:          dep.GetId(),
		MaxGrantMinorUnits: envelope.MaxGrantMinorUnits,
		Gateway:            r.Gateway,
		Ledger:             r.Ledger,
		Logger:             logger,
	}

	promptCtx := llm.PromptContext{
		TenantID:                  tenant.GetId(),
		TenantName:                tenant.GetName(),
		Currency:                  tenant.GetCurrency(),
		DepositID:                 dep.GetId(),
		ExpectedDepositMinorUnits: expectedAmount,
		DepositExpectedAtUnix:     dep.GetExpectedAtUnix(),
		RequestedMinorUnits:       envIn.RequestedMinorUnits,
		HeadroomMinorUnits:        envelope.HeadroomMinorUnits,
		BridgeCapMinorUnits:       envelope.BridgeCapMinorUnits,
		MaxGrantMinorUnits:        envelope.MaxGrantMinorUnits,
		Decision:                  string(envelope.Decision),
		DeclineReason:             envelope.Reason,
	}

	finalText, err := r.LLM.RunConversation(
		ctx,
		llm.SystemPrompt,
		llm.BuildUserMessage(promptCtx),
		llm.BuildToolSchemas(),
		llm.NewHandler(toolCtx),
	)
	if err != nil {
		if errors.Is(err, llm.ErrCircuitOpen) {
			// A tripped breaker (services/agent/internal/llm/breaker.go)
			// must never block or delay the decision the deterministic
			// hard-rule envelope already made — see
			// runFallbackDecision's doc comment. This is the intended,
			// safe behavior, not a failure to report up as a tick
			// error.
			logger.Warn("llm circuit breaker open, executing hard-rule envelope directly", "decision", envelope.Decision)
			r.runFallbackDecision(ctx, toolCtx, envelope, logger)
			return nil
		}
		return fmt.Errorf("LLM conversation: %w", err)
	}

	logger.Info("decision cycle complete", "final_text", finalText)
	return nil
}

// runFallbackDecision executes the already-computed, deterministic
// hard-rule envelope directly — bypassing the LLM conversation
// entirely — by calling the exact same tool handler
// (llm.NewHandler(toolCtx)) the LLM itself would call, with a canned
// reason/message instead of an LLM-authored one.
//
// This is what makes short-circuiting on llm.ErrCircuitOpen safe
// rather than merely fast: per docs/AGENT-DESIGN.md's "LLM decides
// phrasing only" guarantee, envelope.MaxGrantMinorUnits is already the
// safe ceiling (policy.ComputeBridgeEnvelope, computed and recorded
// via RecordAgentDecision before the LLM was ever consulted — see
// above) — the LLM's only real job this tick was choosing how much of
// that ceiling to execute and how to phrase it to the tenant. A tripped
// breaker means that choice can't be made by the LLM right now, so
// this executes the full ceiling deterministically instead of leaving
// safe, already-decided money movement blocked on a stuck provider.
// Every call here goes through the identical tool-handler code path
// (and therefore the identical gateway/ledger-core re-validation and
// RecordAgentDecision audit trail) a real LLM tool call would use.
func (r *Runner) runFallbackDecision(ctx context.Context, toolCtx llm.ToolContext, envelope policy.Envelope, logger *slog.Logger) {
	handler := llm.NewHandler(toolCtx)

	if envelope.Decision != policy.DecisionDeclined && envelope.MaxGrantMinorUnits > 0 {
		input, _ := json.Marshal(map[string]any{
			"amount_minor_units": envelope.MaxGrantMinorUnits,
			"reason":             "circuit breaker fallback: LLM provider unavailable, granting the full hard-rule envelope ceiling deterministically",
		})
		result, isErr := handler(ctx, llm.ToolRationBridgeFunding, input)
		if isErr {
			logger.Error("circuit breaker fallback: ration_bridge_funding call failed", "result", result)
		} else {
			logger.Info("circuit breaker fallback: ration_bridge_funding executed", "result", result)
		}
	}

	message := fmt.Sprintf("Automated decision (LLM temporarily unavailable): %s — %s", envelope.Decision, envelope.Reason)
	notifyInput, _ := json.Marshal(map[string]any{"message": message})
	if result, isErr := handler(ctx, llm.ToolNotifyTenant, notifyInput); isErr {
		logger.Error("circuit breaker fallback: notify_tenant call failed", "result", result)
	}
}

// hasOpenGrantWithinWindow implements the velocity-cap data gathering
// described in docs/AGENT-DESIGN.md: a prior RATION_BRIDGE grant inside
// the rolling window blocks a new grant unless its target deposit has
// since been received.
func (r *Runner) hasOpenGrantWithinWindow(ctx context.Context, tenantID string, now time.Time) (bool, error) {
	decisions, err := r.Ledger.ListAgentDecisions(ctx, tenantID, decisionHistoryLimit)
	if err != nil {
		return false, fmt.Errorf("ListAgentDecisions: %w", err)
	}

	windowStart := now.Add(-time.Duration(r.Policy.VelocityWindowHours) * time.Hour).Unix()

	var depositsCache []*ledger.Deposit

	for _, d := range decisions {
		if d.GetAction() != common.AgentAction_AGENT_ACTION_RATION_BRIDGE {
			continue
		}
		if d.GetCreatedAtUnix() < windowStart {
			continue
		}

		depositID, ok := depositIDFromSnapshot(d.GetPolicySnapshotJson())
		if !ok {
			// Can't tell which deposit this grant was against; fail closed.
			return true, nil
		}

		if depositsCache == nil {
			depositsCache, err = r.Ledger.ListDeposits(ctx, tenantID)
			if err != nil {
				return false, fmt.Errorf("ListDeposits: %w", err)
			}
		}

		repaid := false
		for _, dep := range depositsCache {
			if dep.GetId() == depositID && dep.GetStatus() == common.DepositStatus_DEPOSIT_STATUS_RECEIVED {
				repaid = true
				break
			}
		}
		if !repaid {
			return true, nil
		}
	}

	return false, nil
}

func depositIDFromSnapshot(snapshotJSON string) (string, bool) {
	if snapshotJSON == "" {
		return "", false
	}
	var parsed struct {
		DepositID string `json:"deposit_id"`
	}
	if err := json.Unmarshal([]byte(snapshotJSON), &parsed); err != nil {
		return "", false
	}
	if parsed.DepositID == "" {
		return "", false
	}
	return parsed.DepositID, true
}

func actionForDecision(d policy.Decision) common.AgentAction {
	switch d {
	case policy.DecisionDeclined:
		return common.AgentAction_AGENT_ACTION_DECLINE
	default:
		return common.AgentAction_AGENT_ACTION_RATION_BRIDGE
	}
}
