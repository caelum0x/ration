# ADR 0002: Why a hand-rolled WAL instead of SQLite's WAL mode

## Status

Accepted. `services/wal` is a from-scratch C write-ahead log; SQLite is
still used (`rusqlite::Connection` in `services/ledger-core/src/ledger.rs`)
but only as a queryable index, with `PRAGMA journal_mode=WAL` enabled on
that connection for SQLite's own crash safety — the two are independent
systems, not one WAL doing double duty.

## Context

`ledger-core` needs a durability story where a crash can never lose or
double-apply a ledger event. SQLite already ships a battle-tested WAL
mode (`PRAGMA journal_mode=WAL`) that gives exactly that guarantee for
SQLite's own tables. The question this ADR answers: why does
`services/wal` exist as a separate, hand-rolled 8-byte-header/CRC32/
fsync-per-frame C library (`wal.c`, `frame.h`, `crc32.c`) instead of
just relying on SQLite's WAL mode as the system's single durability
mechanism?

## Decision

Treat the hand-rolled WAL as the durability **source of truth** and
SQLite as a **rebuildable, queryable index over it** — this split is
stated explicitly in `ledger.rs`'s module doc comment ("the WAL is the
durability source of truth, SQLite underneath is a rebuildable,
queryable index over it") and in `ARCHITECTURE.md`. Concretely:

- `Ledger::open` always runs WAL recovery (`replay_wal_into_index`)
  *before* opening the WAL for further appends and *before* accepting
  any RPC — so the SQLite index is always rebuilt from the WAL on boot,
  never assumed to already be consistent.
- Every mutating operation goes through `append_and_commit`: append to
  the WAL (fsync, durable) **first**, then apply to SQLite inside a
  transaction. If the process dies between those two steps, the next
  boot's replay re-derives the SQLite state from the WAL frame that was
  already durable — covered by the idempotent-on-replay pattern in
  `insert_ledger_entry_if_new` ("Idempotent: INSERT OR IGNORE on
  idempotency_key means a frame we've already indexed ... does not get
  double-applied to the balance either").
- The WAL's on-disk format is deliberately minimal and fully specified
  in one small header (`frame.h`): an 8-byte `length`+`crc32` header
  (explicit little-endian encode/decode functions, not a packed C
  struct, so the format "never depends on compiler struct layout,
  alignment, or host byte order") followed by the opaque payload.
  `wal_recover` treats a torn tail (partial header, partial payload, or
  a payload whose CRC doesn't match) as an expected, non-fatal
  condition: truncate and continue, not "corruption, abort."

Reasons this is a hand-rolled format rather than SQLite's own WAL mode
being the single mechanism:

1. **The invariant lives in `ledger-core`'s Rust code, not in SQL.**
   `apply_entry`'s reserve-floor check and the two-phase
   decide-then-durably-log-then-index pattern are the actual safety
   mechanism (ADR 0001). A WAL whose frame format is "whatever
   `serde_json` produces for a `WalEvent` enum" (see the `WalEvent`
   definition in `ledger.rs`) lets the durability layer be exactly as
   expressive as the domain model, including compound events like
   `DepositReceivedWithCredit` that must be replayed as a single atomic
   unit ("These must never be two separate frames: if the process
   crashed between them, a deposit could be marked received without
   ever crediting the ledger"). SQLite's WAL mode durably logs *SQL
   statement effects*; it has no concept of "this deposit-received event
   and this credit are one domain-level unit," so that atomicity would
   have to be re-derived from SQLite transaction boundaries anyway —
   at which point the application still owns the real event log, SQLite
   WAL mode or not.
2. **A project that showcases a ledger needs the WAL itself to be
   inspectable.** This is the project's explicit "low-level showcase
   piece" (`ARCHITECTURE.md`): a from-scratch, auditable frame format
   with its own fuzz target (`services/wal/fuzz/fuzz_frame_parse.c`,
   section 5) and crash-recovery test (`services/wal/tests/test_wal.c`).
   SQLite's WAL implementation is correct but opaque — you cannot point
   at eight bytes of it in a code review and explain exactly what they
   mean, which is the entire point of ADR-worthy infrastructure in this
   repo.
3. **Decoupling the durability format from the query engine.** Because
   the WAL's payload is domain events, not SQL, the queryable index
   underneath it is a replaceable implementation detail (today SQLite
   via `rusqlite`; `Ledger::open_with_memory_db` already swaps it for an
   in-memory `Connection` in tests with zero change to WAL semantics).
   If SQLite's WAL mode were the durability mechanism, the index and
   the durability guarantee would be the same artifact, and swapping the
   index engine would mean re-deriving the durability story from
   scratch.

## Alternatives considered

- **SQLite WAL mode as the only durability mechanism** (no separate
  `services/wal`). Rejected per the reasons above — correctness would
  still require ledger-core to encode domain-level atomicity on top of
  SQL transactions, at which point the hand-rolled event log is doing
  less, not more, work, while losing the auditability and fuzzing
  surface of an explicit frame format.
- **An existing embedded WAL crate** (e.g. a Rust-native log crate).
  Rejected for the same showcase reason as above, plus: the project's
  FFI boundary (Rust calling into a small C library, isolated behind
  `wal_ffi.rs`) is itself a deliberate demonstration of safe FFI
  discipline (ADR 0001) that a pure-Rust dependency wouldn't exercise.

## Consequences

- Two independent crash-recovery paths exist in the stack (the C WAL's
  `wal_recover` truncating a torn tail, and SQLite's own WAL-mode
  journal for its B-tree pages) — this is intentional layering, not
  duplication: a bug in one does not corrupt the other, and
  `services/wal`'s recovery is what `RUNBOOK.md` points at first for any
  ledger-core startup failure, because it is what the system's actual
  invariant depends on.
- `services/wal` carries its own test depth independent of
  `ledger-core`'s: `tests/test_wal.c` (append/recover/torn-write),
  `fuzz/fuzz_frame_parse.c` (malformed-input fuzzing), and
  `bench/bench_wal.c` (append throughput) — all exercised by `make test`
  before the Rust layer is even built.
