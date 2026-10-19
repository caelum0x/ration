# Architecture

## Money type

Everywhere: fixed-point integer minor units (int64, e.g. cents) + ISO 4217
currency string. Never float. Mirrored as `Money{minor_units, currency}` in
proto, `ledger-core::money::Money` in Rust (checked add/sub, no silent
overflow), `pkg/money.Money` in Go.

## services/wal (C)

A minimal, real write-ahead log — this is the low-level showcase piece, not
a toy. Format: sequence of length-prefixed, CRC32-checksummed frames,
append-only, `fsync()` after every durable append (durability > throughput
for a ledger). On startup, replay from the last valid frame to rebuild
state; a corrupt/truncated tail frame (torn write) is detected via CRC and
discarded, not treated as fatal.

Files:
- `wal.h` — public API: `wal_open`, `wal_append`, `wal_read_next`,
  `wal_sync`, `wal_close`, `wal_recover`.
- `wal.c` — implementation.
- `crc32.c` / `crc32.h` — standalone CRC32 (no external deps).
- `frame.h` — on-disk frame layout (length, crc, payload) as a packed struct
  with explicit byte order (little-endian on disk, documented).
- `tests/test_wal.c` — append/read-back, torn-write recovery, fsync-crash
  simulation (truncate mid-frame, assert recovery drops it cleanly).
- `Makefile` — builds `libwal.a` (static lib) consumed by Rust via FFI, plus
  `test_wal` binary.
- `bench/bench_wal.c` — append throughput micro-benchmark (optional but
  real, not a stub).

## services/ledger-core (Rust)

Owns all tenant/deposit/balance/ledger-entry state. Links `libwal.a` via a
`build.rs` that compiles `services/wal` with `cc` and a thin `unsafe extern
"C"` binding module (`src/wal_ffi.rs`) wrapped in a safe `Wal` struct — no
`unsafe` outside that one module.

Files:
- `Cargo.toml` (deps: tonic, prost, tokio, rusqlite or sled for the
  queryable index — WAL is the durability source of truth, an embedded
  index db is fine for list/query paths), `build.rs` (compiles C, runs
  tonic-build on `../../proto/*.proto`).
- `src/main.rs` — boots the gRPC server (`LedgerCore` service from
  `ledger.proto`).
- `src/money.rs` — `Money` type, checked arithmetic, currency mismatches
  are a typed error, not a panic.
- `src/wal_ffi.rs` — the only `unsafe` module; safe wrapper around
  `libwal.a`.
- `src/ledger.rs` — core domain: `apply_entry` (idempotent on
  `idempotency_key`, enforces reserve floor on DEBIT/HOLD before writing to
  WAL, THEN updates in-memory/indexed balance), `get_balance`,
  `check_reserve_floor`.
- `src/tenant.rs`, `src/deposit.rs`, `src/agent_decision.rs` — domain
  entities + repository functions over the index db.
- `src/grpc.rs` — `LedgerCore` trait impl, maps domain <-> proto types,
  recovers WAL state on boot before accepting RPCs.
- `src/error.rs` — typed domain errors -> tonic::Status mapping.
- `tests/idempotency.rs`, `tests/reserve_floor.rs`, `tests/recovery.rs`
  (kills the process mid-write via a test hook, restarts, asserts no lost
  or double-applied entries) — these are the tests that actually prove the
  ledger is correct; do not skip them.

## services/gateway (Go) — thin BFF

**Post section-1 split** (`docs/SCALE-EXPANSION.md` section 1,
`docs/adr/0004`): gateway no longer owns Airwallex, tenant CRUD or the
rationing math directly — it is a thin BFF/aggregator implementing
`gateway.proto` by fanning out gRPC calls to the four services below.
Its external interface (what the TUI/CLI/agent dial) is byte-for-byte
unchanged from before the split.

Files:
- `cmd/gateway/main.go` — wiring: config, ledger-core client (for
  `SimulateDeposit` only), card-issuance client, webhook-ingestion
  client (relayed into the local activity bus), gRPC server for
  `gateway.proto`.
- `internal/config/config.go` — env loading + validation: where to dial
  each backing service, rate-limit tuning, mTLS. No Airwallex or
  bridge-policy config anymore (moved to card-issuance).
- `internal/cardissuanceclient/client.go` — typed gRPC client wrapper
  around `cardissuance.CardIssuanceClient`.
- `internal/webhookeventsclient/client.go` — typed gRPC client wrapper
  that subscribes to webhook-ingestion's `StreamEvents` and republishes
  every event onto gateway's own `internal/activity` bus.
- `internal/ledgerclient/client.go` — typed gRPC client wrapper around
  `ledger.LedgerCoreClient`, used only by `SimulateDeposit` now.
- `internal/activity/stream.go` — fan-out event bus (Go channels) feeding
  `StreamActivity` gRPC stream to the TUI — fed both by gateway's own
  RPC handlers (card/bridge/deposit events) and by the webhook-ingestion
  relay above.
- `internal/grpcserver/server.go` — `gateway.proto` service impl: each
  method either translates to a `cardissuanceclient` call (IssueCard,
  AdjustCardLimit, GetCard, ListCards, RationBridgeFunding), talks to
  ledger-core directly (SimulateDeposit), or serves the local activity
  bus (StreamActivity).
- `internal/ratelimit/limiter.go` — per-tenant token bucket in front of
  every RPC on this service's public gRPC surface (unchanged by the
  split — this is gateway's surface to protect, not any backing
  service's).
- `go.mod`, `*_test.go` next to each package.

## services/auth (Go)

New capability, not an extraction (`docs/adr/0004`) — nothing in the
pre-split gateway issued per-service mTLS certs or tenant API
keys/JWTs. Has no ledger-core dependency.

Files:
- `cmd/auth/main.go` — wiring: JWT signing key, CA (fresh in-memory or
  loaded from disk), gRPC server for `auth.proto`.
- `internal/apikey/apikey.go` — tenant API-key issuance (plaintext
  secret returned once), salted-hash verification, revocation.
- `internal/jwtauth/jwtauth.go` — short-lived bearer-token minting and
  verification, scoped to a tenant + subject.
- `internal/servicecertca/ca.go` — runtime CSR signing against an
  in-process CA, for service-to-service mTLS identity issuance.
- `internal/grpcserver/server.go` — `auth.proto` service impl, wires the
  above. **Not yet dialed by gateway or any other service** — see
  `docs/THREAT-MODEL.md`'s known-gap 1.

## services/tenant-registry (Go)

A direct extraction of the old gateway's `internal/tenant/service.go`
(`docs/adr/0004`) — owns no state of its own, every write proxies to
ledger-core.

Files:
- `cmd/tenant-registry/main.go` — wiring: ledger-core client, gRPC
  server for `tenant_registry.proto`.
- `internal/tenant/service.go` — `CreateTenant`/`GetTenant`/`ListTenants`,
  delegates persistence to ledger-core.
- `internal/ledgerclient/client.go` — this service's own typed gRPC
  client wrapper around `ledger.LedgerCoreClient` (each split service
  gets its own copy, not a shared import — see `docs/adr/0004`).
- `internal/grpcserver/server.go` — `tenant_registry.proto` service
  impl.

## services/card-issuance (Go) — the highest-value service

A direct extraction of the old gateway's `internal/card` +
`internal/bridge` (`docs/adr/0004`): Airwallex issuing calls and the
rationing/bridge-funding math — where real money decisions happen.
Stateless and idempotent per tenant (correctness rests on ledger-core's
own idempotent, reserve-floor-checked `ApplyEntry`), so it is safe to
run as N replicas.

Files:
- `cmd/card-issuance/main.go` — wiring: config, ledger-core client,
  Airwallex client (mock or live per `AIRWALLEX_MODE`), gRPC server for
  `card_issuance.proto`.
- `internal/config/config.go` — Airwallex + bridge-policy env loading
  (this service's share of the old gateway's `internal/config`).
- `internal/airwallex/client.go` / `live.go` / `mock.go` — unchanged
  from the old gateway's package of the same name, moved verbatim.
- `internal/cardsvc/service.go` — `IssueCard`, `AdjustCardLimit`,
  `GetCard`, `ListCards`: airwallex call -> ledgerclient reserve-floor
  check -> persist card metadata -> ledger entry for any hold.
- `internal/bridge/service.go` — `RationBridgeFunding`: the actual
  rationing math (see `AGENT-DESIGN.md`'s policy) — computes how much
  of a requested amount can be granted without breaching reserve floor,
  applies a HOLD ledger entry for the granted portion. Carries over the
  old gateway's `bridge/service_test.go` cases unchanged — the business
  logic didn't change, only which deployable it lives in.
- `internal/ledgerclient/client.go` — this service's own typed gRPC
  client wrapper around `ledger.LedgerCoreClient`.
- `internal/grpcserver/server.go` — `card_issuance.proto` service impl,
  a direct passthrough to `cardsvc`/`bridge` (unlike the old gateway,
  this package's wire types and its internal services' types are
  identical, so there's no translation to do here — gateway's own
  `internal/grpcserver` is where the `cardissuance.*` <-> `gateway.*`
  translation now happens).

## services/webhook-ingestion (Go)

A direct extraction of the old gateway's `internal/airwallex/webhook.go`
(signature verification) + `internal/httpserver/webhooks.go` (the HTTP
listener) (`docs/adr/0004`) — isolates Airwallex's bursty, externally
triggered webhook traffic from every other service's deploy cadence.

Files:
- `cmd/webhook-ingestion/main.go` — wiring: ledger-core client, webhook
  secret, gRPC server for `webhook_ingestion.proto` **and** a plain
  HTTP listener for `/webhooks/airwallex` (always on, independent of
  any mock/live mode — unlike the old gateway's webhook listener, which
  only started in `AIRWALLEX_MODE=live`).
- `internal/airwallex/webhook.go` — webhook payload types + HMAC
  signature verification, moved verbatim from the old gateway.
- `internal/webhookhandler/webhooks.go` — the HTTP handler: verifies
  signature, parses the envelope, turns
  `global_account.transaction.created` into a `MarkDepositReceived`
  call against ledger-core, publishes a normalized event either way.
- `internal/eventbus/eventbus.go` — fan-out event bus (Go channels)
  feeding `webhook_ingestion.proto`'s `StreamEvents` RPC — the
  documented subscriber is gateway's own `internal/webhookeventsclient`,
  merging these events into its activity feed.
- `internal/ledgerclient/client.go` — this service's own typed gRPC
  client wrapper around `ledger.LedgerCoreClient`.
- `internal/grpcserver/server.go` — `webhook_ingestion.proto` service
  impl (`StreamEvents` only — webhook ingestion itself happens over
  plain HTTP, not gRPC).

## services/agent (Go)

The actual "agentic" part. A loop, not a chatbot.

Files:
- `cmd/agent/main.go` — polling loop: every N seconds, ask ledger-core for
  late deposits (`ListLateDeposits`), and for each tenant with pending card
  requests, run the decision pipeline.
- `internal/policy/rules.go` — deterministic, auditable rules evaluated
  BEFORE any LLM call: hard reserve-floor floor, max single-grant cap, max
  grants per tenant per day. These can never be overridden by the LLM —
  the LLM only decides *how much* within the rule-approved envelope and
  *what to tell the tenant*, never whether to breach a hard rule.
- `internal/llm/provider.go` — thin interface over an Anthropic Messages API
  client (model configurable via `RATION_AGENT_MODEL`, default
  `claude-sonnet-5`), tool-calling loop.
- `internal/llm/tools.go` — tool schemas: `ration_bridge_funding`,
  `issue_card`, `adjust_card_limit`, `notify_tenant` — each tool's handler
  calls the gateway gRPC client, and every tool call + result is logged via
  `ledgerclient.RecordAgentDecision` for audit (never silent).
- `internal/llm/prompt.go` — system prompt: the rationing policy in prose,
  explicit instruction to stay within the pre-computed hard envelope from
  `policy/rules.go`, and to prefer partial grants + clear tenant
  notification over silent decline.
- `internal/loop/runner.go` — ties policy + llm + gateway client together,
  per-tenant decision cycle, structured logging of every decision.
- `go.mod`, `internal/policy/rules_test.go`, `internal/loop/runner_test.go`
  (with a fake LLM provider so the loop is tested without network calls).

## services/tui (Go, Bubble Tea)

Files:
- `cmd/tui/main.go` — bubbletea program entry.
- `internal/model/model.go` — top-level `tea.Model`, tab state (tenants /
  deposits / cards / activity feed).
- `internal/view/tenants.go`, `deposits.go`, `cards.go`, `activity.go` —
  one file per tab, each a `lipgloss`-styled table/list.
- `internal/client/grpc.go` — gateway + ledger-core gRPC clients, activity
  stream consumer feeding `tea.Msg` updates.

## services/cli (Go)

- `cmd/ration/main.go` — cobra-based: `ration seed` (creates 3 demo
  tenants with realistic reserve floors), `ration simulate-deposit
  --tenant --amount --late` (drives the late-deposit scenario judges will
  want to see), `ration status --tenant`.

## Cross-cutting

- `pkg/pb/` — generated Go stubs (checked in after `make proto`, so judges
  don't need protoc to build).
- `pkg/money/money.go` — Go mirror of the Money type + formatting helpers,
  imported by gateway/agent/tui/cli.
- Structured logging (`log/slog`) everywhere in Go; `tracing` crate in Rust.
  Every state-changing call logs tenant_id, action, before/after balance.
