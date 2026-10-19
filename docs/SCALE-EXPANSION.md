# Scale expansion — production/big-tech posture

**Standing rule for every phase of this project, not just this one:** build
this as production software a fintech unicorn would actually run real money
through, not as hackathon scaffolding. No stub functions that fake a return
value, no "good enough to demo" shortcuts, no TODOs standing in for the hard
part, no silent failure paths. The Airwallex hackathon is the submission
venue, not the engineering bar — the bar is "would this survive a real
production incident review at a company processing real payments." If a
piece can't be done to that standard in a given pass (e.g. a Terraform
`apply` needs real AWS credentials, a TLA+ model needs the invariant nailed
down first), say so explicitly and leave a precise, real TODO with the exact
blocker — never paper over it with fake-passing code.

Phase 2, run after the initial 6-service build (wal, ledger-core, gateway,
agent, tui, cli) is built, integrated, and verified end to end. Goal: take
Ration from six services to something that reads like it came out of a real
infra org at unicorn scale — true service decomposition, observability,
infra-as-code, formal verification of the one invariant that actually
matters (the reserve floor), and the test depth (fuzzing, property tests,
chaos) that backs a claim like "this ledger doesn't lose money."

## 1. Service decomposition (gateway splits into 4)

`services/gateway` currently does too much for one deployable unit. Split
into independently deployable Go services, each its own go.mod, each a gRPC
server, wired through the same proto contracts (extend proto/ with
`auth.proto`, `tenant_registry.proto`, `card_issuance.proto`,
`webhook_ingestion.proto` as needed — keep gateway.proto as the public
aggregate surface, these are internal):

- `services/auth` — tenant API-key issuance/verification, JWT minting for
  the TUI/CLI, mTLS client cert issuance for service-to-service calls.
- `services/tenant-registry` — tenant CRUD, proxies to ledger-core, owns
  nothing ledger-core doesn't already own (thin service, but a real
  separate deployable/scalable unit, not a package).
- `services/card-issuance` — everything under the old `internal/card` and
  `internal/bridge`: Airwallex issuing calls, the actual rationing math.
  This is the highest-value service; give it its own horizontal scaling
  story (stateless, idempotent, safe to run N replicas behind a queue).
- `services/webhook-ingestion` — receives Airwallex webhooks, verifies
  signatures, publishes normalized events (a simple internal event bus for
  now — document where a real deployment would swap in SQS/Kafka).
- `services/gateway` becomes a thin BFF/aggregator in front of the above
  four, implementing gateway.proto by fanning out — this is what the TUI/CLI
  actually talk to, unchanged externally.

## 2. Observability

- `pkg/telemetry` (Go) — OpenTelemetry SDK wiring: tracer provider, a
  `WithSpan` helper, gRPC client/server interceptors that propagate trace
  context across every service hop (auth → tenant-registry →
  card-issuance → ledger-core). Every agent decision and ledger entry must
  be traceable end to end from the CLI command that triggered it.
- `pkg/metrics` (Go) — Prometheus `/metrics` endpoint per service:
  request counts/latency histograms per RPC, a `ration_bridge_grants_total`
  counter, `ration_reserve_floor_headroom_minor_units` gauge per tenant.
- Rust `ledger-core`: `tracing` + `tracing-opentelemetry`, same trace
  propagation via gRPC metadata.
- `infra/otel-collector/config.yaml` + `infra/grafana/dashboards/*.json` —
  a real local observability stack (docker-compose: otel-collector,
  Prometheus, Grafana, Jaeger or Tempo) so the whole system's trace/metric
  story is runnable, not just described.
- Structured JSON logging everywhere (`log/slog` Go, `tracing` Rust) with a
  correlation ID threaded from the CLI command through every hop.

## 3. Infra as code (AWS)

- `infra/terraform/modules/service/` — a reusable ECS Fargate service
  module (task def, ALB target group, CloudWatch log group, autoscaling
  policy, Secrets Manager integration for ANTHROPIC_API_KEY/
  AIRWALLEX_API_KEY).
- `infra/terraform/envs/{dev,staging,prod}/main.tf` — wires the module per
  service (auth, tenant-registry, card-issuance, webhook-ingestion,
  gateway, agent), a shared VPC module, an ALB, Route53 (optional/commented
  since there's no real domain yet), Secrets Manager entries, an S3 bucket
  for ledger-core WAL backups (ledger-core itself stays on local/EBS disk
  for the WAL — S3 is for periodic snapshot backup, documented in
  `infra/terraform/modules/ledger-backup/`).
- `infra/terraform/README.md` — explicit cost note: this is a real,
  plannable Terraform config but `terraform apply` is an owner action
  (AWS credentials, real spend) — ship `terraform plan` as the verifiable
  artifact, do not apply.
- `Dockerfile` per service (multi-stage, distroless/scratch final image for
  the Go/Rust binaries), `docker-compose.yml` at repo root wiring all
  services + the observability stack for one-command full-stack local dev.

## 4. CI/CD

- `.github/workflows/ci.yml` — matrix job: C (make + test_wal under
  AddressSanitizer), Rust (cargo test, cargo clippy -D warnings, cargo
  audit), Go (go build/vet/test for all six+ modules, golangci-lint),
  proto lint (buf lint if available, else protoc --fatal_warnings).
- `.github/workflows/docker-build.yml` — builds + pushes each service's
  Dockerfile (to a local/placeholder registry path — real push target is
  an owner config value).
- `scripts/pre-commit.sh` + `.pre-commit-config.yaml` (or a Go/Rust-native
  equivalent) wired into `make lint`.

## 5. Test depth

- Rust: `proptest` suite for `ledger::apply_entry` — the property is "no
  sequence of concurrent ApplyEntry calls can ever push available balance
  below reserve_floor", generate random entry sequences and assert the
  invariant holds after every one.
- C: a libFuzzer harness (`services/wal/fuzz/fuzz_frame_parse.c`) fuzzing
  `wal_read_next`'s frame parser against malformed/truncated/bit-flipped
  input — this is the file most likely to have a real memory-safety bug,
  fuzz it for real.
  **Built and run on 2026-10-19** (see `services/wal/fuzz/Makefile`'s own
  comment for why it's intentionally not in `make test`): Apple's Command
  Line Tools clang doesn't bundle `libclang_rt.fuzzer_osx.a`, so
  `brew install llvm` was required first; the Makefile's `CC` auto-detect
  then picked up Homebrew's `llvm@20` clang with no manual `CC=` override
  needed. `make build` compiled clean with
  `-fsanitize=fuzzer,address,undefined`. `make smoke` (30s,
  `services/wal/fuzz/`) ran 216,303 executions at ~6,977 exec/s, 78 new
  corpus units discovered, 84 lines/414 features of coverage reached, zero
  crashes/leaks/ASan or UBSan findings — `wal_read_next()` correctly
  rejects every malformed/truncated/bit-flipped input it was fed as
  `WAL_ERR_CORRUPT`/`WAL_ERR_BUFFER_SMALL`/`WAL_ERR_EOF` rather than
  misbehaving. No bug found, so nothing to fix in `wal.c`.
- Go: `go test -fuzz` harness for `internal/policy/rules.go`'s envelope
  computation (random tenant balances/reserve floors/deposit amounts,
  assert the computed grant never exceeds headroom).
- Chaos test: `scripts/chaos/kill-ledger-mid-write.sh` — drives a load of
  ApplyEntry calls against ledger-core while SIGKILLing and restarting the
  process at random intervals, then verifies via `ListEntries` that no
  entry was lost or double-applied. This is the test that actually proves
  the C WAL + Rust recovery logic works under real failure, not just in a
  unit test's idealized crash simulation.

## 6. Formal spec (the "Amazon" touch)

- `docs/formal/ledger.tla` — a TLA+ spec of the ledger's core state
  machine: tenant balance, reserve floor, ApplyEntry as an action,
  checked with TLC for the safety invariant `balance >= reserve_floor` and
  liveness (every HOLD eventually RELEASEs or converts to DEBIT). Include
  `docs/formal/README.md` explaining how to run TLC and what it checked.
  This is deliberately the same verification discipline AWS is known for
  (they've published on using TLA+ for S3/DynamoDB) — a real, runnable
  spec, not a decorative one.

## 7. Client SDKs + REST facade

- `sdk/go/ration` — a thin typed Go client wrapping the generated gRPC
  stubs with ergonomic constructors (mirrors what a platform customer
  integrating with Ration's gateway would actually import).
- `sdk/rust/ration-client` — same, for Rust consumers.
- `services/gateway` gets a `grpc-gateway`-generated REST/JSON facade
  (reverse-proxy annotations in gateway.proto, or a hand-rolled
  `internal/httpapi` translating JSON <-> the gRPC client) so judges/devs
  without a gRPC client can `curl` it — document this clearly as "REST is
  a convenience facade over the gRPC source of truth," not a parallel
  implementation.

## 8. Security hardening

- mTLS between internal services: a dev-only self-signed CA under
  `infra/dev-ca/`, generation script, and gRPC server/client TLS creds
  wiring (`pkg/mtls`).
- `internal/ratelimit` (Go, token bucket per tenant) in front of every
  public-facing RPC (gateway's external surface).
- `services/audit-log` — every `RecordAgentDecision` and every `ApplyEntry`
  also gets mirrored (async, best-effort) into an append-only audit log
  service distinct from ledger-core's own WAL, so a compromised
  card-issuance service can't quietly rewrite its own history — the audit
  trail lives somewhere it doesn't control.
- `docs/THREAT-MODEL.md` — STRIDE-style pass over the service boundaries
  added in step 1: what an attacker at each trust boundary (TUI/CLI user,
  Airwallex webhook sender, a compromised service) can and can't do.

## 9. Docs that make this read like a real org shipped it

- `docs/adr/0001-why-rust-for-ledger-core.md`,
  `0002-why-a-hand-rolled-wal-instead-of-sqlite-wal-mode.md`,
  `0003-why-grpc-internal-rest-facade-external.md`,
  `0004-service-decomposition-boundaries.md` — short ADRs, each a real
  tradeoff discussion, not filler.
- `docs/RUNBOOK.md` — on-call style: "ledger-core won't start", "a
  tenant's reserve floor shows negative headroom", "the agent loop is
  granting $0 bridges", each with a diagnosis path citing exact files/logs.
- `docs/SLO.md` — stated latency/availability targets per service and
  how they'd be measured from the Prometheus metrics in step 2.

## Execution notes for whoever builds this phase

Each numbered section above is close to independently buildable once the
phase-1 services and proto contracts exist — treat each as a unit of work.
Sections 1 (service split) and 7 (SDKs/REST facade) touch the same gRPC
surface and should NOT run fully in parallel with each other; everything
else (2, 3, 4, 5, 6, 8, 9) is additive and safe to parallelize freely
against the phase-1 codebase.
