# ADR 0003: gRPC as the source of truth, REST as a convenience facade

## Status

Accepted for the service-to-service surface (already built: every
service-to-service call in the codebase today is gRPC over the proto
contracts in `proto/`). The external REST/JSON facade described in
`docs/SCALE-EXPANSION.md` section 7 is planned but not yet implemented
as of this writing — see Consequences for the precise state and the
TODO it leaves.

## Context

Every contract in this system is already defined once, in `proto/`:
`common.proto` (shared types: `Money`, `LedgerEntryType`, `AgentAction`,
`DepositStatus`), `ledger.proto` (`LedgerCore` service, consumed by
`services/gateway/internal/ledgerclient` and `services/agent/internal/
ledgerclient`), `gateway.proto` (`Gateway` service: `IssueCard`,
`AdjustCardLimit`, `RationBridgeFunding`, `StreamActivity`, consumed by
the TUI and CLI), and `audit.proto`. `make proto` (`scripts/gen-proto.sh`)
generates Go stubs into `pkg/pb/` (checked in, per `ARCHITECTURE.md`, "so
judges don't need protoc to build") and Rust stubs via `tonic-build` in
`ledger-core`'s `build.rs`.

Two different audiences need to talk to this system:

1. **Services talking to each other** — gateway → ledger-core, agent →
   gateway → ledger-core, every hop needing low latency, strict typing,
   bidirectional streaming (`StreamActivity`), and (per section 8) mTLS
   client certs issued per service.
2. **Humans and simple tooling** — a judge or integrator without a gRPC
   client installed who wants to `curl` the gateway, and third-party
   backend consumers who would reasonably expect a typed SDK rather than
   hand-rolling gRPC stubs themselves.

## Decision

gRPC over the `proto/` contracts remains the single source of truth for
every service boundary; nothing in this system is ever defined twice.
Where a non-gRPC audience needs access (the `curl`-able case, external
integrators), that access is built as a thin facade *in front of* the
existing gRPC service — translating JSON <-> the generated gRPC
client — not as a second, independently-maintained implementation of the
business logic.

Concretely, per `docs/SCALE-EXPANSION.md` section 7:

- `services/gateway` gets a REST/JSON facade (either `grpc-gateway`
  reverse-proxy annotations on `gateway.proto`, or a hand-rolled
  `internal/httpapi` package translating JSON <-> the `Gateway` gRPC
  client) that sits in front of the same `grpcserver.Server` /
  `bridge.Service` / `card.Service` code paths gateway already runs for
  its gRPC listener — never a parallel implementation of the rationing
  math or card-issuance logic.
- `sdk/go/ration` and `sdk/rust/ration-client` wrap the generated gRPC
  stubs with ergonomic constructors — they are typed clients for the
  gRPC surface, not an alternate protocol.
- Documentation for the facade states explicitly, in its own words, "REST
  is a convenience facade over the gRPC source of truth," so no consumer
  mistakes the JSON surface for an independently-versioned API.

Why gRPC first rather than REST first:

- **Strict typing end to end.** `Money{minor_units, currency}` as an
  `int64` + ISO 4217 string (`ARCHITECTURE.md`) round-trips through
  protobuf without the float-vs-string ambiguity a hand-written JSON
  schema would have to re-litigate at every service boundary.
- **Streaming is a first-class proto feature.** `StreamActivity` (in
  `gateway.proto`) is a server-streaming RPC consumed directly by the
  TUI's activity feed (`services/tui/internal/client/grpc.go`) and
  gateway's internal fan-out (`internal/activity/stream.go`) — REST has
  no equivalent primitive; it would need SSE or WebSockets layered on
  top, which is exactly the kind of second protocol this ADR avoids
  making a second source of truth.
- **mTLS and interceptor-based cross-cutting concerns compose cleanly on
  gRPC.** Section 8's service-to-service mTLS (`pkg/mtls`) and section
  2's trace/metric propagation (`pkg/telemetry`'s gRPC client/server
  interceptors, `pkg/metrics`'s gRPC interceptors) are both written once,
  against gRPC's interceptor chain, and apply uniformly to every
  service-to-service call without being re-implemented for a second
  transport.
- **Generated stubs are already the integration point.** `pkg/pb/` is
  already checked into the repo specifically so nothing downstream (Go
  services, the planned Go/Rust SDKs, a REST facade's translation layer)
  needs its own hand-maintained copy of the wire format.

## Alternatives considered

- **REST/JSON as the primary (and only) protocol.** Rejected: loses
  native streaming for `StreamActivity`, loses compile-time-checked
  request/response types across language boundaries (Go gateway, Rust
  ledger-core, Go agent/tui/cli), and would require hand-maintaining an
  OpenAPI schema in parallel with `proto/` — exactly the "two sources of
  truth" failure mode this ADR exists to avoid.
- **GraphQL facade instead of REST.** Rejected as unnecessary complexity
  for this system's actual external consumers (judges running `curl`,
  backend integrators who want a typed SDK) — a REST/JSON facade with a
  stated single responsibility (convenience over the real gRPC API)
  serves that need with far less machinery, and `grpc-gateway`-style
  reverse-proxy annotations give REST "for free" from the proto
  definitions that already exist, with no new schema to maintain.
- **Two independently maintained APIs (gRPC internal, hand-written REST
  external with its own handlers reimplementing the business logic).**
  Rejected explicitly: that duplicates `bridge.Service`'s rationing math
  and `card.Service`'s issuance flow behind two code paths that WILL
  drift, which is precisely the production-reliability failure mode
  "fintech bar, not hackathon shortcuts" (`SCALE-EXPANSION.md`'s standing
  rule) rules out.

## Consequences

- Anything built under `internal/httpapi` (or via `grpc-gateway`
  annotations) must call into the *same* `Gateway` gRPC service gateway
  already runs — enforced by code review, not by tooling today; a
  follow-up (not yet built) would be a lint/CI check that `internal/
  httpapi` imports only the generated `Gateway` client, never
  `internal/bridge` or `internal/card` directly.
- **Explicit TODO, not yet done**: `sdk/go/ration`, `sdk/rust/
  ration-client`, and the REST facade itself (`internal/httpapi` or
  `grpc-gateway` annotations on `gateway.proto`) do not exist in the repo
  yet. This ADR records the decision taken before implementation so the
  facade, when built, has a settled design to build against rather than
  improvising the gRPC-vs-REST tradeoff mid-implementation.
- Every new RPC (e.g. a future `webhook-ingestion` or `card-issuance`
  split per section 1 / ADR 0004) must be added to `proto/` first; a REST
  endpoint or SDK method with no corresponding `.proto` RPC is a review
  rejection, not a style nit.
