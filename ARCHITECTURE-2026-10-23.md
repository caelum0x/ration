# Ration — Architecture (2026-10-23)

Scope note: `PLAN-2026-10-23-DEEP.md` does not exist in this repo as of this
write — no duplicate-plan collision. This doc covers competitive-positioning-
adjacent technical architecture only: real system shape, real data flow,
real risks, real next-build priorities. Pages/APIs/feature backlog is out of
scope here by the task split, not because it doesn't matter.

## 0. Hosting target (confirmed, not assumed)

**There is no live deployment.** This is a pre-submission hackathon project
(Airwallex Agentic Banking Hackathon, Starter Kit 5). Checked directly:

- `infra/terraform/envs/{dev,staging,prod}` targets **AWS** (ECS services,
  ECR, VPC module, `aws_service_discovery_private_dns_namespace`) — but
  `.terraform/modules/modules.json` shows modules resolved, **no
  `terraform.tfstate`, no backend configured** (backend bootstrap is a
  `TODO(owner)` comment in `versions.tf`). Nothing has been `apply`'d.
- `docker-compose.yml` is the only thing that actually runs today — full
  local dev stack (`docker compose up --build`), explicitly documented as
  wiring services "exactly like `infra/terraform/envs/dev` wires them in
  AWS," i.e. compose is a local stand-in for infra that doesn't exist yet.
- None of this portfolio's real hosting (Oracle boxes `box`/`railyard-box`/
  `recoder-micro`, or Cloudflare Workers/Pages) is wired in anywhere in this
  repo — not in compose, not in Terraform, not in any Dockerfile.
- **Conflict worth flagging:** the only IaC written targets AWS, and the
  owner is banned from AWS. If this ever needs a real deployment (demo
  video capture, judge-accessible sandbox) it must go on Oracle or
  Cloudflare instead, which means the Terraform in this repo is currently
  unusable as written, not just unapplied.

## 1. System diagram (real components only)

```mermaid
flowchart TB
    subgraph Operator["Operator tools (interactive, docker compose profile=tools)"]
        CLI["services/cli (Go)\nseed tenants, simulate-deposit"]
        TUI["services/tui (Go, Bubble Tea)\nlive dashboard"]
    end

    subgraph Core["Always-on services (docker-compose.yml)"]
        GW["services/gateway (Go)\nthin BFF, gRPC fan-out\nproto/gateway.proto"]
        AUTH["services/auth (Go)\nAPI-key verify, JWT mint"]
        TREG["services/tenant-registry (Go)\ntenant CRUD, proxies ledger-core"]
        CARD["services/card-issuance (Go)\nAirwallex Issuing client\n+ bridge-funding rationing math"]
        WEBHOOK["services/webhook-ingestion (Go)\nAirwallex webhook receiver,\ndeposit matching"]
        AGENT["services/agent (Go)\npolicy engine + LLM tool-calling loop"]
        LEDGER["services/ledger-core (Rust)\ndouble-entry ledger,\nreserve-floor invariant\ngRPC server (proto/ledger.proto)"]
        AUDIT["services/audit-log (Go)\nstanding audit-chain mirror"]
        WAL["services/wal (C, linked via FFI)\ncrash-safe append log + recovery"]
    end

    subgraph External["Third-party / external"]
        AW["Airwallex Issuing / Global\nAccounts / Webhooks API\n(mock in-process fake by default;\nAIRWALLEX_MODE=live swaps\nin real sandbox creds)"]
        ANTH["Anthropic Messages API\n(api.anthropic.com/v1/messages)\nor optional Railyard Relay\n(RATION_LLM_GATEWAY_URL,\nOpenAI-compatible, undeployed)"]
    end

    subgraph Obs["Observability (fail-open, optional)"]
        OTEL["otel-collector (OTLP/gRPC traces)"]
        JAEGER["Jaeger"]
        PROM["Prometheus (scrapes /metrics)"]
        GRAF["Grafana"]
    end

    CLI -->|gRPC| GW
    TUI -->|gRPC StreamActivity| GW

    GW -->|gRPC| AUTH
    GW -->|gRPC| TREG
    GW -->|gRPC IssueCard/RationBridgeFunding| CARD
    GW -->|gRPC SimulateDeposit| WEBHOOK
    GW -.->|configured but inert,\nsee docs/notes below| AUDIT

    AGENT -->|gRPC ListLateDeposits,\nGetTenant, CheckReserveFloor| LEDGER
    AGENT -->|gRPC RationBridgeFunding| GW
    AGENT -->|HTTPS Messages API| ANTH

    CARD -->|Issuing/Global Accounts calls| AW
    CARD -->|gRPC ApplyEntry| LEDGER
    WEBHOOK -->|webhook receive| AW
    WEBHOOK -->|gRPC MarkDepositReceived| LEDGER
    TREG -->|gRPC CreateTenant/GetTenant| LEDGER

    LEDGER -->|FFI append/fsync| WAL
    LEDGER -->|mirror every credited entry\n(audit_mirror.rs)| AUDIT

    GW -.OTLP.-> OTEL
    AUTH -.OTLP.-> OTEL
    TREG -.OTLP.-> OTEL
    CARD -.OTLP.-> OTEL
    WEBHOOK -.OTLP.-> OTEL
    AGENT -.OTLP.-> OTEL
    LEDGER -.OTLP.-> OTEL
    OTEL --> JAEGER
    PROM -->|scrape /metrics| GW
    PROM -->|scrape /metrics| AUTH
    PROM -->|scrape /metrics| CARD
    GRAF --> PROM
    GRAF --> JAEGER
```

Notes grounded in code, not inferred:
- `gateway`'s `AUDIT_LOG_ADDR` is set in `docker-compose.yml` but is **inert**
  per the compose file's own header comment and `infra/terraform/README.md`:
  `ApplyEntry`/`RecordAgentDecision` are ledger-core RPCs, so ledger-core's
  own `audit_mirror.rs` already mirrors every entry centrally — gateway
  calling audit-log directly would double-mirror. Shown dashed above.
- mTLS (`pkg/mtls`, `infra/dev-ca/`) exists in code but is **off by default**
  in `docker-compose.yml` (every `*_MTLS_ENABLED` var unset). Not drawn as
  active.
- `services/wal` is C, linked into `ledger-core` (Rust) via FFI
  (`services/ledger-core/src/wal_ffi.rs`), not a separate network service.

## 2. Data flow — 3 most important user actions

### 2a. Tenant deposit arrives late → agent grants/declines bridge funding
(the actual point of the submission; traced through `services/agent/internal/loop/runner.go`)

1. `webhook-ingestion` receives an Airwallex deposit-notification webhook
   (or `cli simulate-deposit --late` forces the scenario) → calls ledger-core
   `MarkDepositReceived` over gRPC.
2. `ledger-core` credits the ledger in a **single WAL frame**
   (idempotent mark-and-credit — this exact bug, deposit marked received but
   never credited, was found and fixed per the README's integration notes)
   → `wal.c` appends + fsyncs → `audit_mirror.rs` mirrors the entry to
   `audit-log`.
3. `agent`'s `Runner.Tick` (timer-driven loop) calls ledger-core
   `ListLateDeposits` → for each late deposit, `GetTenant` +
   `CheckReserveFloor` (deterministic hard-rule envelope: reserve-floor
   headroom, 24h velocity cap from `RecordAgentDecision` history, limit
   `decisionHistoryLimit = 20`).
4. The envelope + tenant context is handed to `services/agent/internal/llm`
   — a tool-calling loop against the Anthropic Messages API
   (`api.anthropic.com/v1/messages`, model `claude-sonnet-5` by default; an
   unconfirmed, undeployed Railyard Relay path exists behind
   `RATION_LLM_GATEWAY_URL` for future metering).
5. The LLM can only call tools scoped to that tenant and that envelope — it
   cannot exceed the reserve floor or the velocity cap; those are enforced
   as Go/Rust code, not LLM judgment.
6. Agent calls `gateway.RationBridgeFunding` → gateway fans out to
   `card-issuance` → `card-issuance` calls ledger-core `ApplyEntry` to move
   real ledger balance, and (in live mode) the Airwallex Issuing API to
   adjust the card.
7. Every decision — grant, partial grant, or decline — is recorded via
   ledger-core `RecordAgentDecision`, which becomes the velocity-cap input
   for the next deposit and the audit trail (`policy_snapshot_json`).

### 2b. New tenant onboarding → card issuance
1. Operator (`cli seed` or a real onboarding call) → `gateway` →
   `tenant-registry.CreateTenant` → `tenant-registry` calls ledger-core
   `CreateTenant` (ledger-core is the system of record; tenant-registry is
   a thin proxy, not a separate store).
2. `gateway.IssueCard` → `card-issuance` → Airwallex Issuing API
   (`services/card-issuance/internal/airwallex/{live,mock}.go`, selected by
   `AIRWALLEX_MODE`) → card record returned, no ledger entry yet (balance is
   zero until a deposit lands).

### 2c. Judge/operator watches it live
1. `tui` opens a gRPC stream to `gateway.StreamActivity`.
2. `gateway`'s `internal/activity` package fans every mutating RPC result
   (card issuance, bridge decisions, deposit events) into that stream —
   this is the only consumer-facing "UI" in the system; there is no web
   dashboard.

## 3. Real architectural risks / bottlenecks (current scale, zero budget)

- **Single-writer ledger is also the single point of failure.** Every
  money-moving path (`ApplyEntry`, `MarkDepositReceived`,
  `RecordAgentDecision`) goes through one `ledger-core` process backed by
  one WAL file on one volume. There's no replica, no failover — fine for a
  hackathon demo, a real outage risk the moment this takes real tenants.
- **No deployed environment at all.** Everything above is validated only
  via `docker compose up` on a laptop. There is zero evidence (no state
  file, no CI deploy step) that the Terraform in `infra/terraform/` has
  ever produced a running resource. A judge or real pilot tenant cannot
  reach this system today.
- **Terraform targets a cloud the owner cannot use.** The only IaC written
  is AWS-shaped (ECS, ECR, VPC). Given the standing AWS ban, this code is
  dead weight until it's either ported to Oracle/Cloudflare or explicitly
  scoped as "reference IaC, never applied."
- **LLM call sits on the critical money path with only a documented, not
  implemented, budget control.** `RATION_LLM_GATEWAY_URL`/Railyard metering
  is a TODO, not wired. At zero budget, an unbounded Anthropic API loop
  (`defaultMaxToolIterations = 8` per decision, times every late deposit,
  times every tenant) is the single biggest cost risk in the whole stack —
  there is currently no spend cap, no circuit breaker, no cached/replayed
  decisions for identical envelopes.
- **mTLS and audit double-mirroring are both "exists in code, off/inert in
  the only running config."** Two production-readiness claims in
  README/docs (`docs/THREAT-MODEL.md` mTLS story, audit-log's standing
  role) are not actually exercised by `docker-compose.yml` today, so they
  are unverified in practice, not just unverified in prod.
- **Nine-service gRPC mesh for a project with zero real traffic.** The
  Phase-2 split (`docs/SCALE-EXPANSION.md` section 1) already happened in
  code (auth/tenant-registry/card-issuance/webhook-ingestion all exist as
  separate Go modules) before there is a single real user generating load
  that would justify independent scaling. This adds real operational
  surface (9 services' worth of `docker-compose.yml` wiring, 9 sets of env
  vars, `AUTH_JWT_SIGNING_KEY` with no default) for a system nobody outside
  this machine can reach yet.

## 4. Prioritized next-build list (effort vs. impact)

1. **(Lowest effort, unlocks the growth-move demo video)** Re-cut
   `docs/DEMO-SCRIPT.md`'s existing sequence
   (`ration simulate-deposit --late` → TUI grant → TUI decline on velocity
   cap) into the first 30 seconds of the submission video. Zero code
   changes — this is pure editing against a sequence that already works
   end to end locally. Directly implements the strategy doc's top
   growth move.
2. **(Low effort, closes the single biggest real risk)** Add a hard LLM
   spend cap in `services/agent/internal/llm` (max calls per tick, max
   tokens per tenant per day) before any live demo or sandbox run — this
   is a few-hour Go change, not a new service, and removes the one
   unbounded-cost path in the whole system.
3. **(Medium effort)** File the Airwallex sandbox access request today per
   `docs/AIRWALLEX-SETUP.md`'s flagged 1-2 day approval lag — this is a
   zero-engineering, owner-facing action item with a hard Nov 13 deadline
   bearing on it; put it in the plan file as an owner-only step, do not
   wait on it before other work.
4. **(Medium effort)** Stand up one real, reachable environment — Oracle
   box (not AWS, given the ban; not Cloudflare, since this is a gRPC mesh
   with a Rust binary + FFI'd C lib, not Workers-shaped) running
   `docker-compose.yml` as-is, exposed behind a reverse proxy. This single
   change converts "judges read a README" into "judges can hit a live
   gRPC/TUI endpoint," which is the only way the differentiation claims
   (WAL crash-kill tests, TLA+ proof, 29 passing tests) become *checkable*
   by someone other than the submitter, not just asserted.
5. **(Higher effort, defer past submission)** Either delete/clearly-label
   the AWS Terraform as non-functional reference material, or port the
   `modules/service` abstraction to Oracle (systemd units / docker on the
   existing boxes) so "infra-as-code" in the README stops pointing at a
   cloud this portfolio cannot deploy to. Not urgent for the Nov 13
   deadline; urgent before anyone treats `infra/terraform/envs/prod` as a
   real deploy target.
