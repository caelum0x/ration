package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	common "github.com/caelum0x/ration/pkg/pb/common"
	gateway "github.com/caelum0x/ration/pkg/pb/gateway"

	"github.com/caelum0x/ration/services/agent/internal/ledgerclient"
)

// Tool names. These must match exactly what's passed to the model in
// BuildToolSchemas and what the handler built by NewHandler switches on.
const (
	ToolRationBridgeFunding = "ration_bridge_funding"
	ToolIssueCard           = "issue_card"
	ToolAdjustCardLimit     = "adjust_card_limit"
	ToolNotifyTenant        = "notify_tenant"
)

// BuildToolSchemas returns the tool definitions passed to the Anthropic
// Messages API on every call. tenant_id is deliberately not a parameter on
// any of these — the handler always acts on the single tenant the current
// decision cycle is for, so the model can never redirect an action at
// another tenant.
func BuildToolSchemas() []Tool {
	return []Tool{
		{
			Name:        ToolRationBridgeFunding,
			Description: "Grant bridge funding to the current tenant against their late deposit, up to the pre-computed envelope maximum stated in the user message. The gateway independently re-validates the amount against the same hard rules.",
			InputSchema: mustSchema(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"amount_minor_units": map[string]any{
						"type":        "integer",
						"description": "Amount to grant, in minor currency units. Must not exceed the stated envelope maximum.",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Short internal reason for the grant amount chosen (for the audit log).",
					},
				},
				"required":             []string{"amount_minor_units", "reason"},
				"additionalProperties": false,
			}),
		},
		{
			Name:        ToolIssueCard,
			Description: "Issue a new card for the current tenant.",
			InputSchema: mustSchema(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cardholder_name": map[string]any{"type": "string"},
					"limit_minor_units": map[string]any{
						"type":        "integer",
						"description": "Spend limit for the card, in minor currency units.",
					},
					"limit_interval": map[string]any{
						"type": "string",
						"enum": []string{"daily", "monthly"},
					},
					"currency": map[string]any{"type": "string"},
				},
				"required":             []string{"cardholder_name", "limit_minor_units", "limit_interval", "currency"},
				"additionalProperties": false,
			}),
		},
		{
			Name:        ToolAdjustCardLimit,
			Description: "Tighten or adjust an existing card's spend limit, e.g. to narrow spend to essential categories alongside a partial bridge grant.",
			InputSchema: mustSchema(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"card_id": map[string]any{"type": "string"},
					"new_limit_minor_units": map[string]any{
						"type":        "integer",
						"description": "New spend limit, in minor currency units.",
					},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"card_id", "new_limit_minor_units", "reason"},
				"additionalProperties": false,
			}),
		},
		{
			Name:        ToolNotifyTenant,
			Description: "Deliver a tenant-facing explanation of the decision. Call this exactly once per decision cycle, after any funding/card tool calls.",
			InputSchema: mustSchema(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "The explanation to deliver to the tenant.",
					},
					"escalate_risk_review": map[string]any{
						"type":        "boolean",
						"description": "Set true if this decision pattern (e.g. rapid sequential card requests) looks worth a human risk review.",
					},
				},
				"required":             []string{"message"},
				"additionalProperties": false,
			}),
		},
	}
}

func mustSchema(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// These schemas are static and constructed above; a marshal
		// failure here is a programmer error, not a runtime condition.
		panic(fmt.Sprintf("llm: failed to marshal static tool schema: %v", err))
	}
	return b
}

// ToolContext carries everything a tool handler needs for one decision
// cycle: which tenant it's acting on, the already-computed hard-rule
// envelope, and the clients it's allowed to call.
type ToolContext struct {
	TenantID  string
	Trigger   string // e.g. "deposit_late"
	DepositID string

	// MaxGrantMinorUnits is the hard ceiling from policy.Envelope. The
	// ration_bridge_funding handler re-validates every request against
	// this as a second, agent-side defense-in-depth check — the gateway
	// re-validates again independently, and ledger-core again after that.
	MaxGrantMinorUnits int64

	Gateway gateway.GatewayClient
	Ledger  *ledgerclient.Client
	Logger  *slog.Logger
}

type rationBridgeFundingInput struct {
	AmountMinorUnits int64  `json:"amount_minor_units"`
	Reason           string `json:"reason"`
}

type issueCardInput struct {
	CardholderName  string `json:"cardholder_name"`
	LimitMinorUnits int64  `json:"limit_minor_units"`
	LimitInterval   string `json:"limit_interval"`
	Currency        string `json:"currency"`
}

type adjustCardLimitInput struct {
	CardID             string `json:"card_id"`
	NewLimitMinorUnits int64  `json:"new_limit_minor_units"`
	Reason             string `json:"reason"`
}

type notifyTenantInput struct {
	Message            string `json:"message"`
	EscalateRiskReview bool   `json:"escalate_risk_review"`
}

// NewHandler builds a ToolHandler bound to tc. Every call it makes,
// successful or not, is logged to ledger-core via RecordAgentDecision
// before returning — no tool call is ever silent.
func NewHandler(tc ToolContext) ToolHandler {
	return func(ctx context.Context, toolName string, input json.RawMessage) (string, bool) {
		switch toolName {
		case ToolRationBridgeFunding:
			return tc.handleRationBridgeFunding(ctx, input)
		case ToolIssueCard:
			return tc.handleIssueCard(ctx, input)
		case ToolAdjustCardLimit:
			return tc.handleAdjustCardLimit(ctx, input)
		case ToolNotifyTenant:
			return tc.handleNotifyTenant(ctx, input)
		default:
			return fmt.Sprintf("unknown tool %q", toolName), true
		}
	}
}

func (tc ToolContext) record(ctx context.Context, action common.AgentAction, rationale string, snapshot any) {
	snapshotJSON := "{}"
	if b, err := json.Marshal(snapshot); err == nil {
		snapshotJSON = string(b)
	}
	if _, err := tc.Ledger.RecordAgentDecision(ctx, tc.TenantID, tc.Trigger, action, rationale, snapshotJSON); err != nil {
		tc.logger().Error("failed to record agent decision", "tenant_id", tc.TenantID, "action", action.String(), "error", err)
	}
}

func (tc ToolContext) logger() *slog.Logger {
	if tc.Logger != nil {
		return tc.Logger
	}
	return slog.Default()
}

func (tc ToolContext) handleRationBridgeFunding(ctx context.Context, raw json.RawMessage) (string, bool) {
	var in rationBridgeFundingInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	snapshot := map[string]any{
		"deposit_id":            tc.DepositID,
		"requested_amount":      in.AmountMinorUnits,
		"max_grant_minor_units": tc.MaxGrantMinorUnits,
	}

	if in.AmountMinorUnits <= 0 || in.AmountMinorUnits > tc.MaxGrantMinorUnits {
		reason := fmt.Sprintf("requested amount %d minor units is outside the approved envelope (max %d); agent-side pre-check rejected the call", in.AmountMinorUnits, tc.MaxGrantMinorUnits)
		tc.record(ctx, common.AgentAction_AGENT_ACTION_DECLINE, reason, snapshot)
		return reason, true
	}

	resp, err := tc.Gateway.RationBridgeFunding(ctx, &gateway.RationBridgeFundingRequest{
		TenantId:            tc.TenantID,
		RequestedMinorUnits: in.AmountMinorUnits,
		Reason:              in.Reason,
	})
	if err != nil {
		reason := fmt.Sprintf("gateway RationBridgeFunding failed: %v", err)
		tc.record(ctx, common.AgentAction_AGENT_ACTION_DECLINE, reason, snapshot)
		return reason, true
	}

	snapshot["granted_minor_units"] = resp.GetGrantedMinorUnits()
	snapshot["fully_granted"] = resp.GetFullyGranted()
	rationale := fmt.Sprintf("granted %d of %d requested: %s", resp.GetGrantedMinorUnits(), in.AmountMinorUnits, resp.GetExplanation())

	// The gateway can return a perfectly valid, nil-error decline —
	// GrantedMinorUnits: 0, FullyGranted: false — on any of
	// bridge.Service.RationBridgeFunding's three zero-grant paths: below
	// the $5 minimum-useful-grant floor (including its own, independent,
	// ledger-entry-based velocity check disagreeing with this agent's
	// decision-history-based hasOpenGrantWithinWindow — they consult two
	// different sources of truth and can genuinely diverge), a
	// ledger-core-rechecked reserve-floor violation, or "deposit already
	// received" (round 27). Tagging every nil-error response as
	// RATION_BRIDGE regardless of amount corrupted the audit trail (a
	// decision that moved zero money, labeled as if it granted one) and
	// — far more seriously — fed hasOpenGrantWithinWindow's
	// wasActuallyGranted check a false "actually executed" signal purely
	// because this snapshot map always carries a granted_minor_units key
	// (even at 0, since it's always set above): that function then fails
	// closed on an unresolvable-looking grant and permanently
	// velocity-caps a tenant who was never actually granted anything —
	// the same fund-stranding-via-misclassification failure mode round
	// 6 fixed via a missing deposit_id, reached here instead via a wrong
	// action tag. Reproduced and fixed live via
	// services/agent/internal/loop/runner_test.go's
	// TestRunner_Tick_GatewayDeclineWithNoErrorRecordedAsDecline and
	// TestRunner_Tick_GatewayDeclineWithNoErrorDoesNotBlockLaterGrant.
	action := common.AgentAction_AGENT_ACTION_RATION_BRIDGE
	if resp.GetGrantedMinorUnits() <= 0 {
		action = common.AgentAction_AGENT_ACTION_DECLINE
	}
	tc.record(ctx, action, rationale, snapshot)

	out, _ := json.Marshal(map[string]any{
		"granted_minor_units": resp.GetGrantedMinorUnits(),
		"fully_granted":       resp.GetFullyGranted(),
		"explanation":         resp.GetExplanation(),
	})
	return string(out), false
}

func (tc ToolContext) handleIssueCard(ctx context.Context, raw json.RawMessage) (string, bool) {
	var in issueCardInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	card, err := tc.Gateway.IssueCard(ctx, &gateway.IssueCardRequest{
		TenantId:        tc.TenantID,
		CardholderName:  in.CardholderName,
		LimitMinorUnits: in.LimitMinorUnits,
		LimitInterval:   in.LimitInterval,
		Currency:        in.Currency,
	})
	snapshot := map[string]any{"input": in}
	if err != nil {
		reason := fmt.Sprintf("gateway IssueCard failed: %v", err)
		tc.record(ctx, common.AgentAction_AGENT_ACTION_DECLINE, reason, snapshot)
		return reason, true
	}

	snapshot["card_id"] = card.GetId()
	tc.record(ctx, common.AgentAction_AGENT_ACTION_ISSUE_CARD, fmt.Sprintf("issued card %s for %s", card.GetId(), in.CardholderName), snapshot)

	out, _ := json.Marshal(map[string]any{
		"card_id":           card.GetId(),
		"airwallex_card_id": card.GetAirwallexCardId(),
		"status":            card.GetStatus().String(),
	})
	return string(out), false
}

func (tc ToolContext) handleAdjustCardLimit(ctx context.Context, raw json.RawMessage) (string, bool) {
	var in adjustCardLimitInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}

	card, err := tc.Gateway.AdjustCardLimit(ctx, &gateway.AdjustCardLimitRequest{
		CardId:             in.CardID,
		NewLimitMinorUnits: in.NewLimitMinorUnits,
		Reason:             in.Reason,
	})
	snapshot := map[string]any{"input": in}
	if err != nil {
		reason := fmt.Sprintf("gateway AdjustCardLimit failed: %v", err)
		tc.record(ctx, common.AgentAction_AGENT_ACTION_DECLINE, reason, snapshot)
		return reason, true
	}

	tc.record(ctx, common.AgentAction_AGENT_ACTION_ADJUST_LIMIT, fmt.Sprintf("adjusted card %s limit to %d: %s", in.CardID, in.NewLimitMinorUnits, in.Reason), snapshot)

	out, _ := json.Marshal(map[string]any{
		"card_id":           card.GetId(),
		"limit_minor_units": card.GetLimitMinorUnits(),
		"status":            card.GetStatus().String(),
	})
	return string(out), false
}

func (tc ToolContext) handleNotifyTenant(ctx context.Context, raw json.RawMessage) (string, bool) {
	var in notifyTenantInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Sprintf("invalid input: %v", err), true
	}
	if in.Message == "" {
		return "message must not be empty", true
	}

	trigger := tc.Trigger
	if in.EscalateRiskReview {
		trigger = "risk_review"
	}

	snapshot := map[string]any{
		"escalate_risk_review": in.EscalateRiskReview,
	}
	if _, err := tc.Ledger.RecordAgentDecision(ctx, tc.TenantID, trigger, common.AgentAction_AGENT_ACTION_NOTIFY, in.Message, toJSON(snapshot)); err != nil {
		tc.logger().Error("failed to record notify_tenant decision", "tenant_id", tc.TenantID, "error", err)
		return fmt.Sprintf("failed to record notification: %v", err), true
	}

	return "notification recorded", false
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
