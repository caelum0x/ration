# Formal spec: `ledger.tla`

This directory contains a TLA+ spec of `services/ledger-core`'s core state
machine (`Ledger::apply_entry` in `services/ledger-core/src/ledger.rs`) and
a concrete model instance that TLC actually model-checks. Per
`docs/SCALE-EXPANSION.md` section 6, the two properties checked are:

1. **Safety**: `available_minor_units >= reserve_floor_minor_units` for
   every tenant, at every reachable state.
2. **Liveness**: every `HOLD` eventually `RELEASE`s or converts to a
   `DEBIT` ("captures").

**This was actually run, not just written.** TLC (`tla2tools.jar`,
TLC2 2.19) was installed and executed in this environment (OpenJDK 27 via
`brew install openjdk`, jar fetched from
`https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar`)
against the concrete model in `ledger_model.tla` / `ledger_model.cfg`. Full
real output below, not a transcription — command ran from `docs/formal/`:

```
$ java -cp tla2tools.jar tlc2.TLC -config ledger_model.cfg ledger_model.tla
...
Computing initial states...
Finished computing initial states: 1 distinct state generated at 2026-10-19 03:46:19.
Checking 2 branches of temporal properties for the current state space with 30566 total distinct states
Finished checking temporal properties in 00s
...
Checking 2 branches of temporal properties for the complete state space with 100000 total distinct states
Finished checking temporal properties in 02s
Model checking completed. No error has been found.
896001 states generated, 50000 distinct states found, 0 states left on queue.
The depth of the complete state graph search is 13.
```

`TypeOK`, `SafetyReserveFloor`, and `SafetyHeldNonNegative` were checked as
invariants on every one of the 50,000 distinct reachable states (896,001
generated, state space fully explored — "0 states left on queue"), and
`HoldsResolve` (the liveness property) was checked as a temporal property
and also passed ("No error has been found" covers both the invariants and
the property given in `PROPERTY`). This is an exhaustive check of the
*model's* bounded state space (2 tenants, 2 hold ids, amounts 0–3,
balances capped at 9 for finiteness — see "Why bounded" below), not a
statistical/simulation sample.

## How to run it yourself

```bash
# 1. A JRE is required. If you don't have one:
brew install openjdk   # or any JDK 11+

# 2. Fetch the TLA+ tools jar (≈2.2MB, no license key needed):
curl -L -o tla2tools.jar \
  https://github.com/tlaplus/tlaplus/releases/latest/download/tla2tools.jar

# 3. Run TLC from this directory:
cd docs/formal
java -cp tla2tools.jar tlc2.TLC -config ledger_model.cfg ledger_model.tla
```

Expect it to finish in well under a minute and print
`Model checking completed. No error has been found.` at the end. (The
`tla2tools.jar` is intentionally not vendored into the repo — it's a
~2.2MB third-party binary artifact; fetching it is a one-line `curl`.)

### Why there are two `.tla` files

TLC's `.cfg` grammar can assign simple constants (sets, numbers, model
values) directly, but `ReserveFloor` is a *function* (`Tenants -> Nat`),
and TLC's cfg syntax can't express a function literal over model values
directly in the `CONSTANTS` block (a literal like `[t1 |-> 2, t2 |-> 0]`
is parsed as a *record* whose domain is the string set `{"t1", "t2"}`,
not the model-value set `{t1, t2}` — this is a real, easy-to-hit TLA+
footgun, confirmed by hitting it directly while building this spec, see
"What actually got checked while building this" below).

The standard TLA+/TLC idiom for this is a small "MC" module that `EXTENDS`
the real spec and defines the function as an ordinary operator, which the
`.cfg` file then points the constant at via `<-`:

- `ledger.tla` — the actual spec. Everything meaningful (the action
  definitions, the invariant, the liveness property) lives here.
- `ledger_model.tla` — `EXTENDS ledger`, declares the model-value
  constants (`t1, t2, h1, h2`), and defines `ReserveFloorDef` as a real
  function over them.
- `ledger_model.cfg` — the TLC config: assigns `Tenants`, `HoldIds`,
  `MaxAmount`, `MaxBalance` directly, and `ReserveFloor <- ReserveFloorDef`.

## What the model checks, precisely

- **Two tenants**, `t1` (reserve floor 2) and `t2` (reserve floor 0) — one
  positive floor so the headroom-gated branch is actually exercised and
  can actually reject something, one zero floor matching
  `create_tenant`'s explicitly-allowed "no protected capital" case.
- **Two hold ids** (`h1`, `h2`) — enough for one hold to be `RELEASE`d
  while another is `CAPTURE`d in the same run, and for a third `HOLD`
  attempt (once both ids are used) to be exercised as a no-op.
- **Amounts bounded to `0..3`** and **balances bounded to `<= 9`**
  (`StateConstraint`) — purely to keep TLC's state space finite; see "Why
  bounded" below.

Every one of `CREDIT`, `DEBIT` (both the accepted and the
`ReserveFloorViolated`-no-op branch), `HOLD` (both branches), `RELEASE`,
and `CAPTURE` is reachable and was reached in the 50,000-state check —
confirmed directly: deleting the headroom check from a scratch copy of
`Debit` and re-running produces a counterexample in 2 steps (credit to
t1, then an unchecked debit pushes `available[t1]` from 2 to 1, below its
floor of 2); separately, deleting the `WF_vars(...)` fairness conjunct
from `Spec` and re-running produces a 5-step counterexample ending in
`Stuttering` with a hold (`h1`) permanently stuck `"pending"`. Both were
run while building this spec specifically to prove the checks are real
and not vacuous — see that transcript in this repo's session history; the
commands are reproducible by applying the same two edits described above
to a scratch copy and re-running the same `java -cp ...` command.

## Scope: what this spec models vs. what it doesn't

Modeled, 1:1 with `services/ledger-core/src/ledger.rs`:

- `CREDIT` — unconditional `available += amt` (no floor check needed:
  increasing available can't breach a lower-bound invariant).
- `DEBIT` — `available -= amt` gated on
  `amt <= available - reserve_floor`; on failure, `ApplyOutcome::`
  `ReserveFloorViolated` leaves the balance **unchanged**, modeled here as
  a same-state no-op transition, exactly like the Rust arm (not an error,
  not a block).
- `HOLD` — same headroom gate as `DEBIT`, but moves `amt` from `available`
  to `held` instead of extinguishing it.
- `RELEASE` — moves a pending hold's exact reserved amount back from
  `held` to `available`.

Deliberately **not** modeled (out of scope, already covered elsewhere):

- WAL/SQLite durability, crash recovery, and replay — covered by
  `Ledger::open`'s `replay_wal_into_index`, the `recovery.rs` integration
  tests, and the chaos script from `docs/SCALE-EXPANSION.md` section 5.
- Idempotency-key deduplication — `apply_entry` short-circuits on a
  repeated key before any of this logic runs; replaying the same logical
  entry twice is a no-op by construction.
- Multi-currency / currency-mismatch rejection — orthogonal to the balance
  arithmetic this spec checks.

## Two honesty notes — read before trusting a green run

**1. `CAPTURE` doesn't exist in `services/ledger-core` yet.** The real
RPC surface's only hold-resolution primitive today is `RELEASE` — see
`services/gateway/internal/card/service.go` and
`services/gateway/internal/bridge/service.go`, which place `HOLD` entries
backing card limits / bridge grants and resolve them only via `RELEASE`.
A real card-spend "capture" today would need a `RELEASE` followed by a
separate `DEBIT` call, not the single atomic action this spec's `Capture`
models. `Capture` is included here because `docs/SCALE-EXPANSION.md`
section 6 states the liveness property in terms of "RELEASEs or converts
to DEBIT" — this spec models that target state machine so the property is
meaningful, but **a passing TLC run is a check of the spec's logic, not
proof that the Rust code implements capture, because it doesn't yet.**
Precise TODO for whoever adds it: add a `LedgerEntryType::Capture` (or
equivalent) arm to `Ledger::apply_entry` in
`services/ledger-core/src/ledger.rs` that behaves exactly like this
module's `Capture` action (extinguish `held`, never return to
`available`); this spec needs no changes if it does.

**2. The liveness property's fairness assumption is NOT separately
verified anywhere else in this repo.** `HoldsResolve` holds *given* weak
fairness on `Release(h) \/ Capture(h)` for every hold id — i.e. given that
whichever caller placed a `HOLD` (card-issuance for limit holds, the
bridge service for bridge grants) always eventually issues the matching
`RELEASE`/capture call. `ledger-core` itself has no background process
that auto-resolves stale holds; nothing in this repo guarantees that
assumption holds operationally (a crashed or buggy caller could leak a
hold forever — ledger-core's transition relation would still happily
accept a late `RELEASE` if one ever arrived, but nothing forces one to
arrive). This was confirmed directly by removing the `WF_vars(...)`
conjunct and re-running: TLC finds a 5-step counterexample where a hold is
created and simply never resolved. If a real stale-hold timeout/sweep is
needed, it does not exist yet — precise TODO, not currently covered by
`services/agent` (`services/agent/internal/policy/rules.go` only computes
bridge envelopes and checks reserve floor; it never issues `HOLD` or
`RELEASE` calls today) or anywhere else.

## Why the model is bounded (and what that does/doesn't prove)

TLC needs a finite state space. `MaxAmount = 3` and the `StateConstraint`
capping `available`/`held` at `9` are model-checking artifacts, not
production limits — `Ledger::apply_entry`'s real arithmetic is `i64` with
explicit overflow checks (`checked_add`/`checked_sub` in
`services/ledger-core/src/money.rs`), unbounded in practice. This TLC run
proves the safety and liveness properties for every reachable state
*within these small bounds*; it does not prove them for arbitrary `i64`
magnitudes by itself. Two things narrow that gap:

- The invariant's truth doesn't depend on the magnitude of `amt` — the
  guard `amt <= available - reserve_floor` is scale-invariant (it holds or
  fails identically whether `amt` is `3` or `3_000_000`), so the small
  bound exercises the same control-flow branches a huge one would.
- `services/ledger-core/src/money.rs`'s own property-style unit tests
  (`checked_add_detects_overflow`, `checked_sub_detects_overflow`) cover
  the large-magnitude/overflow edge the TLA+ model abstracts away (this
  spec models `available`/`held` as unbounded mathematical `Int`, i.e. it
  assumes overflow never happens — overflow-safety is Rust's job via
  `checked_add`/`checked_sub`, not this spec's).
