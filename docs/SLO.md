# Service Level Objectives

Targets per service, and exactly how each would be measured from the
Prometheus metrics `pkg/metrics` (Go) exposes today and the metrics
Rust `ledger-core` does not yet expose (see the explicit gap called out
under ledger-core below — this is a real TODO, not glossed over).

Every Go service that wires `pkg/metrics` exposes its own `/metrics`
(`Metrics.Server(addr)` in `pkg/metrics/server.go`) on a dedicated
metrics address distinct from its gRPC/HTTP ports (e.g. gateway:
`GATEWAY_METRICS_ADDR`, default `localhost:9091`,
`services/gateway/internal/config/config.go`) — scrape that, not the
service port. Every metric carries a `service` const-label (set at
`metrics.New(serviceName)`), so one Prometheus instance scraping every
service's `/metrics` can slice any query by `service=`.

## How to read this doc

For each service: a latency target (p50/p99 for the RPC(s) that matter
most), an availability target, and the exact PromQL this would run
against once Prometheus is actually scraping every service's
`/metrics` endpoint (per `infra/otel-collector`/Grafana, section 2 —
wiring the scrape config itself is infra work, not covered here).
Targets are deliberately conservative for a system at this project's
real scale (a hackathon-grown ledger, not a payments unicorn's current
traffic) — they are meant to catch real regressions, not to be
impressive numbers.

---

## `ledger-core` (Rust)

**Why it matters most:** every balance-moving RPC in the whole system
ultimately calls `LedgerCore.ApplyEntry`
(`services/ledger-core/src/grpc.rs` → `ledger.rs::apply_entry`), which
holds `Ledger.inner`'s mutex for the full decide→WAL-append-fsync→SQLite-
commit duration (`append_and_commit`). Its tail latency is a lower bound
on every other service's tail latency for anything that moves money.

| RPC | p50 target | p99 target | Availability |
|---|---|---|---|
| `ApplyEntry` | 10ms | 50ms | 99.9% (error rate < 0.1%, excluding `FAILED_PRECONDITION`/`failed_precondition` reserve-floor declines, which are a correct business outcome, not an error) |
| `GetBalance` / `CheckReserveFloor` (read paths) | 5ms | 20ms | 99.9% |
| `CreateTenant` / `RecordDeposit` / `MarkDepositReceived` | 15ms | 75ms | 99.9% |

The `ApplyEntry` target is set by the dominant fixed cost in its path:
one `fsync()` per append (`wal_append` in `services/wal/wal.c`,
deliberately "durability > throughput" per `ARCHITECTURE.md`) plus one
SQLite transaction commit. 10ms/50ms assumes a local SSD WAL file (not
network-attached storage); the Terraform `ledger-backup` module notes
ledger-core itself "stays on local/EBS disk for the WAL" for exactly
this reason — moving the WAL to network storage would blow this budget.

**Tracing landed, metrics did not (real, scoped gap remains):**
`ledger-core` now has `tracing-opentelemetry` wired
(`src/telemetry.rs`): every RPC gets a server span via
`tonic-tracing-opentelemetry`'s `OtelGrpcLayer` (joined to the caller's
W3C `traceparent` when the Go services' `pkg/telemetry` client
interceptors sent one), exported over OTLP/gRPC to
`infra/otel-collector`, plus structured JSON logs via
`tracing_subscriber`'s `fmt().json()` layer — the full "Rust
`ledger-core`: `tracing` + `tracing-opentelemetry`, same trace
propagation via gRPC metadata" half of `docs/SCALE-EXPANSION.md`
section 2. So a trace started by a CLI/TUI command or an agent tick now
extends all the way into `ledger-core`'s own `ApplyEntry` span, visible
in Grafana/Tempo/Jaeger by `trace_id`.

What's still missing is the **full `pkg/metrics`-equivalent** half:
`ledger-core` now has one Prometheus counter and one `/metrics` HTTP
listener (`src/metrics.rs`, bound in `main.rs` at
`LEDGER_CORE_METRICS_ADDR`, scraped per
`infra/prometheus/prometheus.yml`'s `ledger-core` job) — but that
counter is scoped narrowly to `docs/THREAT-MODEL.md`'s "Known gaps" #3
(audit-mirror `spawn_append` failures), not this table's per-RPC
latency/availability targets. There is still no
`ledger_core_grpc_server_requests_total` /
`..._request_duration_seconds` pair (the Go services' `pkg/metrics`
equivalent) for `ApplyEntry` or any other RPC, so no PromQL query can
check this table's targets directly from scraped metrics yet (traces
give per-request timing, but not the aggregated histograms/counters
this table's targets are phrased against); the
fallback diagnostics are `RUNBOOK.md`'s "ledger-core won't start" /
"negative headroom" entries, a trace-based p50/p99 read against
whatever backend `infra/otel-collector` exports spans to, and
wall-clock timing from outside the process (e.g. gateway's own
`ledgerclient` call latency, which IS visible — see below). **TODO,
precise blocker:** add a Rust Prometheus client (the `prometheus`
crate, matching `pkg/metrics`'s `prometheus/client_golang` choice on
the Go side) emitting the Go-side-equivalent
`ledger_core_grpc_server_requests_total{method,code}` /
`ledger_core_grpc_server_request_duration_seconds{method}`, record them
from a small wrapper around `TracedService` (or a second tower layer
composed the same way, per `src/telemetry.rs`'s doc comment on why the
per-service wrap is required under this crate's pinned tonic 0.12), and
bind a `/metrics` HTTP listener (e.g. via `hyper` or `axum`, both
already in the dependency tree transitively) next to the gRPC server in
`main.rs` — this is additive instrumentation code, deliberately not
done in this pass alongside the tracing wiring so each lands as its own
reviewable unit.

**Indirect measurement available today:** gateway's
`internal/ledgerclient` calls are themselves instrumented (gateway's own
`ration_grpc_server_request_duration_seconds` covers gateway's *own*
RPCs, not its outbound calls to ledger-core — so even this indirect
path currently only bounds total request time from the *caller's*
RPC-to-RPC latency, via gateway's bridge/card service latency minus
known gateway-side work; it is not a substitute for direct ledger-core
instrumentation and should not be treated as one in an incident).

---

## `services/gateway` (Go) — monolith today; BFF after the split (ADR 0004)

| RPC | p50 target | p99 target | Availability |
|---|---|---|---|
| `IssueCard` / `AdjustCardLimit` | 150ms | 500ms | 99.5% (dominated by the Airwallex HTTP call in live mode; `AIRWALLEX_MODE=mock` should comfortably beat this) |
| `RationBridgeFunding` | 30ms | 120ms | 99.9% (no external HTTP call in this path — it's `GetTenant`+`GetBalance`+`ListDeposits`+`ListEntries`+`ApplyEntry` against ledger-core, per `bridge.Service.RationBridgeFunding`) |
| `StreamActivity` | n/a (long-lived stream) | — | 99.5% connection uptime; target < 2s from event publish (`internal/activity/stream.go`'s fan-out) to client delivery |

**Measurement (available today):** `ration_grpc_server_requests_total{service="gateway",method,code}`
and `ration_grpc_server_request_duration_seconds{service="gateway",method}`
(`pkg/metrics/metrics.go`, wired via `UnaryServerInterceptor`/
`StreamServerInterceptor` in `pkg/metrics/interceptors.go`, registered on
gateway's gRPC server in `cmd/gateway/main.go`). Example PromQL for the
`RationBridgeFunding` p99:

```promql
histogram_quantile(0.99,
  sum(rate(ration_grpc_server_request_duration_seconds_bucket{
    service="gateway", method="/ration.gateway.v1.Gateway/RationBridgeFunding"
  }[5m])) by (le)
)
```

Availability (error budget) for the same RPC:

```promql
1 - (
  sum(rate(ration_grpc_server_requests_total{
    service="gateway", method="/ration.gateway.v1.Gateway/RationBridgeFunding", code!="OK"
  }[5m]))
  /
  sum(rate(ration_grpc_server_requests_total{
    service="gateway", method="/ration.gateway.v1.Gateway/RationBridgeFunding"
  }[5m]))
)
```

**Business-outcome metrics, not pure latency/availability, but part of
this service's health story:** `ration_bridge_grants_total{service="gateway",result="granted"|"partial"|"declined"}`
and `ration_reserve_floor_headroom_minor_units{service="gateway",tenant_id}`
(both in `pkg/metrics/metrics.go`, recorded by `bridge.Service`). A step
change in the ratio of `declined` to `granted`+`partial` across *every*
tenant simultaneously is a policy/config regression, not a latency/
availability breach — see `RUNBOOK.md`'s "agent loop is granting $0
bridges" entry, which this metric feeds directly.

After the service split (ADR 0004), these same targets apply to the
thin BFF's fan-out calls; `services/card-issuance`,
`services/tenant-registry`, and `services/webhook-ingestion` each get
their own copy of this table once they exist as independently
deployable services with their own `/metrics` — not written here
speculatively, since their exact RPC surfaces (`card_issuance.proto`
etc.) don't exist in `proto/` yet.

---

## `services/agent` (Go)

Not gRPC-served (it's a polling loop, `internal/loop/runner.go`), so its
SLO is framed as the loop's own cadence and downstream-call health
rather than an RPC table:

| Dimension | Target |
|---|---|
| Poll cycle latency (one full pass over `ListLateDeposits` + per-tenant decision pipeline) | p99 < `RATION_AGENT_POLL_INTERVAL_SECONDS` (default 10s per `cmd/agent/main.go`'s `defaultPollInterval`) — a cycle that takes longer than the interval it's supposed to run on is itself the alert condition, not a separate threshold |
| LLM round-trip (`internal/llm/provider.go`'s Anthropic Messages API call) | p99 < 8s per tool-calling turn |
| Availability (successful poll cycles, excluding cycles with zero late deposits to act on) | 99% |

**Measurement:** the agent process does not currently export its own
`/metrics` (no `metrics.New("agent")` call exists in
`services/agent/cmd/agent/main.go` today, unlike gateway) — its downstream
calls land in *gateway's* and *ledger-core's* RPC metrics (as the
caller), so agent-side health today is read from structured JSON logs
(`log/slog`, one line per decision cycle) grep'd by correlation ID, not
PromQL. **TODO:** wire `metrics.New("agent")` + a `/metrics` listener in
`cmd/agent/main.go`, mirroring gateway's pattern exactly — scoped,
small, not done in this pass for the same reason as the ledger-core gap
above (instrumentation code, not documentation).

---

## `services/wal` (C, linked into `ledger-core`, not independently
deployed)

Not a service with its own SLO (it has no network surface — it's a
linked library), but its append latency is the hard floor the
`ledger-core` `ApplyEntry` target above is built on top of:

| Dimension | Target |
|---|---|
| `wal_append` (header write + payload write + `fsync()`) | p99 < 5ms on local SSD |

**Measurement:** `services/wal/bench/bench_wal.c` is the throughput
micro-benchmark that would catch a regression here (run via `make -C
services/wal` targets) — this is a build-time/CI benchmark, not a
Prometheus metric, since the WAL has no process of its own to scrape.
If `ledger-core`'s `ApplyEntry` p99 (once instrumented, per the gap
above) regresses without any change to `ledger.rs`'s logic, `bench_wal.c`
is the first place to check whether the regression is actually here.
