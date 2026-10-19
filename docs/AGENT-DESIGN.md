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

## LLM transport: direct Anthropic vs. Railyard Relay gateway

`services/agent/internal/llm` can send every model call either directly to
the Anthropic Messages API (the original path) or through Railyard's Relay
gateway (a sibling product, `~/products/railyard/apps/relay` — a maintained
fork of TensorZero with opt-in success-only Mint billing and per-key spend
caps via its Abacus/budget feature). Routing through Relay is what makes
per-call cost metering and a hard monthly/per-key spend cap on the agent's
own LLM usage possible — today every `RunConversation` call goes straight to
Anthropic with no spend visibility or cap of its own; Relay's budgets
feature (`docs` in the Railyard repo) would stop a runaway agent loop with a
`429` before the provider is even called, independently of the hard
rationing rules above (which govern money Ration moves, not Ration's own
LLM bill).

**What's actually wired (`services/agent/internal/llm/provider.go`,
`gateway.go`):**

- `RATION_LLM_GATEWAY_URL` (env var, read in `NewAnthropicProviderFromEnv`):
  when set, `createMessage` dispatches to `createMessageViaGateway`, which
  translates the Anthropic Messages wire shape (content-block arrays,
  `tool_use`/`tool_result` blocks, the top-level `system` field) to and from
  Relay's client-facing OpenAI-compatible `chat.completions` shape and POSTs
  to `<RATION_LLM_GATEWAY_URL>/chat/completions`. Confirmed against
  `apps/relay/README.md`'s quickstart (`POST
  http://localhost:3000/openai/v1/chat/completions`) and
  `apps/relay/examples/relay/openai-client/README.md` — not guessed.
- `RATION_LLM_GATEWAY_MODEL`: the Relay/TensorZero model or function
  selector (e.g. `tensorzero::function_name::chat`), sent as the request's
  `model` field. Falls back to `RATION_AGENT_MODEL` if unset.
- `RATION_LLM_GATEWAY_API_KEY`: sent as `Authorization: Bearer <value>`,
  matching Relay's OpenAI-SDK-style auth (confirmed from
  `apps/relay/docs/gateway/configuration-reference.mdx`).
- Leaving `RATION_LLM_GATEWAY_URL` unset preserves the exact original
  direct-to-Anthropic behavior — `createMessage` falls back to
  `createMessageDirect` with zero change to request/response handling,
  retry/backoff (`sendWithRetry`, shared by both transports), or the
  tool-calling loop in `RunConversation`. The gateway is purely a transport
  swap underneath that loop, not a rewrite of it.
- `go test ./services/agent/...` covers both transports: translation
  (`gateway_test.go`), dispatch/fallback and the retry path
  (`provider_test.go`), and a full tool-call round trip through a fake
  Relay server.

**What's a documented TODO, not silently skipped:**

- There is no confirmed-reachable, owner-provided production Relay URL from
  this environment, so `RATION_LLM_GATEWAY_URL` has no non-empty default —
  an operator must point it at their own running Relay deployment (local
  quickstart or production `apps/relay/config/tensorzero.toml`).
- Relay's budget/metering features (Mint billing, per-key spend caps via
  Abacus) are opt-in server-side configuration on the Relay deployment
  itself (`RELAY_BILLING_ENABLED=1` plus `MINT_*` env vars, per
  `apps/relay/README.md`). Routing the agent through Relay makes enforcing
  a cap *possible*; it does not provision one. No cap is enforced today
  regardless of which transport is used.
- The `RATION_LLM_GATEWAY_MODEL` → Relay model/function mapping is only
  meaningful against whatever `tensorzero.toml` the operator's Relay
  instance actually serves; this package has no way to introspect or
  validate that mapping, so a misconfigured selector fails at request time
  with Relay's own error, not a local check.

## Audit trail

Every decision — hard-rule computation and LLM tool call — is written via
`RecordAgentDecision` to ledger-core before any money moves, with
`policy_snapshot_json` capturing the exact rule inputs (headroom, deposit
amount, velocity state) that produced the envelope. This is what a judge or
the TUI's activity feed shows: not just "granted $340" but "granted $340 of
$600 requested because headroom was $340 after a $200 reserve floor and a
prior grant 11h ago already used this tenant's 24h velocity slot."
