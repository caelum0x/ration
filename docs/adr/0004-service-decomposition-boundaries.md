# ADR 0004: Service decomposition boundaries (gateway splits into 4)

## Status

Accepted as the target decomposition (`docs/SCALE-EXPANSION.md` section
1). Not yet executed in code as of this writing: `services/gateway` is
still the single Go monolith described below, and this ADR records the
boundaries along which it will be cut so the split is a mechanical
extraction of already-separated packages, not a redesign.

## Context

`services/gateway` today is one deployable Go binary
(`cmd/gateway/main.go`) whose `internal/` already has real seams, because
its packages were written with single responsibilities from the start:

- `internal/tenant` — tenant CRUD, "delegates persistence to ledger-core"
  (`ARCHITECTURE.md`).
- `internal/card` + `internal/bridge` — card issuance and the
  rationing/bridge-funding math (`bridge.Service.RationBridgeFunding`,
  the highest-value business logic in the whole system per
  `services/gateway/internal/bridge/service.go`'s own doc comment).
- `internal/airwallex` (webhook signature verification) +
  `internal/httpserver` (the `/webhooks/airwallex` HTTP listener) —
  receiving and verifying Airwallex webhook deliveries, turning them into
  `MarkDepositReceived` calls.
- `internal/grpcserver` — the `Gateway` gRPC service implementation
  (`gateway.proto`) that the TUI and CLI actually dial.
- `internal/activity` — the fan-out event bus feeding `StreamActivity`.
- `internal/ledgerclient`, `internal/config`, `internal/idgen`,
  `internal/ratelimit` — cross-cutting support used by more than one of
  the above.

The problem one binary creates at scale: card issuance (expected to be
the system's highest-traffic, horizontally-scaled path — "give it its
own horizontal scaling story," section 1) is deployed, restarted, and
rate-limited as one unit with tenant CRUD (low traffic, rarely changes)
and webhook ingestion (bursty, externally triggered, needs to be safe to
retry independently of everything else).

## Decision

Split along the package boundaries that already exist, one new Go
module (its own `go.mod`, its own gRPC server) per bullet, with the
internal proto surfaces this split requires added to `proto/` alongside
the existing `common.proto`, `ledger.proto`, `gateway.proto`,
`audit.proto`:

- **`services/auth`** (new `proto/auth.proto`) — tenant API-key
  issuance/verification, JWT minting for the TUI/CLI, and mTLS client
  cert issuance for service-to-service calls. This is new code, not an
  extraction — today nothing in `services/gateway` issues
  per-service mTLS certs; `infra/dev-ca/` and `pkg/mtls` (section 8)
  generate and wire TLS *credentials*, but a dedicated issuance service
  is what lets a compromised downstream service's cert be revoked
  without redeploying every other service.
- **`services/tenant-registry`** (new `proto/tenant_registry.proto`) —
  a direct extraction of `internal/tenant/service.go`. Deliberately a
  "thin service, but a real separate deployable/scalable unit, not a
  package" (section 1): it owns nothing ledger-core doesn't already own
  — every write still proxies to `ledger-core`'s `LedgerCore.CreateTenant`
  / `GetTenant` / `ListTenants` RPCs, exactly as `internal/tenant` does
  today. It is split out anyway so tenant CRUD's (low, bursty-on-onboarding)
  traffic and deploy cadence never forces a redeploy of card-issuance.
- **`services/card-issuance`** (new `proto/card_issuance.proto`) — a
  direct extraction of `internal/card` + `internal/bridge`: Airwallex
  issuing calls and the rationing math. Named "the highest-value service"
  in the spec because `bridge.Service.RationBridgeFunding` is where real
  money decisions happen; its extraction target is explicitly "stateless,
  idempotent, safe to run N replicas behind a queue" — already true of
  the current code (`RationBridgeFunding`'s own doc comment: "correctness
  for a single tenant under concurrent calls ultimately rests on
  ledger-core's idempotent, reserve-floor-checked ApplyEntry ..., not on
  any lock in this package"), so this split changes deployment topology,
  not business logic.
- **`services/webhook-ingestion`** (new `proto/webhook_ingestion.proto`)
  — a direct extraction of `internal/airwallex/webhook.go` (signature
  verification) + `internal/httpserver/webhooks.go` (the HTTP listener),
  publishing normalized events onto a simple internal event bus (the Go
  channel bus `internal/activity/stream.go` already implements) rather
  than calling `MarkDepositReceived` inline — isolating webhook delivery
  spikes from the services actually indexing them. The spec is explicit
  that this internal bus is a placeholder: "document where a real
  deployment would swap in SQS/Kafka" (noted here; the swap itself is
  future work, not done).
- **`services/gateway`** becomes a thin BFF/aggregator implementing
  `gateway.proto` by fanning out gRPC calls to the four services above —
  this is what the TUI and CLI keep talking to, with the external
  surface (and therefore `internal/ratelimit`, per section 8's "in front
  of every public-facing RPC") staying here, not duplicated into each
  internal service. `internal/grpcserver/server.go` is the part of
  today's codebase that survives essentially unchanged in shape, just
  rewired to call four gRPC clients instead of four in-process packages.

`internal/ledgerclient` and `internal/idgen` are shared support, not
owned by one service: each extracted service gets its own copy of the
thin `ledgerclient` wrapper (matching the existing pattern where
`services/agent` already has its own independent
`internal/ledgerclient`, not a shared import of gateway's) and `idgen`,
since the spec calls for separate `go.mod`s per service and this project
does not yet have a shared internal Go module for cross-service
utilities.

## Alternatives considered

- **Split along a different axis** (e.g. by data entity — "cards
  service," "deposits service" — rather than by capability). Rejected:
  `bridge.Service` already spans cards, deposits, *and* ledger entries
  in a single rationing decision; splitting by entity would force that
  one decision to make three cross-service calls instead of one, adding
  latency and partial-failure surface to the highest-value code path for
  no benefit.
- **Extract card-issuance only, leave the rest in gateway.** Rejected:
  it solves card-issuance's scaling story but still couples
  webhook-ingestion's bursty, externally-triggered traffic to
  tenant-registry's deploy cadence, and auth (new capability) would have
  nowhere principled to live.
- **Keep one monolith, scale it horizontally as-is.** Rejected per the
  spec's framing — "true service decomposition" (not just more replicas
  of the same binary) is explicitly what distinguishes this phase from
  "hackathon scaffolding," and a single binary cannot give card-issuance
  an independent scaling policy from tenant-registry's.

## Consequences

- Sections 1 and 7 (SDKs/REST facade, ADR 0003) touch the same gRPC
  surface and are explicitly called out in `SCALE-EXPANSION.md`'s
  execution notes as **not** safe to run fully in parallel with each
  other — the facade's `internal/httpapi` must be written against
  whatever `gateway.proto` looks like after the BFF rewiring, not
  against the pre-split monolith's implicit internal calls.
- Every new internal RPC this split introduces (`auth.proto`,
  `tenant_registry.proto`, `card_issuance.proto`,
  `webhook_ingestion.proto`) is internal-only; `gateway.proto` remains
  the one public aggregate surface external callers (TUI, CLI, the
  future REST facade) ever see — unchanged externally, per the spec.
- Until this split lands, `RUNBOOK.md`'s diagnosis paths for gateway
  issues point at `services/gateway/internal/<package>`, not at a
  separate deployable — this ADR is the record of where those paths move
  once the split is executed.
