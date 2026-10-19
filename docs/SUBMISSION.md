# Ration — hackathon submission

**Track:** Airwallex Agentic Banking Hackathon — Starter Kit 5, Platform
Spend Controller (Multi-Tenant Platforms track).

## Problem (Starter Kit 5)

A multi-tenant platform (marketplace, payroll, gig/freight exchange — see
the 3 demo tenants) issues cards to its own customers against money the
platform expects to receive on a schedule (e.g. settlement payouts). When
an expected deposit is late, the customer side still wants to spend, but
the platform can't just let spend run unconstrained against money that
hasn't landed — that risks breaching the platform's own reserve floor
(its own capital, never touchable) if the deposit never shows up at all.

Starter Kit 5 asks for a spend controller that makes this tradeoff safely
and explainably: let the tenant bridge a *safe fraction* of what's coming,
never risk the platform's floor, and make every decision auditable enough
that a human (or a judge) can see exactly why a given amount was granted
or declined.

## Approach

Full design in [docs/ARCHITECTURE.md](ARCHITECTURE.md) (service layout,
money model, crash-safety) and [docs/AGENT-DESIGN.md](AGENT-DESIGN.md)
(the rationing policy itself). Summary:

**Six services, four languages, one invariant that's never delegated to
an LLM:**

- **`services/wal` (C)** — a real crash-safe write-ahead log: length-prefixed,
  CRC32-checksummed frames, `fsync()` after every append, torn-tail
  recovery on restart. This is the durability floor everything else sits
  on.
- **`services/ledger-core` (Rust)** — the authoritative ledger. Owns
  tenants, deposits, balances, and the one function that's allowed to move
  money (`apply_entry`): idempotent on an idempotency key, and for
  DEBIT/HOLD, checks the reserve floor *before* writing to the WAL. Links
  `libwal.a` via FFI, with exactly one `unsafe` module
  (`src/wal_ffi.rs`) wrapping it safely.
- **`services/gateway` (Go)** — owns the Airwallex integration (mock or
  live behind one interface, see below) and re-validates every
  balance-moving request against the same reserve-floor/bridge-cap rules
  *again*, independently of the agent, before proxying to ledger-core.
  Defense in depth: the agent is never trusted as the only gate.
- **`services/agent` (Go)** — the actual agentic loop. Every poll cycle:
  list currently-late deposits, compute a deterministic hard-rule envelope
  per tenant (reserve floor, 60% bridge cap, 24h velocity cap — see
  AGENT-DESIGN.md), **record that envelope as an auditable decision before
  calling any LLM**, then hand the LLM only the already-safe envelope to
  decide phrasing / spend-control tightening / escalation — never whether
  to breach a hard rule. Talks to the Anthropic Messages API directly over
  `net/http` (no SDK dependency), with a real tool-calling loop
  (`ration_bridge_funding`, `issue_card`, `adjust_card_limit`,
  `notify_tenant`).
- **`services/tui` (Go, Bubble Tea)** — live dashboard: tenants, deposits,
  cards, and an activity feed fed by `gateway.proto`'s `StreamActivity`.
- **`services/cli` (Go, cobra)** — the judge-facing demo driver: `ration
  seed` (creates + funds 3 demo tenants), `ration simulate-deposit
  --late` (forces the scenario), `ration status` (shows the resulting
  balance, cards, and agent decisions).

**The audit trail is the point.** Every hard-rule computation *and* every
LLM tool call is written via `RecordAgentDecision` to ledger-core before
any money moves, with a `policy_snapshot_json` capturing the exact inputs
(headroom, deposit amount, velocity state) that produced the decision —
see [docs/DEMO-SCRIPT.md](DEMO-SCRIPT.md) for what that looks like for a
real run, including the case where the LLM call itself fails (missing
`ANTHROPIC_API_KEY`): the hard-rule decision is still computed, still
recorded, and the LLM failure is logged loudly, never swallowed.

**Scale-out posture is written down, not hand-waved**: service
decomposition, observability, infra-as-code, formal verification of the
reserve-floor invariant, and fuzzing/chaos testing are scoped in
[docs/SCALE-EXPANSION.md](SCALE-EXPANSION.md) as the next phase past this
submission.

## What's mocked vs. real

See [docs/AIRWALLEX-SETUP.md](AIRWALLEX-SETUP.md) for the live-credential
status in detail. Short version:

- **Real**: the WAL, the ledger (double-entry-equivalent balance
  tracking, reserve-floor enforcement, idempotency, crash recovery — all
  covered by tests that actually kill the process mid-write and assert
  nothing was lost or double-applied), the policy math, the agent loop's
  tool-calling against the Anthropic API, the gRPC wiring between every
  service, the TUI, the CLI, and defense-in-depth re-validation at the
  gateway layer.
- **Mocked**: `AIRWALLEX_MODE=mock` (the default) runs against
  `services/card-issuance/internal/airwallex/mock.go`, an in-memory fake that
  mirrors the real Issuing API, Global Accounts, and webhook payload
  shapes closely enough that flipping to `AIRWALLEX_MODE=live` with real
  sandbox credentials is a one-line env change, not a rewrite — same code
  path, same interface (`airwallex.Client`), different implementation.
  Sandbox platform access is a per-participant approval from the
  organizers, requested but not yet granted as of this submission (see
  AIRWALLEX-SETUP.md for the exact blocker and status).

## How to run it

```bash
make build
scripts/run-demo.sh      # starts ledger-core + gateway (mock) + agent, seeds + funds 3 tenants
./bin/tui                # second terminal — live activity feed
./bin/ration simulate-deposit --tenant <id> --amount 400000 --late
# wait ~10s for the agent's next poll
./bin/ration status --tenant <id>   # see the recorded decision + rationale
scripts/stop-demo.sh
```

Full walkthrough with exact expected output: [docs/DEMO-SCRIPT.md](DEMO-SCRIPT.md).

## Verification performed for this submission

- `make build` — clean build of the C WAL, Rust ledger-core (release),
  and all 4 Go binaries.
- `go build ./...`, `go vet ./...`, `go test ./...` — clean across every
  Go module in the workspace (`pkg`, `agent`, `cli`, `gateway`, `tui`).
- `cargo build --release && cargo test --release` — 29 passing tests
  (unit + FFI + idempotency + crash-recovery + reserve-floor integration
  suites), 0 failed.
- `make -C services/wal test` — the C WAL's own append/recovery/corruption
  test binary, all passing.
- A real end-to-end run: built everything, ran `scripts/run-demo.sh`,
  seeded + funded 3 tenants, forced a late deposit via `ration
  simulate-deposit --late`, and confirmed `ration status` recorded a real
  `AGENT_ACTION_RATION_BRIDGE` decision with a computed (not templated)
  rationale, followed by a correctly-triggered `AGENT_ACTION_DECLINE` on
  the next poll once the 24h velocity cap kicked in — both without
  `ANTHROPIC_API_KEY` set, proving the hard-rule envelope and its audit
  trail run independently of (and before) the LLM call.

Two real bugs were found and fixed during that end-to-end run (not just
compile-time issues):
1. `services/ledger-core`'s bind address env var didn't match what every
   other service dials (`RATION_LEDGER_ADDR` vs. the `LEDGER_CORE_ADDR`
   every other service, and `.env.example`, agree on) — the whole stack
   would never actually connect together with default config.
2. `MarkDepositReceived` updated a deposit's status but never credited
   the tenant's ledger balance — meaning no tenant could ever accumulate
   headroom, so the bridge-funding policy could only ever decline,
   regardless of how a late deposit was sized. Fixed at the ledger-core
   level (atomic WAL event covering both the status update and the
   credit, so a crash between the two is impossible by construction), and
   covered by new tests (`tests/deposit_received_credits_balance.rs`).
   `ration seed` was also extended to fund each demo tenant with settled
   opening capital through this same path, so the judge-facing demo
   starts with real headroom instead of a guaranteed decline.
