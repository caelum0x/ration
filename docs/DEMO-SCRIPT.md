# Demo script — for hackathon judges

This is the exact command sequence to see Ration's agentic rationing
decision happen live, end to end, with no Airwallex sandbox credentials
required (everything below runs in `AIRWALLEX_MODE=mock`).

Total time: under 2 minutes of waiting, most of it a single 10-second
agent poll interval.

## 0. Build

```bash
make build
```

Builds the C WAL (`services/wal/libwal.a`), the Rust ledger-core release
binary, and 10 Go binaries into `./bin` (`gateway`, `auth`,
`tenant-registry`, `card-issuance`, `webhook-ingestion`, `agent`, `tui`,
`ration`, `audit-log`, `verify-chain`) — the full post-split service set
(`docs/SCALE-EXPANSION.md` section 1), not just the original 4 from the
first scaffold build.

## 1. Start the stack and seed demo tenants

```bash
scripts/run-demo.sh
```

This starts, as background processes, all **8 services** (confirmed
against the script's own startup banner and `docker-compose.yml`'s
service list — this doc previously undercounted it at 4, the same
staleness class `README.md` had before a 2026-10-29 pass fixed it there):
- **audit-log** (Go) — the append-only, hash-chained audit trail service
  (`AUDIT_LOG_GRPC_ADDR`, default `localhost:7003`), started first so
  ledger-core has a live mirror target from the moment it starts.
- **ledger-core** (Rust, release build) — the authoritative ledger, on
  `LEDGER_CORE_ADDR` (default `localhost:7001`). With `AUDIT_LOG_ADDR` set
  (which `scripts/run-demo.sh` always does), every `ApplyEntry` and
  `RecordAgentDecision` call is mirrored into audit-log's hash chain.
- **auth** (Go) — tenant API-key issuance/verification and operator JWT
  minting, on `AUTH_GRPC_ADDR` (default `localhost:7010`). Every other
  service below mints a short-lived operator credential from this one at
  startup.
- **tenant-registry** (Go) — tenant onboarding (`CreateTenant`/`ListTenants`),
  on `TENANT_REGISTRY_GRPC_ADDR` (default `localhost:7011`) — what
  `ration tenant create`/`tenant list` (step 8 below) actually talks to.
- **card-issuance** (Go, `AIRWALLEX_MODE=mock`) — card issuance, limit
  adjustment, and the bridge-funding rationing math itself, on
  `CARD_ISSUANCE_GRPC_ADDR` (default `localhost:7012`).
- **webhook-ingestion** (Go) — the HMAC-verified `POST /webhooks/airwallex`
  listener, on `WEBHOOK_INGESTION_GRPC_ADDR`/HTTP `:8081`.
- **gateway** (Go) — the external BFF: gRPC + REST/JSON `/v1/*` + the
  judge-facing HTML dashboard, dialing `auth`/`ledger-core`/
  `card-issuance`/`webhook-ingestion` directly (not `tenant-registry` —
  that one's only reached via its own dedicated port, step 8 below), on
  `GATEWAY_GRPC_ADDR` (default `localhost:7002`).
- **agent** — the polling loop that watches for late deposits and runs the
  rationing decision pipeline (default poll interval: 10s).

If this box is under load from other concurrent processes, any one of the
services above can fail to start with a `DeadlineExceeded` minting its
startup operator JWT against `auth`. If that happens, use
`scripts/start-individually.sh` instead — identical result, longer delays
between each service's startup.

It waits for ledger-core and gateway to actually be accepting TCP
connections on their gRPC ports before moving on (not a fixed sleep), then
runs `./bin/ration seed`, which:
- creates 3 demo tenants (small/medium/large platforms, 3 different
  settlement currencies), and
- **funds each one with settled opening capital** above its reserve
  floor (e.g. the small tenant: $2,500 reserve floor, $7,500 opening
  capital → $5,000 of real headroom) — so the very first late-deposit
  scenario below has real money to ration against, not just a guaranteed
  decline.

You'll see output like:

```
  Lumen Creator Payouts            id=<tenant-id> currency=USD reserve_floor=2,500.00 USD
    funded with 7,500.00 USD of settled opening capital (headroom 5,000.00 USD above reserve floor)
```

**Copy one of the printed tenant IDs** — you'll need it for the next step.

## 2. Open the TUI (second terminal)

```bash
./bin/tui
```

Watch the **activity feed** tab — this is where the deposit and agent
decision events land live, pushed over `gateway.proto`'s `StreamActivity`
gRPC stream. Also check the **tenants** tab to see the funded balance from
step 1.

## 3. Trigger a late deposit

Back in the first terminal:

```bash
./bin/ration simulate-deposit --tenant <tenant-id> --amount 400000 --late
```

(`400000` minor units = $4,000 for a USD tenant — minor units are always
cents/pence/etc., see `pkg/money`.) `--late` backdates the deposit's
expected-arrival time by default so the agent's `ListLateDeposits` scan
picks it up on its very next poll, instead of waiting for a real SLA
window to pass.

## 4. Watch for the decision

Within one poll interval (10s by default — `RATION_AGENT_POLL_INTERVAL_SECONDS`
to change it), two things happen:

1. **In the TUI's activity feed**: a `deposit` event for the simulated
   deposit, and — once the agent's policy envelope computes — a
   corresponding activity entry.
2. **Via the CLI**:
   ```bash
   ./bin/ration status --tenant <tenant-id>
   ```
   Look at **Recent agent decisions**. You should see something like:
   ```
   WHEN                       TRIGGER       ACTION                      RATIONALE
   2026-10-19T02:59:02+03:00  deposit_late  AGENT_ACTION_RATION_BRIDGE  partial grant: capped to 240000 minor units by
                                                                         reserve-floor headroom (500000) and the 60%
                                                                         bridge cap (240000) against a 400000 request
   ```
   That rationale is not a template string — it's the literal output of
   `services/agent/internal/policy/rules.go`'s envelope computation
   (`policy.ComputeBridgeEnvelope`), run against this tenant's actual
   headroom and this deposit's actual amount, and persisted via
   `RecordAgentDecision` **before** the LLM is even called (see
   `docs/AGENT-DESIGN.md`'s audit-trail section).

## 5. What to watch for if `ANTHROPIC_API_KEY` is unset

The agent logs a clear warning on startup (`ANTHROPIC_API_KEY is not set;
LLM calls will fail until it is configured`) and keeps running. Every tick
still:
- computes the deterministic hard-rule envelope (reserve floor, 60% bridge
  cap, 24h velocity cap — see `docs/AGENT-DESIGN.md`),
- **records it as an auditable `AgentDecision`** via `RecordAgentDecision`,
  then
- attempts the LLM call for tenant-facing phrasing / spend-control
  tightening, which fails with a logged `ERROR` (`LLM conversation:
  llm: ANTHROPIC_API_KEY is not set`) — never silently.

This is the fail-safe property the architecture is built around: the
hard-rule envelope is money-safety-critical and runs independently of the
LLM; the LLM only ever adds judgment on top of an already-safe,
already-logged decision. You can verify this live by running the sequence
above without setting the key — the `ration status` decision table still
populates.

## 6. Watch the velocity cap kick in (optional)

Run `ration status` again a few poll cycles later (the same late deposit is
still outstanding, so the agent keeps re-evaluating it on every tick). You
should see a *second* decision for the same tenant:

```
AGENT_ACTION_DECLINE   velocity cap: a prior bridge grant within the last 24h has not yet been repaid
```

This is `docs/AGENT-DESIGN.md` rule 3 — "max 1 bridge grant per tenant per
rolling 24h unless the prior grant was fully repaid" — enforced exactly as
documented.

## 7. Verify the audit trail independently

Every ledger write and agent decision above was mirrored into audit-log's
append-only, hash-chained log as it happened. Verify that chain is intact
*without trusting the live audit-log process at all* — the standalone
verifier re-derives the hash chain straight from the log file on disk:

```bash
./bin/verify-chain ./data/audit.log
```

Expected output:

```
verify-chain: intact (<N> records checked) — ./data/audit.log
```

`<N>` should be at least one record per `ApplyEntry`/`RecordAgentDecision`
call so far (the opening-capital funding from step 1, the simulated
deposit from step 3, and every agent decision from steps 4/6). A nonzero
exit code and a `CHAIN BROKEN at sequence ...` message would mean the log
was tampered with or corrupted after the fact — this is what makes "the
audit trail is the point" (README, `docs/SUBMISSION.md`) a checkable claim
instead of an assertion.

## 8. Operator surface: onboard a tenant, issue a card, mint a credential

The steps above exercise the agent's own decision loop. Separately, `ration`
has 3 operator-facing command groups added across later rounds of this
build (`tenant`, `card`, `apikey`) that a judge can drive directly — none
of these mutate anything the agent's own demo flow above depends on, so
they're safe to run before or after it:

```bash
./bin/ration tenant create --name "Acme Platform Payouts" --currency USD --reserve-floor-minor-units 500000
./bin/ration tenant list
./bin/ration card issue --tenant <tenant-id> --cardholder "Jane Operator" --limit 100000
./bin/ration status --tenant <tenant-id>          # the new card shows up, Held balance reflects its limit
./bin/ration apikey issue --tenant <tenant-id>     # mints a real credential for sdk/go/ration or sdk/rust/ration-client
```

`ration tenant create`/`card issue` both go through real gRPC round trips
(`tenant-registry`/`card-issuance` respectively, fronted by `gateway` for
the card path) — the new tenant and card are both independently visible
via `ration status`, not a local echo.

## 9. Shut everything down

```bash
scripts/stop-demo.sh
```

Kills every PID recorded in `.ration-demo.pid` from step 1 and removes the
file. Safe to run even if some processes already exited.

## Troubleshooting

- **"missing binary ... run 'make build' first"**: you skipped step 0.
- **A port is already in use**: an earlier demo run's processes weren't
  stopped. Run `scripts/stop-demo.sh`, or `lsof -i :7001` / `:7002` to find
  and kill stragglers manually, then retry.
- **`ration status` shows 0 decisions after waiting**: confirm the agent
  process is actually running — check `data/logs/agent.log` for a `tick:
  scanning late deposits` line with `count` > 0. If `count` is 0, the
  deposit wasn't recorded as late; re-run step 3 with `--late`.
