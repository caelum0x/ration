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

## services/gateway (Go)

Owns Airwallex. Never touches money state directly — every balance-moving
operation proxies to ledger-core over gRPC.

Files:
- `cmd/gateway/main.go` — wiring: config, ledger-core client, airwallex
  client (mock or live per `AIRWALLEX_MODE`), gRPC server for
  `gateway.proto`.
- `internal/config/config.go` — env loading + validation, fails fast on
  missing required vars in live mode.
- `internal/airwallex/client.go` — interface `type Client interface {
  IssueCard(...); GetCard(...); SetCardControls(...);
  ListGlobalAccountTransactions(...); VerifyWebhookSignature(...) }`.
- `internal/airwallex/live.go` — real HTTP client: auth token exchange
  (`POST /api/v1/authentication/login`), issuing endpoints, global account
  endpoints, correct Airwallex header/signature handling.
- `internal/airwallex/mock.go` — in-memory fake implementing the same
  interface, deterministic fixtures, used when `AIRWALLEX_MODE=mock`
  (default).
- `internal/airwallex/webhook.go` — webhook payload types + HMAC signature
  verification (shared by live and a `/webhooks/airwallex` HTTP handler
  used in mock mode to simulate deposit-received events).
- `internal/ledgerclient/client.go` — typed gRPC client wrapper around
  `ledger.LedgerCoreClient`.
- `internal/tenant/service.go` — tenant CRUD, delegates persistence to
  ledger-core.
- `internal/card/service.go` — `IssueCard`, `AdjustCardLimit`:
  airwallex.IssueCard -> ledgerclient reserve-floor check -> persist card
  metadata -> ledger entry for any hold.
- `internal/bridge/service.go` — `RationBridgeFunding`: the actual rationing
  math (see AGENT-DESIGN.md policy) — computes how much of a requested
  amount can be granted without breaching reserve floor, applies a HOLD
  ledger entry for the granted portion.
- `internal/activity/stream.go` — fan-out event bus (Go channels) feeding
  `StreamActivity` gRPC stream to the TUI.
- `internal/grpcserver/server.go` — `gateway.proto` service impl, wires the
  above.
- `internal/httpserver/webhooks.go` — plain HTTP listener for Airwallex
  webhook delivery (signature-verified), only live in `AIRWALLEX_MODE=live`.
- `go.mod`, `*_test.go` next to each package (table-driven tests, esp.
  `bridge/service_test.go` covering partial-grant and reserve-floor-breach
  cases).

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
