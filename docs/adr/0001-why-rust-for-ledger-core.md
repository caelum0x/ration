# ADR 0001: Why Rust for `ledger-core`

## Status

Accepted. `services/ledger-core` is implemented in Rust (`services/ledger-core/src/`).

## Context

`ledger-core` is the one component in this system where a bug is a money
bug: `Ledger::apply_entry` (`services/ledger-core/src/ledger.rs`) is "the
only way money moves," and it owns the single invariant the whole project
is built around — `available_minor_units >= reserve_floor_minor_units`
must hold for every tenant at every reachable state (checked for real by
`docs/formal/ledger.tla`, section 6).

That invariant is enforced in-process, in memory, before anything is
written to disk (`apply_entry`'s reserve-floor check runs before the WAL
append in `append_and_commit`). Two properties of the implementation
language matter more here than almost anywhere else in the codebase:

1. **No data race can corrupt the balance check.** `apply_entry` reads
   `current.available_minor_units`, decides, then writes a new balance —
   a classic check-then-act race if two calls for the same tenant ever
   ran concurrently without exclusion.
2. **No silent integer overflow can make a debit look smaller than it
   is.** Every arithmetic step in `apply_entry` is a `checked_add` /
   `checked_sub` that returns `MoneyError::Overflow` instead of wrapping
   (see `services/ledger-core/src/money.rs` and every arm of the
   `match entry_type` block in `apply_entry`).

## Decision

Write `ledger-core` in Rust, with `Ledger` wrapping all mutable state
(`wal: Wal`, `db: Connection`) inside a single `std::sync::Mutex`
(`LedgerInner`) and taking `&self` everywhere — the compiler enforces that
no code path can read or write ledger state without holding that lock,
and `Send`/`Sync` bounds on `Ledger` are checked at compile time, not
discovered under load. Overflow safety comes from Rust's `checked_*`
arithmetic methods, which make "what happens on overflow" a value in the
`Result` type the caller must handle, not a wraparound that silently
produces a wrong `balance_after_minor_units`.

The one place that genuinely needs to be unsafe — calling into the C WAL
library — is deliberately quarantined to a single module
(`services/ledger-core/src/wal_ffi.rs`), documented at the top of that
file: "This is the ONLY module in this crate that uses `unsafe` — every
other module talks to the write-ahead log exclusively through `Wal` and
`recover`." That module's `unsafe impl Send for Wal` comes with a written
safety argument (`Wal` owns its `wal_t*` exclusively, every operation
requires `&mut self`), and the `recover_trampoline` function carries a
`// SAFETY:` comment justifying the pointer-to-closure cast. The rest of
the remaining ~2,300 lines of Rust in this service (`agent_decision.rs`,
`deposit.rs`, `error.rs`, `grpc.rs`, `ledger.rs`, `lib.rs`, `main.rs`,
`money.rs`, `tenant.rs`) never touch a raw pointer.

## Alternatives considered

- **Go**, matching the rest of the Go services (`gateway`, `agent`,
  `tui`, `cli`). Rejected for this specific service: Go's garbage
  collector and lack of a borrow checker mean "two goroutines touching
  the same tenant's balance without a lock" is a runtime bug a linter
  might catch, not a compile error. Given this is also the service a
  TLA+ spec is written against (ADR context above), a language whose
  type system can state and check "this struct's invariants are only
  ever mutated under this lock" removes an entire class of findings a
  production-incident review would otherwise have to rule out by
  testing alone. Go remains the right choice for every service around
  `ledger-core` (gateway, agent, tui, cli) where the concurrency and
  overflow stakes are lower and consistency with the rest of the Go
  codebase matters more.
- **C or C++** for the whole ledger, not just the WAL. Rejected: manual
  memory management for the stateful, long-lived `Ledger`/`Connection`
  object graph is exactly the risk profile Rust's ownership model exists
  to remove, with none of the ergonomic cost paid by the WAL (which is a
  thin, mostly-stateless, single-struct C library — see ADR 0002).
- **JVM language (Kotlin/Java)**. Rejected: no `unsafe`-free path to a
  zero-copy FFI boundary with a hand-rolled C library as thin as
  `libwal.a`, and GC pause characteristics are a worse fit for an
  `fsync()`-per-append hot path than Rust's deterministic, no-GC
  ownership model.

## Consequences

- Every public method on `Ledger` is testable without mocking the lock
  (see the `#[cfg(test)] mod tests` block in `ledger.rs`: credit/debit,
  reserve-floor breach, idempotency) because the mutex is internal state,
  not something callers have to manage.
- `cargo clippy -- -D warnings` and `cargo test` (wired into
  `make test`, section 4 CI) are the enforcement mechanism for the "no
  unsafe outside `wal_ffi.rs`" rule — a `#![deny(unsafe_code)]`-style
  discipline enforced by code review today; a crate-level lint is a
  natural follow-up if a second FFI boundary is ever added.
- The cost is a second toolchain in the repo (`cargo` next to `go` and
  `make`/`gcc` for the C WAL) and a smaller hiring pool than Go — accepted
  given the invariant this service is responsible for.
