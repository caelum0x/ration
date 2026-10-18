# Agent design — rationing policy

## The problem (Starter Kit 5)

A platform's end customers get issued Airwallex cards against expected
deposits (e.g. marketplace payouts due to arrive on a schedule). Sometimes a
deposit is late. The customer still needs to spend. Ration bridge funding:
let them spend against a safe fraction of what's coming, never breach the
platform's reserve floor, and make the tradeoff auditable.

## Hard rules (deterministic, never delegated to the LLM)

Implemented identically in `services/agent/internal/policy/rules.go` (agent
side, pre-check) and `services/gateway/internal/bridge/service.go` (gateway
side, enforced again — defense in depth, the agent is not trusted as the
only gate) and ultimately re-enforced by `ledger-core::ledger::apply_entry`
(the actual source of truth — even a buggy gateway can't breach it):

1. **Reserve floor**: `tenant.reserve_floor_minor_units` is the platform's
   own capital, never touchable. `headroom = available_balance -
   reserve_floor`. No HOLD or DEBIT may exceed headroom.
2. **Bridge cap**: a single bridge grant is capped at
   `min(headroom, expected_late_deposit_amount * 0.6)` — never bridge more
   than 60% of what's actually coming, so a no-show deposit can't wipe the
   floor.
3. **Velocity cap**: max 1 bridge grant per tenant per rolling 24h unless
   the prior grant was fully repaid (deposit received and ledger entry
   released).
4. **Partial over decline**: if the full requested amount can't be granted
   under rules 1-3, grant the largest amount that satisfies them rather
   than declining outright, unless the computed amount is below a $5
   minimum-useful-grant floor — then decline with a clear reason.

## What the LLM actually decides

Only inside the envelope rules 1-4 already computed:

- Phrasing of the tenant-facing explanation (`notify_tenant` tool).
- Whether to also tighten the card's spend controls (category restrictions,
  per-transaction cap) alongside a partial grant, based on the tenant's
  recent spend pattern in `ListEntries` — e.g. prefer narrowing to
  essential categories over an equal percentage cut across all spend.
- Whether a `card_request` (new card issuance for a cardholder) looks like
  normal onboarding vs. a pattern worth a `risk_review` trigger (rapid
  sequential requests) — it can escalate but never auto-approve past the
  hard rules.

The system prompt (`services/agent/internal/llm/prompt.go`) states the
envelope explicitly per call: "You may grant up to exactly
{max_grant_minor_units}. You may not exceed this under any reasoning." The
tool schema for `ration_bridge_funding` takes `amount_minor_units` and the
gateway independently re-validates it against the same rules before
touching the ledger — a model that ignores the instruction still can't
move money past the floor.

## Audit trail

Every decision — hard-rule computation and LLM tool call — is written via
`RecordAgentDecision` to ledger-core before any money moves, with
`policy_snapshot_json` capturing the exact rule inputs (headroom, deposit
amount, velocity state) that produced the envelope. This is what a judge or
the TUI's activity feed shows: not just "granted $340" but "granted $340 of
$600 requested because headroom was $340 after a $200 reserve floor and a
prior grant 11h ago already used this tenant's 24h velocity slot."
