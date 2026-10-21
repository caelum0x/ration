# Threat model

A STRIDE pass over the service boundaries that exist in this repo
today: the phase-1 services (`ledger-core`, `agent`, `tui`, `cli`),
`audit-log` (section 8), and the section 1 decomposition
(`docs/SCALE-EXPANSION.md` section 1, `docs/adr/0004`) that split the
old `gateway` monolith into `auth`, `tenant-registry`, `card-issuance`,
`webhook-ingestion` and a thin `gateway` BFF in front of all four.
Revised as of that split landing — every claim below is checked against
what actually runs today, not a hypothetical future topology.

Same standard as the rest of this project: name the exact file/RPC a
mitigation lives in, and when something isn't mitigated, say so with a
precise TODO instead of a vague "should add auth later."

## What's actually running

```
                    ┌──────────┐        ┌──────────┐
   operator  ──────▶│   cli    │        │   tui    │◀────── operator
   (terminal)        └────┬─────┘        └────┬─────┘
                          │  gRPC               │  gRPC
                          ▼                     ▼
┌─────────────────────────────────────────────────────┐
│                   gateway (Go) :7002 — thin BFF       │
│  gateway.proto: IssueCard, AdjustCardLimit,            │
│  RationBridgeFunding, StreamActivity, ListCards,       │
│  SimulateDeposit — unchanged externally by section 1  │
│  internal/ratelimit sits in front of every gRPC RPC    │
│  on this service's public surface                      │
└───┬──────────┬──────────────┬──────────────┬──────────┘
    │ gRPC     │ gRPC          │ gRPC          │ gRPC (ApplyEntry,
    ▼          ▼               ▼               │ CheckReserveFloor,
┌────────┐ ┌──────────────┐ ┌──────────────┐  │ RecordDeposit, ...)
│  auth  │ │tenant-registry│ │card-issuance │  │
│ :7010  │ │    :7011      │ │   :7012      │  │
│(not yet│ │ (tenant CRUD, │ │ Airwallex    │  │
│ wired  │ │  proxies to   │ │ issuing +    │  │
│ into   │ │  ledger-core) │ │ bridge math, │  │
│ gateway│ └──────┬────────┘ │ the highest- │  │
│  — see │        │ gRPC     │ value RPCs)  │  │
│ gap 1) │        │          └──────┬───────┘  │
└────────┘        │                 │ gRPC      │
                   ▼                 ▼           ▼
┌─────────────────────────────────────────────────────┐
│               ledger-core (Rust) :7001                 │
│  ledger.proto: the ONLY implementation of ApplyEntry   │
│  and RecordAgentDecision in this system — every        │
│  money-moving/decision-recording call, from any Go     │
│  service or cli, ends up here.                         │
└───────────┬─────────────────────────────┬─────────────┘
            │ best-effort, async           │ C FFI (unsafe,
            │ gRPC mirror                  │ in-process, not
            ▼                              │ a network hop)
┌─────────────────────────┐                ▼
│   audit-log (Go) :7003   │        services/wal (libwal.a)
│  append-only, hash-      │        local disk, fsync'd
│  chained AppendRequest   │        append-only frames
│  log — separate trust    │
│  boundary from the WAL   │
└─────────────────────────┘

┌─────────────────────────────────────────────────────┐
│          webhook-ingestion (Go) :7013 / :8081          │◀── Airwallex
│  gRPC: StreamEvents (merged into gateway's own          │    webhooks
│  activity feed via internal/webhookeventsclient);       │    (HTTP, always
│  HTTP :8081 /webhooks/airwallex — always listening,     │    listening,
│  independent of card-issuance's AIRWALLEX_MODE          │    not gated on
│  (signature-verified against AIRWALLEX_WEBHOOK_SECRET,   │    a live mode)
│  calls ledger-core's MarkDepositReceived directly)      │
└───────────────────────┬───────────────────────────────┘
                         │ gRPC (ListDeposits, MarkDepositReceived)
                         ▼
                   ledger-core (above)

   agent (Go) ── gRPC ──▶ ledger-core (ListLateDeposits, RecordAgentDecision)
       │         gRPC ──▶ gateway (RationBridgeFunding, IssueCard)
       └── HTTPS ──▶ Anthropic Messages API (or Railyard Relay — see
                      docs/AGENT-DESIGN.md)

   card-issuance ── HTTPS ──▶ Airwallex Issuing API (AIRWALLEX_MODE=live
                               only; mock mode, the default, never
                               leaves the process)
```

Every internal gRPC hop (cli/tui/agent → gateway, gateway → auth/
tenant-registry/card-issuance/webhook-ingestion/ledger-core,
webhook-ingestion → ledger-core, ledger-core → audit-log) can run mTLS
(`pkg/mtls`, `ledger_core::mtls`, `infra/dev-ca/`) — opt-in, off by
default, on by setting `<PREFIX>_MTLS_ENABLED=true` per service (see
`.env.example`).
`services/wal` is a linked C library inside the `ledger-core` process,
not a network boundary, and is out of scope for this doc (its threat
model is "a malformed frame on disk" — covered by
`services/wal/fuzz/fuzz_frame_parse.c` and `tests/recovery.rs`, not
STRIDE).

## Assets

1. **Tenant ledger state** (balances, holds, reserve floor headroom) —
   `ledger-core`'s SQLite index + WAL. The thing this whole system exists
   to not lose or corrupt.
2. **The reserve floor invariant itself** — `available >= reserve_floor`
   must hold after every `ApplyEntry`. Formally checked in
   `docs/formal/ledger.tla`, property-tested in
   `tests/apply_entry_proptest.rs`.
3. **Agent decisions and their rationale** — `AgentDecisionRecord`, the
   audit trail for "why did the system grant/decline this."
4. **Airwallex credentials** — `AIRWALLEX_API_KEY`/`AIRWALLEX_CLIENT_ID`
   live in `card-issuance` only (it is the one service that calls
   Airwallex's Issuing API; see `services/card-issuance/internal/
   airwallex/live.go`); `AIRWALLEX_WEBHOOK_SECRET` lives in
   `webhook-ingestion` only (the one service that verifies inbound
   webhook deliveries). Neither credential is held by `gateway` anymore
   post section-1 split — never logged in either service.
5. **The Anthropic API key** — agent-only.
6. **The append-only audit trail** itself (`audit-log`'s hash chain) —
   an asset specifically because it is meant to survive the compromise
   of every other asset on this list.
7. **mTLS private keys** (`infra/dev-ca/out/*-key.pem` in dev; a real
   PKI's issued keys in prod) — compromise of one is a stolen identity
   for that one service, not a CA compromise (see Elevation of
   Privilege below).

## Actors / trust boundaries

### 1. The TUI/CLI user (an operator at a terminal)

**What they can do today:** dial `gateway`/`ledger-core` directly
(`services/cli/cmd/ration/clients.go`, `services/tui/internal/client/
grpc.go`) and call any RPC either service exposes, for any
`tenant_id`, with no per-tenant authorization check anywhere in the
call path (see "Known gap: no tenant-scoped authorization" below —
this is the single biggest finding in this document, not a CLI-specific
one).

| STRIDE | Assessment |
|---|---|
| **S**poofing | Nothing distinguishes "the real operator" from "anyone who can reach the gRPC port and, if mTLS is enabled, holds a `CLI_`/`TUI_`-issued cert." With mTLS off (the default), spoofing the CLI is trivial — any client on the network. With mTLS on, an attacker needs the CLI's private key, not just network access. |
| **T**ampering | A CLI/TUI session can call `ApplyEntry`/`RecordAgentDecision`/`IssueCard` for *any* tenant — there is nothing CLI-specific it's restricted to. It cannot, however, tamper with history: `ApplyEntry` only appends (idempotent on `idempotency_key`), and `audit-log`'s chain makes a post-hoc edit of a past record detectable. |
| **R**epudiation | Every `ApplyEntry`/`RecordAgentDecision` call mirrors into `audit-log` (`services/ledger-core/src/audit_mirror.rs`) with `source_service="ledger-core"` — the audit record proves *ledger-core* made the call, not which upstream caller (cli vs. gateway vs. agent) asked it to. A CLI-driven action and a gateway-driven action are indistinguishable in the trail. **Gap, not mitigated**: no per-RPC caller identity is threaded from cli/tui through to the audit record today. |
| **I**nformation disclosure | `ListTenants`/`ListEntries`/`ListAgentDecisions`/`GetBalance` return full detail for whatever `tenant_id` is requested — a CLI session can read every tenant's balance history and every agent decision's `rationale`/`policy_snapshot_json`, not just "its own." |
| **D**enial of service | `internal/ratelimit` throttles gateway's gRPC surface per-tenant (keyed on `tenant_id` in the request, falling back to peer address — see `keyFor` in `services/gateway/internal/ratelimit/limiter.go`), so a runaway CLI script hammering one tenant_id degrades that tenant's own quota, not every tenant's. `ledger-core` itself has no rate limiting in front of it at all (only gateway's public surface does, per `docs/SCALE-EXPANSION.md` section 8's own scope) — a CLI/agent/gateway bug that loops `ApplyEntry` calls can still exhaust ledger-core's single-writer WAL throughput. |
| **E**levation of privilege | There is no privilege to elevate *from* — cli/tui already have the same effective access as gateway/agent. See the gap below. |

### 2. The Airwallex webhook sender (external, HTTP)

Reaches `services/webhook-ingestion`'s own `:8081` HTTP listener
(`services/webhook-ingestion/cmd/webhook-ingestion/main.go`), not
`gateway` — this moved off gateway entirely in the section-1 split
(docs/adr/0004). Unlike the pre-split gateway (which only started its
webhook listener in `AIRWALLEX_MODE=live`), webhook-ingestion's HTTP
listener always starts, independent of card-issuance's own
`AIRWALLEX_MODE` — an unconfigured `AIRWALLEX_WEBHOOK_SECRET` makes
every delivery fail signature verification (fail-closed), rather than
the listener not existing at all.

| STRIDE | Assessment |
|---|---|
| **S**poofing | Mitigated: every delivery is HMAC-SHA256'd over `x-timestamp + body` with `AIRWALLEX_WEBHOOK_SECRET` (`services/webhook-ingestion/internal/airwallex/webhook.go`'s `VerifySignature`), compared with `hmac.Equal` (constant-time). A sender without the shared secret cannot forge a valid signature. |
| **T**ampering | Mitigated the same way — the signature covers the full body, so any bit flip invalidates it. |
| **R**epudiation | N/A — Airwallex is an external system this project doesn't control the logs of; what matters is that a *valid* webhook can't be denied internally, and it can't (it's mirrored into `audit-log` transitively via the `MarkDepositReceived`/`ApplyEntry` call it triggers). |
| **I**nformation disclosure | The webhook handler returns minimal responses (200/4xx), no ledger state is echoed back to the sender. |
| **D**enial of service | **Mitigated**: `services/webhook-ingestion/internal/ratelimit` (webhook-ingestion's own copy, per docs/adr/0004 — mirrors gateway's `internal/ratelimit` token-bucket mechanics but as an HTTP middleware instead of a gRPC interceptor) now fronts the entire plain `net/http` webhook listener, keyed per source IP (`ratelimit.Middleware` wraps `webhookHandler.Mux()` in `cmd/webhook-ingestion/main.go`, tuned via `WEBHOOK_INGESTION_RATELIMIT_RPS`/`_BURST`/`_SWEEP_INTERVAL_MINUTES`). A flood of (even validly-signed, replayed) webhook POSTs from one source is now rejected with 429 before it ever reaches `VerifySignature`/`MarkDepositReceived`. Known residual gap: the key is the raw source IP (deliberately not `X-Forwarded-For`, which is spoofable) — a sender fronted by a shared NAT/proxy shares one bucket with every other sender behind it. |
| **E**levation of privilege | A valid webhook can only trigger `MarkDepositReceived` for the `tenant_id`/`deposit_id` named in its own payload — it cannot reach `ApplyEntry`/card-issuance paths directly. **Mitigated**: `VerifySignature` (`services/webhook-ingestion/internal/airwallex/webhook.go`) now rejects any delivery whose `x-timestamp` is more than 5 minutes from the verifier's clock in either direction (`ErrStaleTimestamp`), matching Airwallex's own documented delivery tolerance and shrinking the replay window from unbounded to that 5-minute skew. Replaying `MarkDepositReceived` for an already-received deposit within that window remains a no-op in `ledger-core::ledger` (status is already `Received`, not re-credited) rather than a double-credit, so this closes the window rather than just reducing the blast radius of a hit. |

### 3. Airwallex's API (card-issuance calling *out*)

Moved off gateway in the section-1 split (docs/adr/0004) — `gateway`
itself makes no outbound HTTPS calls to Airwallex anymore; only
`card-issuance` does.

| STRIDE | Assessment |
|---|---|
| **S**poofing | card-issuance verifies Airwallex's identity via normal TLS (HTTPS to `AIRWALLEX_BASE_URL`); no certificate pinning. A CA compromise could MITM this, same risk as any HTTPS API client — not specific to Ration. |
| **I**nformation disclosure | `AIRWALLEX_API_KEY`/`AIRWALLEX_CLIENT_ID` live only in card-issuance's env (`services/card-issuance/internal/config/config.go`) and, in a real deployment, Secrets Manager (`infra/terraform/modules/service`) — never logged (checked: no `log`/`slog` call in `services/card-issuance/internal/airwallex/live.go` includes the key). |
| **D**enial of service | If Airwallex's API is slow/down, every card-issuance call that depends on it (card issuance, control changes) blocks or errors, and (via gateway's fan-out) every gateway `IssueCard`/`AdjustCardLimit` call in turn — `AIRWALLEX_MODE=mock` exists specifically so demo/dev traffic never depends on Airwallex's actual availability. |

### 4. Anthropic's API / an LLM provider (agent calling *out*)

| STRIDE | Assessment |
|---|---|
| **S**poofing | Standard HTTPS; `ANTHROPIC_API_KEY` authenticates the agent to Anthropic, not the reverse — agent has no way to detect a MITM'd response beyond TLS itself. |
| **T**ampering / **E**levation of privilege | This is the actually interesting boundary: the LLM's output is *untrusted input* the moment it comes back. `docs/AGENT-DESIGN.md`'s hard-rule envelope is the mitigation — the model is told "you may grant up to exactly X" but the real enforcement is that `services/card-issuance/internal/bridge/service.go` re-validates any `ration_bridge_funding` tool call against reserve floor/bridge cap/velocity cap independently, and `ledger-core::ledger::apply_entry` enforces the reserve floor a third time. A fully adversarial or hijacked model response **cannot** move money past the floor — it can only waste a bridge/notify call or produce a nonsensical `rationale`. |
| **I**nformation disclosure | Whatever `rationale`/tenant data the agent includes in its prompt (`services/agent/internal/llm/prompt.go`) is sent to Anthropic (or Railyard Relay) — i.e., tenant deposit amounts, balances, and trigger text leave the system boundary on every decision. This is an accepted, documented data flow (same as any LLM-backed feature), not a bug, but it is the thing to point at in a data-residency conversation. |
| **D**enial of service | `services/agent/internal/llm` has no circuit breaker: a slow/erroring LLM provider stalls that tick's decision loop but `loop.Runner.Tick` is called on a fixed interval (`RATION_AGENT_POLL_INTERVAL_SECONDS`) independently each time, so one bad tick doesn't cascade into a crash loop. |
| **Spoofing** | **Mitigated**: `rationale`/`trigger` text (LLM-authored, derived from attacker-influenceable tenant data) is no longer printed verbatim by the CLI's `status` command — `sanitizeControlChars` (`services/cli/cmd/ration/status.go`) strips every C0/C1 control character (including ESC, so no ANSI/VT100 escape sequence survives) before `truncate`'s slicing and the `tabwriter` write, closing the cursor-manipulation/hidden-text injection risk this row used to document. (The TUI does not currently display `rationale`/agent decisions at all — only tenants, deposits, cards, and the live activity stream, per `services/tui/internal/client/grpc.go` — so the same sanitization will need to be applied there too the day agent-decision display is added.) |

### 5. A compromised `gateway`

Post section-1 split (docs/adr/0004), gateway is a thin BFF: it holds
no Airwallex credentials of its own anymore (those moved to
card-issuance/webhook-ingestion — see asset 4) and no business logic
beyond request/response translation (`internal/grpcserver`) and
activity-feed fan-in. Its effective reach is now "whatever
auth/tenant-registry/card-issuance/webhook-ingestion/ledger-core would
do for a request that looks legitimate," not independent logic of its
own.

| STRIDE | Assessment |
|---|---|
| **T**ampering | Can call any RPC on any of its four backing services, for any tenant (same as the CLI/TUI gap — gateway has no more and no less effective access than they do, just one more hop). Cannot rewrite `ledger-core`'s WAL or `audit-log`'s chain directly — those are separate processes/disks it has no filesystem access to. |
| **R**epudiation | **This is exactly what `audit-log` defends against.** A compromised gateway *can* fan out `RationBridgeFunding`/`IssueCard` calls to card-issuance with attacker-chosen parameters, but it cannot make `ledger-core` skip mirroring the resulting `ApplyEntry`/`RecordAgentDecision` into `audit-log` (the mirror call is inside `ledger-core`'s own `grpc.rs`, not something gateway's or card-issuance's request controls) and it cannot reach into `audit-log`'s append-only, hash-chained file to retroactively edit or delete what already landed. This is the specific property section 8 asked for: "a compromised card-issuance service can't quietly rewrite its own history" — equally true of a compromised gateway sitting in front of it. |
| **I**nformation disclosure | Can read any tenant data reachable via its four backing services' RPCs (no tenant scoping — see the gap below) but, unlike pre-split, cannot exfiltrate `AIRWALLEX_API_KEY`/`AIRWALLEX_WEBHOOK_SECRET` directly — those now live in card-issuance's/webhook-ingestion's own process memory, a separate blast radius (see actor 6 below). |
| **D**enial of service | Can flood `ledger-core` (via SimulateDeposit) or card-issuance/webhook-ingestion (via its gRPC fan-out) with calls — no rate limiting on any of those services directly, only on gateway's own public surface (see actor 1's DoS row). Can also disable/neuter its *own* rate limiter (it's in-process config, `RATION_RATELIMIT_*` env vars) — this doesn't help an external attacker but does mean "gateway's rate limiter" is not a control that survives gateway's own compromise, only external abuse of an uncompromised gateway. |
| **E**levation of privilege | Already has the maximum privilege this system grants to any internal service (call any backing-service RPC for any tenant). mTLS, once enabled, would at least confirm the *caller* really is a service holding a cert signed by the shared CA — it does not scope *which* RPCs/tenants that caller may touch. |

### 6. A compromised `card-issuance`, `webhook-ingestion`, `auth` or `tenant-registry`

The four section-1 split services (docs/adr/0004) share a common shape
for this analysis: each is reachable only from gateway (card-issuance,
tenant-registry), from gateway plus the public internet (webhook-
ingestion's HTTP listener), or from nothing yet (auth — see gap 1), and
each has no more effective privilege against `ledger-core` than gateway
itself already has, since none of them does anything gateway couldn't
also ask ledger-core to do directly.

| STRIDE | Assessment |
|---|---|
| **S**poofing | Same mTLS story as every other internal service — opt-in, off by default (`CARD_ISSUANCE_MTLS_*`/`WEBHOOK_INGESTION_MTLS_*`/`AUTH_MTLS_*`/`TENANT_REGISTRY_MTLS_*`, `infra/dev-ca/`'s `SERVICES` list now includes all four). |
| **T**ampering | card-issuance can call `ApplyEntry`/`RecordDeposit` via its own `ledgerclient` for any tenant — identical reach to what gateway had pre-split for card/bridge operations. tenant-registry can call `CreateTenant`/`ListTenants`. webhook-ingestion can call `MarkDepositReceived`. auth mutates only its own in-memory API-key/JWT/CA state (`internal/apikey`, `internal/jwtauth`, `internal/servicecertca`) — it has no ledger-core dependency at all (see its own `internal/config` doc comment), so a compromised auth cannot touch money state directly, only mint credentials nothing currently checks (gap 1). |
| **R**epudiation | card-issuance's and webhook-ingestion's ledger-core calls are mirrored into `audit-log` exactly like gateway's were pre-split (the mirror lives in `ledger-core`, not in the caller) — same guarantee, one hop earlier in the call chain now. |
| **I**nformation disclosure | card-issuance uniquely holds `AIRWALLEX_API_KEY`/`AIRWALLEX_CLIENT_ID`; webhook-ingestion uniquely holds `AIRWALLEX_WEBHOOK_SECRET` — compromising either one is strictly *less* exposure than the pre-split gateway holding both plus every ledger-core RPC's reach in one process. auth holds `AUTH_JWT_SIGNING_KEY`, a secret with no current blast radius (nothing verifies the JWTs it mints yet — gap 1) but a real one the day that wiring lands: a leaked signing key would let an attacker mint valid-looking tenant-scoped tokens for any tenant. |
| **D**enial of service | Each is an independent deployable (its own `go.mod`, its own container, its own autoscaling policy per `infra/terraform/modules/service` once applied) — flooding one (e.g. a webhook replay storm against webhook-ingestion) no longer starves the others' capacity the way one shared gateway process would have pre-split. This is the specific scaling property section 1 asked for. |
| **E**levation of privilege | card-issuance/tenant-registry/webhook-ingestion: same ceiling as gateway already had for their respective slice of gateway.proto's old surface — no new privilege created by the split. auth: **the interesting one** — `IssueServiceCert`/`MintJWT` exist today with no caller-side authorization of their own (anyone who can reach auth's gRPC port can mint a JWT for any `tenant_id`/subject, or a service cert for any `service_name`) because nothing calls them yet (gap 1) — once something *does* depend on auth's output for real authorization, auth itself becomes as privileged as whatever trusts it, and needs its own access control, not just "it exists." |

### 7. A compromised `agent`

Holds the Anthropic/Relay API key; calls `ledger-core` (read late
deposits, record decisions) and `gateway` (request bridge funding, card
actions).

| STRIDE | Assessment |
|---|---|
| **T**ampering | Same ceiling as gateway: can call any RPC gateway/ledger-core expose, but every money-moving call is still re-validated against the hard-rule envelope at `bridge/service.go` and the reserve floor at `ledger::apply_entry` — a compromised agent cannot grant itself unlimited funds, only up to whatever a legitimate-looking request would already be allowed (bounded by the real headroom of whatever tenant_id it names). |
| **R**epudiation | Mitigated the same way as gateway — `RecordAgentDecision` calls mirror into `audit-log` regardless of what called them. |
| **I**nformation disclosure | Can exfiltrate the Anthropic/Relay API key and, via `ListLateDeposits`/`ListEntries`, every tenant's deposit/balance history. |
| **D**enial of service | Could call `RationBridgeFunding`/`IssueCard` in a tight loop; bounded by gateway's rate limiter (per-tenant) for whatever tenant_id it targets, not for ledger-core calls it makes directly. |

### 8. A compromised `ledger-core`

The most consequential compromise on this list — it's the money's
single source of truth.

| STRIDE | Assessment |
|---|---|
| **T**ampering | **Can directly violate the reserve floor invariant** — it's the process that enforces it; a sufficiently deep compromise (e.g. arbitrary code execution, not just "a malicious RPC caller") controls `ledger::apply_entry`'s own logic. This is why `docs/formal/ledger.tla` and the reserve-floor property tests exist: they prove the *intended* code can't violate the invariant, not that a compromised binary can't. Out of scope for a network-boundary threat model; the mitigation is supply-chain/deploy integrity (binary provenance, not covered here), not anything at the RPC layer. |
| **R**epudiation | **This is the primary reason `audit-log` is a separate process and disk.** A compromised ledger-core can still *call* `AuditMirror::connect`'s `Append` RPC with arbitrary `payload_json` (it controls what it reports about itself), so it can lie about what it did — but it cannot edit or delete an audit-log record that already landed, because audit-log's own `store.Append` (`services/audit-log/internal/store/store.go`) is the only thing that ever writes to that file, and `VerifyChain` detects any gap or edit after the fact. A compromised ledger-core's most severe repudiation move is to simply *stop* calling `Append` (or call it with falsified `payload_json`) going forward — detectable as "audit-log's record count stopped matching ledger-core's own `ListEntries` count" (an operational check this repo does not yet automate — see the gap below). |
| **I**nformation disclosure | Holds the WAL and SQLite index in plaintext on local disk (`RATION_LEDGER_WAL_PATH`/`RATION_LEDGER_DB_PATH`) — no encryption at rest. A host-level compromise (not just an RPC-level one) reads every tenant's full financial history directly off disk. |
| **D**enial of service | Single-writer by design (`max_capacity = 1` in every `infra/terraform/envs/*/main.tf`, "single-writer hash chain" comments throughout) — there is exactly one `ledger-core` process; killing it is a full outage, by construction, not a bug. `services/wal` + `tests/recovery.rs` prove restart-and-replay is safe, which bounds the *blast radius* of a crash/DoS to "a few seconds of unavailability," not "lost or corrupted state." |
| **E**levation of privilege | N/A — it's already the most-privileged service in the system. |

### 9. A compromised `audit-log`

| STRIDE | Assessment |
|---|---|
| **T**ampering | Can refuse new `Append` calls (denial of service against future mirroring — never blocks the caller, per `AuditMirror`'s best-effort design) but, by its own `store.Append`'s hash-chaining, cannot retroactively edit a *past* record without `VerifyChain` detecting the break on the next check. It *can* append records with false `source_service`/`payload_json` going forward if compromised at the application level (it trusts whatever the gRPC caller sends — see the mTLS-identity-vs-claimed-source gap below), but that's forgery of new entries, not tampering with history. |
| **R**epudiation | A compromised audit-log cannot un-happen a `VerifyChain` call an operator already ran and recorded the result of elsewhere — but it *can* lie to a `VerifyChain` call made after compromise (it's the same process computing the answer). The actual tamper-evidence guarantee is "an *independent* re-verification of the raw `audit.log` file's hash chain, from a trusted process, will catch it" — `VerifyChain` run *by audit-log itself* post-compromise is not that independent check. Precise TODO: ship a small standalone verifier (reads `RATION_AUDIT_LOG_DATA_DIR/audit.log` directly, re-derives the chain, no dependency on the audit-log binary being trustworthy) for operators to run out-of-band — today `VerifyChain` is only exposed as audit-log's own RPC. |
| **I**nformation disclosure | Holds a full copy of every `ApplyEntry`/`RecordAgentDecision` payload (`payload_json`, including `memo`/`reference`/`rationale` free-text fields) — the same tenant data as ledger-core, duplicated. This is deliberate (that's the point of an independent trail) but doubles the blast radius of a data-exposure incident: compromising *either* ledger-core's disk or audit-log's disk now discloses the same history. |
| **D**enial of service | Pinned to `max_capacity = 1`/`desired_count = 1` in every `infra/terraform/envs/*/main.tf` ("single-writer hash chain" — same reasoning as ledger-core, since its hash chain is itself a single-writer append sequence), so a crash is a mirroring outage until restart — never a mirroring-*callers* outage, since `AuditMirror` never blocks on it. |
| **E**levation of privilege | N/A — audit-log has no write access to anything outside its own append file; it cannot reach back into ledger-core's WAL or SQLite index. |

## Cross-cutting mitigations already in place

- **mTLS** (`pkg/mtls`, `ledger_core::mtls`, `infra/dev-ca/`) — opt-in,
  mutual, every internal hop. Proves "this caller holds a cert signed
  by the shared internal CA," i.e. transport-level authentication
  between services already trusted to call each other. Does **not**
  implement authorization (see the gap below) — this is stated
  explicitly in both `pkg/mtls`'s and `ledger_core::mtls`'s own doc
  comments, repeated here because it's the single most important
  caveat in this document.
- **Per-tenant rate limiting** (`internal/ratelimit`) on gateway's
  public gRPC surface — bounds noisy-neighbor DoS between tenants, not
  a security boundary between trust levels.
- **The reserve floor invariant**, enforced three times independently
  (agent's pre-check, card-issuance's `bridge`/`cardsvc` services,
  ledger-core's `apply_entry`) — defense in depth against a bug or
  compromise at any *one* of those layers, not all three
  simultaneously.
- **Idempotency keys** on `ApplyEntry` and `(source_service, kind,
  ref_id)` on `audit-log.Append` — a retried call (network blip, crash
  mid-write) is always safe to resend, never double-applies.
- **HMAC webhook signatures** — Airwallex deliveries are authenticated
  and tamper-evident (modulo the replay-window gap above).
- **The audit-log trust boundary itself** — the core deliverable of
  section 8: a second, append-only, hash-chained copy of every
  money-moving/decision event, living in a process and on a disk that
  no other service in this system has write access to.

## Known gaps (precise, scoped — not glossed over)

1. **No tenant-scoped authorization anywhere, still.** Every RPC that
   takes a `tenant_id` (`ledger-core`'s entire surface, `gateway`'s
   entire surface) trusts whatever `tenant_id` the caller supplies —
   there is no notion of "this caller is only allowed to act as tenant
   X." `services/auth` now exists (`docs/SCALE-EXPANSION.md` section 1)
   and its `IssueAPIKey`/`VerifyAPIKey`/`MintJWT`/`VerifyJWT` RPCs all
   work correctly in isolation (`internal/apikey`, `internal/jwtauth`
   are both unit-tested) — but **nothing calls them yet**: `gateway`'s
   `internal/grpcserver` does not dial auth at all, and no RPC in
   `ledger.proto`/`gateway.proto`/`card_issuance.proto`/
   `tenant_registry.proto` checks a caller-presented API key or JWT
   before acting. auth shipping as a service changes nothing about this
   gap by itself — it only becomes a mitigation the day gateway (or
   each split service directly) actually verifies a credential before
   proxying a tenant-scoped call. mTLS proves service identity ("this
   is really gateway calling"), never tenant identity ("this call is
   authorized to touch tenant X's money"), and auth existing-but-unwired
   doesn't change that either. This is the single biggest residual risk
   in the system today and the reason this repo is explicitly
   pre-production. **Precise TODO**: wire `gateway`'s `internal/
   grpcserver` (or a shared interceptor every split service imports) to
   call `auth.VerifyAPIKey`/`VerifyJWT` on every tenant-scoped RPC and
   reject a mismatch between the verified identity's `tenant_id` and the
   request's `tenant_id`, before this is safe to run with real
   tenants/real money.
2. **`audit-log` trusts the claimed `source_service` field verbatim.**
   `AppendRequest.source_service` is attacker-controlled data from
   audit-log's point of view — with mTLS *disabled* (the default),
   nothing binds "the gRPC caller's transport identity" to "the
   `source_service` string it claims." A compromised `agent` could call
   `audit-log.Append` directly with `source_service: "ledger-core"` and
   forge an entry that looks like it came from the authoritative
   ledger. **Precise TODO**: once mTLS is enabled, have
   `services/audit-log/internal/grpcserver/server.go`'s `Append`
   extract the caller's verified certificate CN (available via
   `tonic`/`grpc-go`'s peer info) and either overwrite
   `source_service` with it or reject a mismatch, rather than trusting
   the request body's field. Until mTLS is mandatory (not just
   opt-in) in a given deployment, this gap is unavoidable.
3. **No operational cross-check between ledger-core's own entry count
   and audit-log's mirrored count.** `AuditMirror` is best-effort by
   design (never blocks `ApplyEntry`), which means a sustained
   audit-log outage silently drops mirroring with only a `tracing::warn!`
   log line per failed call — nothing pages anyone or alerts on
   "ledger-core's `ListEntries` count and audit-log's `ListRecords`
   count have diverged by more than expected network-blip noise."
   **Precise TODO**: a Prometheus counter (see `docs/SCALE-EXPANSION.md`
   section 2) incremented on every `spawn_append` failure in
   `services/ledger-core/src/audit_mirror.rs`, alerted on past a small
   non-zero threshold.
4. **Public HTTP webhook listener has no rate limiting** (actor 2's DoS
   row above) and **no replay-window check** (actor 2's EoP row above).
5. **mTLS certs are dev-only self-signed** (`infra/dev-ca/`, explicitly
   labeled "DEV-ONLY" in its own script header) with 1-year leaves and
   no rotation story. A real deployment needs a managed CA (AWS Private
   CA, Vault PKI, or a service mesh's own mTLS) with short-lived,
   auto-rotated certs — `infra/dev-ca/generate-dev-ca.sh`'s own header
   says this explicitly; repeated here because it's load-bearing for
   this entire threat model's mTLS-based mitigations.
6. **No encryption at rest** for `ledger-core`'s WAL/SQLite index or
   `audit-log`'s append file — a host/disk-level compromise (not an RPC
   one) reads everything in plaintext. Out of scope for section 8
   specifically (it's a storage-layer concern, not a service-boundary
   one) but worth naming here rather than implying this document covers
   every threat.
7. **Terminal-escape-sequence injection via `rationale`/`memo` display**
   in `tui`/`cli` (actor 4's gap row above) — low severity (local
   annoyance, not a money-moving path) but unmitigated today.

## What mTLS is, and is not, a defense against (repeated from `pkg/mtls`)

mTLS answers exactly one question: *"is the process on the other end of
this connection holding a private key corresponding to a certificate
the shared internal CA signed?"* It is the right tool for "keep an
untrusted network from impersonating or eavesdropping on
service-to-service traffic." It is the wrong tool — and this repo does
not claim otherwise — for "is this specific call allowed to touch this
specific tenant's money," which is gap #1 above and needs an
application-layer authorization system, not a transport-layer one.
