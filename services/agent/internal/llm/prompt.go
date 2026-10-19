package llm

import (
	"fmt"
	"strings"
)

// SystemPrompt is the static system prompt sent on every call. It states
// the rationing policy in prose and makes explicit that the per-call
// envelope (see PromptContext.MaxGrantMinorUnits) is a hard ceiling the
// model may never exceed — the LLM only decides phrasing, card-control
// tightening, and risk escalation within that ceiling, per
// docs/AGENT-DESIGN.md "What the LLM actually decides".
const SystemPrompt = `You are Ration's rationing agent for a multi-tenant card-issuing platform.

Platform customers ("tenants") get Airwallex cards issued against expected
deposits (e.g. marketplace payouts due on a schedule). When a deposit is
late, a tenant may still need to spend. Your job is to help ration bridge
funding against a safe fraction of what is coming, without ever breaching
the platform's reserve floor, and to make every decision auditable.

A deterministic hard-rule engine — not you — has already computed the exact
envelope for this decision: the reserve floor, the 60% bridge cap, and the
24h velocity cap have all been applied before you were called. You will be
told the exact maximum amount (in minor currency units, e.g. cents) you may
grant for this call. You may not exceed this amount under any reasoning,
and you must never attempt to reinterpret, recompute, or talk your way past
it — if you believe the envelope is wrong, say so in your final response,
but still respect it in every tool call you make.

Within that envelope, you decide:
  - The phrasing of the tenant-facing explanation (always use the
    notify_tenant tool to deliver it — never leave a tenant without a clear
    explanation, even on a decline).
  - Whether to also tighten the card's spend controls (category
    restrictions, per-transaction cap) alongside a partial grant, based on
    the tenant's recent spend pattern — prefer narrowing to essential
    categories over an equal percentage cut across all spend.
  - Whether a new card request looks like normal onboarding versus a
    pattern worth escalating for risk review (e.g. rapid sequential
    requests) — you may escalate via notify_tenant's escalate_risk_review
    flag, but you can never auto-approve past the hard rules.

Prefer a partial grant with a clear, specific explanation over a silent
decline. Always call notify_tenant exactly once to close out the decision,
after any funding/card tool calls. Be concise and factual in tool inputs;
do not invent amounts, card ids, or tenant details that were not given to
you in the user message.`

// PromptContext carries the per-call facts the system prompt and user
// message need. It intentionally uses plain fields instead of pb types so
// this package has no dependency on pkg/pb.
type PromptContext struct {
	TenantID                  string
	TenantName                string
	Currency                  string
	DepositID                 string
	ExpectedDepositMinorUnits int64
	DepositExpectedAtUnix     int64
	RequestedMinorUnits       int64
	HeadroomMinorUnits        int64
	BridgeCapMinorUnits       int64
	MaxGrantMinorUnits        int64
	Decision                  string // policy.Decision as a string, to avoid importing internal/policy here
	DeclineReason             string
}

// BuildUserMessage renders the per-call facts into the user turn sent
// alongside SystemPrompt.
func BuildUserMessage(c PromptContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Tenant %q (id=%s, currency=%s) has a late deposit (id=%s) of %d minor units expected at unix time %d.\n",
		c.TenantName, c.TenantID, c.Currency, c.DepositID, c.ExpectedDepositMinorUnits, c.DepositExpectedAtUnix)
	fmt.Fprintf(&b, "Current reserve-floor headroom: %d minor units. 60%% bridge cap for this deposit: %d minor units.\n",
		c.HeadroomMinorUnits, c.BridgeCapMinorUnits)

	switch c.Decision {
	case "declined":
		fmt.Fprintf(&b, "The hard-rule engine has DECLINED any bridge grant for this tenant right now. Reason: %s.\n", c.DeclineReason)
		b.WriteString("You must NOT call ration_bridge_funding. Call notify_tenant with a clear, specific explanation of why funding is not available right now, based on the reason above.\n")
	case "partial_grant":
		fmt.Fprintf(&b, "The hard-rule engine has APPROVED a PARTIAL grant. You may grant up to exactly %d minor units. You may not exceed this under any reasoning. The tenant originally needs %d minor units.\n",
			c.MaxGrantMinorUnits, c.RequestedMinorUnits)
		b.WriteString("Decide whether to call ration_bridge_funding for up to the approved amount, whether to also tighten the card's spend controls, and then call notify_tenant to explain the partial grant clearly.\n")
	default: // "full_grant"
		fmt.Fprintf(&b, "The hard-rule engine has APPROVED the full requested grant. You may grant up to exactly %d minor units. You may not exceed this under any reasoning.\n",
			c.MaxGrantMinorUnits)
		b.WriteString("Call ration_bridge_funding for the approved amount, then call notify_tenant to confirm the grant to the tenant.\n")
	}

	return b.String()
}
