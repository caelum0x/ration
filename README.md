# Ration

Agentic multi-tenant spend controller for the Airwallex Agentic Banking
Hackathon — **Starter Kit 5: Platform Spend Controller** (Multi-Tenant
Platforms track).

> Issue customer cards and ration bridge funding when deposits arrive late.

No web dashboard. This is a systems project: a Rust ledger core backed by a
hand-rolled C write-ahead log, a Go gateway that owns the Airwallex
integration, a Go agent loop that makes the actual rationing decisions
(policy rules + an LLM for judgment calls), and a terminal UI for judges to
watch it work live.

**Engineering standard:** this is built to the bar of production fintech
infrastructure, not hackathon scaffolding — real crash-safety (see the WAL
recovery tests), a hard-rule money-safety invariant the LLM can never
override, and a scale-out roadmap (service decomposition, observability,
infra-as-code, formal verification, fuzzing/chaos testing — see
[docs/SCALE-EXPANSION.md](docs/SCALE-EXPANSION.md)) aimed at unicorn-scale
production use, not a weekend demo.

## Architecture

```
                    ┌─────────────────────┐
                    │   tui / cli (Go)     │  terminal dashboard + demo driver
                    └──────────┬───────────┘
                               │ gRPC
                    ┌──────────▼───────────┐       ┌────────────────────┐
  Airwallex API <──►│   gateway (Go)        │──────►│  agent (Go)         │
  (mock or live)    │  issuing/global accts │ gRPC  │  policy + LLM loop  │
                    │  webhooks, multi-tenant│◄──────│  tool-calling        │
                    └──────────┬───────────┘       └────────────────────┘
                               │ gRPC
                    ┌──────────▼───────────┐
                    │  ledger-core (Rust)   │  authoritative balances,
                    │  double-entry, reserve │  deposits, reserve floor
                    │  floor enforcement     │
                    └──────────┬───────────┘
                               │ FFI
                    ┌──────────▼───────────┐
                    │  wal (C)              │  crash-safe append log +
                    │  write-ahead log       │  recovery, fsync-correct
                    └───────────────────────┘
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design,
[docs/AGENT-DESIGN.md](docs/AGENT-DESIGN.md) for the agent/policy design,
and [docs/AIRWALLEX-SETUP.md](docs/AIRWALLEX-SETUP.md) for sandbox access
status.

## Services

| Service | Language | Role |
|---|---|---|
| `services/wal` | C | Crash-safe write-ahead log: append, fsync, recover. |
| `services/ledger-core` | Rust | Double-entry ledger, reserve-floor enforcement, deposit tracking. gRPC server over the C WAL. |
| `services/gateway` | Go | Airwallex Issuing/Global Accounts/webhooks client (mock + live), multi-tenant routing, fronts ledger-core. |
| `services/agent` | Go | Agentic loop: watches late deposits, decides card issuance / bridge rationing, calls an LLM for non-deterministic judgment calls, executes via gateway. |
| `services/tui` | Go (Bubble Tea) | Terminal dashboard: tenants, deposits, cards, live agent activity feed. |
| `services/cli` | Go | Operator CLI: seed demo tenants, simulate deposits, force late-deposit scenarios for judging. |
| `proto/` | Protobuf | Wire contracts shared by every service. |

## Quickstart

```bash
make proto          # generate Go stubs from proto/
make build           # build all services
make run-demo         # starts wal-backed ledger-core, gateway (mock Airwallex), agent, seeds 3 tenants
./bin/tui              # watch it live in a second terminal
```

## Airwallex mode

`AIRWALLEX_MODE=mock` (default) runs entirely against an in-process fake
that mirrors the real Issuing/Global Accounts/webhook contracts byte-for-byte
so flipping to `AIRWALLEX_MODE=live` with real sandbox credentials is a
one-line env change, not a rewrite. Sandbox platform access is pending
organizer approval — see [docs/AIRWALLEX-SETUP.md](docs/AIRWALLEX-SETUP.md).
