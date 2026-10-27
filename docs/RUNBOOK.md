# Runbook

On-call style: symptom first, then a diagnosis path that names exact
files, log lines, and commands in this repo — not generic advice. If a
step below doesn't resolve it, the next escalation is always "read the
cited source file," because every path here traces back to a specific
function.

Every service logs structured JSON (`log/slog` in Go, `tracing` in
Rust) and, once `pkg/telemetry`'s correlation-ID propagation is wired
into a given call path, every log line for one CLI command / webhook /
agent tick carries the same `x-ration-correlation-id`
(`pkg/telemetry/correlation.go`) — `grep` logs for that ID across
services' stdout to follow one request end to end before reaching for a
tracing backend.

---

## "ledger-core won't start"

**Symptom:** `./services/ledger-core/target/release/ledger-core` (built
by `make build-ledger`, invoked by `scripts/run-demo.sh` as
`$LEDGER_BIN`) exits immediately, or `scripts/run-demo.sh` reports it
never became ready to accept connections.

**Diagnosis path, in order:**

1. **Check how far startup got.** `main.rs`'s `run()` logs, in order:
   `"opening ledger (recovering WAL before accepting RPCs)"` →
   `Ledger::open` → `"WAL recovery complete"` (with `replayed_events`) →
   `"LedgerCore gRPC server listening"`. Whichever log line is the last
   one printed tells you which step failed:
   - **Never logs "opening ledger"**: the process didn't even get past
     env/arg parsing — check that `services/ledger-core/target/release/
     ledger-core` is the binary actually being run (not a stale build;
     `make build-ledger` rebuilds it) and that stdout isn't being
     swallowed (`scripts/run-demo.sh` redirects each service's
     stdout/stderr to `data/logs/`).
   - **Logs "opening ledger" but never "WAL recovery complete"**: the
     failure is inside `Ledger::open` (`services/ledger-core/src/
     ledger.rs`) — go to step 2.
   - **Logs "WAL recovery complete" but never "LedgerCore gRPC server
     listening"**: the failure is `resolve_bind_addr` or `tonic::
     transport::Server::builder()...serve(addr)` — go to step 4.

2. **A `RATION_LEDGER_WAL_PATH` or `RATION_LEDGER_DB_PATH` directory
   problem.** `Ledger::open` creates `db_path`'s parent directory
   (`std::fs::create_dir_all(parent)`) but does **not** create
   `wal_path`'s parent directory — `wal_ffi::Wal::open` just calls C's
   `wal_open`, which does a plain `open(path, O_RDWR|O_CREAT, 0644)`
   (`services/wal/wal.c`) and fails with `WAL_ERR_IO` if that directory
   doesn't exist. In the default config both paths are under the same
   `./data/` dir (`.env.example`: `RATION_LEDGER_WAL_PATH=./data/
   ledger.wal`, `RATION_LEDGER_DB_PATH=./data/ledger.db`), so this only
   bites if you've pointed the two paths at different directories and
   only created one of them. Fix: `mkdir -p "$(dirname
   "$RATION_LEDGER_WAL_PATH")"` before starting the process. (This is a
   real, currently-undocumented gap in `Ledger::open`, not a
   configuration mistake on your part — worth a follow-up patch to make
   `Ledger::open` create both parents, tracked here rather than silently
   worked around.)

3. **A corrupt-beyond-recovery WAL.** `wal_recover` (`services/wal/
   wal.c`) already treats a torn tail (partial header/payload, or a
   payload whose CRC doesn't match) as expected and non-fatal — it
   truncates and keeps going, so this is NOT your problem if the last
   shutdown was a crash. What `replay_wal_into_index`
   (`services/ledger-core/src/ledger.rs`) surfaces as a real, fatal
   error is a **well-formed, CRC-valid frame whose payload isn't valid
   JSON for the current `WalEvent` enum** — logged by `main.rs`'s `Err`
   path as `"ledger-core exited with error"` with a message starting
   `"corrupt WAL event payload: ..."`. This happens if the WAL file was
   hand-edited, or if a previous deploy wrote frames with an older/newer
   `WalEvent` shape than the binary now running expects (a schema
   migration hazard: `WalEvent`'s variants are never versioned on disk
   today). There is no automatic recovery from this — the fix is a
   manual decision (restore a backed-up WAL per
   `infra/terraform/modules/ledger-backup/`, or, for a dev-only
   database, delete `RATION_LEDGER_WAL_PATH`/`RATION_LEDGER_DB_PATH` and
   accept the data loss). Never routinely delete a WAL as a first step —
   it is the durability source of truth (ADR 0002).

4. **Port/address problem.** `resolve_bind_addr` in `main.rs` resolves
   `LEDGER_CORE_ADDR` (default `localhost:7001`, `.env.example`) via the
   system resolver and fails fast with `"LEDGER_CORE_ADDR {addr} did not
   resolve to any address"` if the host part doesn't resolve at all. If
   it resolves but the process still fails to bind, something else
   already owns the port — `lsof -iTCP:7001 -sTCP:LISTEN` (adjust the
   port to your `LEDGER_CORE_ADDR`), most commonly a previous
   `ledger-core` process `scripts/stop-demo.sh` didn't clean up (check
   `.ration-demo.pid`).

5. **It never finished building.** `make build-ledger` depends on
   `build-wal` (`$(MAKE) -C services/wal`, producing `libwal.a`) before
   `cargo build --release` — if `libwal.a` is stale or missing,
   `ledger-core`'s `build.rs` (which compiles `services/wal` with `cc`)
   is the thing to re-run: `cd services/ledger-core && cargo clean &&
   cargo build --release` forces a fresh link.

---

## "I want to enable encryption at rest" / "ledger-core won't decrypt its own data after a restart"

**What `RATION_LEDGER_ENCRYPTION_KEY` actually encrypts:** one base64-
encoded 32-byte AES-256 key (`services/ledger-core/src/wal_crypto.rs`),
used for two independent things once set:

1. Every frame `ledger-core` appends to its own write-ahead log
   (`crate::ledger`'s `append_and_commit`/`replay_wal_into_index`) —
   the durable source of truth.
2. The free-text columns in its SQLite index that aren't already
   covered by `tenant_id`/amounts/timestamps/enum filters:
   `deposit.external_ref`, `ledger_entries.reference`/`memo`, and
   `agent_decisions.trigger`/`rationale`/`policy_snapshot_json`
   (`crate::deposit`, `crate::agent_decision`, the `ledger_entries`
   functions in `crate::ledger`, all going through `wal_crypto::
   encrypt_opt`/`decrypt_opt`). `tenant_id`, ids, amounts, timestamps,
   and status/type enums are deliberately left unencrypted — every
   query in the crate filters or sorts on one of those, and AES-GCM
   ciphertext can't support that.

**Enabling this is NOT a live migration.** Set
`RATION_LEDGER_ENCRYPTION_KEY` only against a **fresh**
`RATION_LEDGER_WAL_PATH`/`RATION_LEDGER_DB_PATH` pair (or a pair that
was always written with this exact key). Flipping it on against an
existing plaintext WAL/db, or changing the key against an existing
encrypted one, makes `Ledger::open_with_encryption` fail recovery
loudly (`wal_crypto::Error::Decrypt`/`Truncated`,
`LedgerError::Storage`/`LedgerError::Wal`) rather than silently
misreading or corrupting data — that is the intended, safe failure
mode, not a bug to work around. To actually rotate the key: stop
`ledger-core`, start a brand-new WAL/db path under the new key, and
replay/migrate application-level (there is no in-place re-encrypt
tool for either the WAL or the SQLite columns as of this writing).

Generate a key: `openssl rand -base64 32` (must decode to exactly 32
bytes — `wal_crypto::Error::WrongKeyLength` rejects anything else at
startup, immediately, not on first write).

Disabled by default (env var unset) — every other security control in
this project uses the same opt-in posture for anything that changes an
on-disk format. See `infra/terraform/modules/service` for this
project's existing Secrets Manager wiring pattern a real deployment
would use to provision this value instead of a bare env var.

---

## "a tenant's reserve floor shows negative headroom"

**Symptom:** `ration status --tenant <id>` (or the TUI's tenants tab)
shows a headroom value below zero, or `bridge.Service.RationBridgeFunding`
logs/`AgentDecision` entries show `headroom_minor_units: 0` on every call
even when you'd expect the tenant to have room.

**This should be structurally impossible for `ledger-core`'s own
`check_reserve_floor`** (`services/ledger-core/src/ledger.rs`): it
computes `headroom = balance.available_minor_units -
t.reserve_floor_minor_units` and returns it as-is, so a genuinely
negative number IS a real signal that `available_minor_units` has
dropped below `reserve_floor_minor_units` — which `apply_entry` is
supposed to prevent for every DEBIT/HOLD by rejecting the entry (as
`ApplyOutcome::ReserveFloorViolated`) *before* writing it. If you are
seeing this, work the diagnosis in this order:

1. **Confirm it's real, not a display artifact.** `bridge.Service.
   RationBridgeFunding` (`services/card-issuance/internal/bridge/service.go`)
   deliberately clamps negative headroom to zero before using it
   (`if headroom < 0 { snap.ReserveFloorBreached = true; headroom = 0
   }`) and reports that state as `reserve_floor_breached` in the
   recorded `AgentDecision`'s policy snapshot JSON — query
   `ledger-core`'s `ListAgentDecisions` for that tenant (or check the
   TUI's activity feed) and look at the `headroom_minor_units` /
   `reserve_floor_breached` fields directly rather than trusting a UI
   that might itself clamp or mis-render a negative value.
2. **Call `ledger-core` directly, bypassing gateway, to rule out a
   proxy bug.** `check_reserve_floor(tenant_id, 0)` (via a raw gRPC call
   to `LedgerCore`, or `ledger-core`'s own test harness) returns the
   unclamped `(allowed, headroom)` pair straight from SQLite's
   `balances` table — if gateway and ledger-core disagree, the bug is in
   gateway's proxying (`internal/ledgerclient`), not in the ledger.
3. **If `ledger-core` itself reports negative headroom, suspect the WAL
   replay path, not a live race.** `apply_entry`'s in-memory check runs
   under `Ledger.inner`'s mutex for its *entire* decide-append-commit
   duration (`append_and_commit`), so a live concurrent-call race that
   produces a floor breach should be structurally impossible — the
   realistic cause is **WAL replay reapplying events that already
   happened**, which `insert_ledger_entry_if_new`'s idempotency guard
   (unique `idempotency_key`) is supposed to prevent. Check `docs/
   formal/ledger.tla` section 6's safety property — if it's been
   re-run against a model that doesn't match the current `apply_entry`
   code (e.g. after a change to `WalEvent` or `apply_entry` itself
   without updating `ledger_model.tla`), the spec no longer actually
   covers what's running in production; re-run TLC (`docs/formal/
   README.md`) against the current logic as the first real check here,
   not as an afterthought.
4. **Check for a tenant whose `reserve_floor_minor_units` was set
   negative.** `create_tenant` rejects `reserve_floor_minor_units < 0`
   at creation time, but if that validation was ever bypassed (direct
   SQLite write, a WAL frame written by tooling other than
   `Ledger::create_tenant`), headroom math downstream has no further
   defense — `SELECT reserve_floor_minor_units FROM tenants WHERE id =
   ?` against the ledger-core SQLite index file
   (`RATION_LEDGER_DB_PATH`) directly, read-only, to confirm.

---

## "the agent loop is granting $0 bridges"

**Symptom:** `services/agent`'s polling loop
(`internal/loop/runner.go`) runs, calls `RationBridgeFunding` for a
tenant with a genuinely late deposit, but every grant comes back
`GrantedMinorUnits: 0` / `FullyGranted: false`.

**This is very often correct behavior, not a bug** — `bridge.Service.
RationBridgeFunding` (`services/card-issuance/internal/bridge/service.go`) has
exactly four reasons to decline, and it always says which one in the
`Explanation` string it returns and records via `RecordAgentDecision`
(`declineExplanation`'s four branches). Read the explanation string
first; it names the exact cause:

1. **`"reserve floor breached"`** — `headroom <= 0`. See the previous
   runbook entry; this is the reserve-floor scenario surfacing through
   the bridge path specifically.
2. **`"tenant already has an unrepaid bridge grant within the last
   24h (velocity cap)"`** — `velocityBlocked` found a `bridge:`-prefixed
   HOLD entry (`bridgeReferencePrefix`) within `RATION_BRIDGE_VELOCITY_
   WINDOW_HOURS` (default 24h, `.env.example`) with no matching RELEASE.
   Check `ListEntries` for that tenant for an open `bridge:` HOLD; if
   it's actually been repaid, confirm the RELEASE entry's `Reference`
   field matches the HOLD's `Reference` exactly — `velocityBlocked`
   matches on exact string equality (`released[ref]`), so any drift in
   how the reference string is constructed (e.g. a manual RELEASE
   that doesn't copy the original HOLD's `reference`) will falsely keep
   a tenant velocity-blocked forever.
3. **`"no late deposit on file for this tenant, nothing to bridge
   against"`** despite the deposit genuinely being overdue — **FIXED
   2026-10-23.** `findRelevantLateDeposit`
   (`services/card-issuance/internal/bridge/service.go`) used to filter
   strictly on the deposit's stored `status` field
   (`d.GetStatus() != DepositStatus_DEPOSIT_STATUS_LATE`), but nothing
   on `ledger-core`'s write path (`deposit::insert` sets
   `DepositStatus::Expected`; `deposit::mark_received` only ever writes
   `DepositStatus::Received` — see `services/ledger-core/src/
   deposit.rs`) ever wrote `DepositStatus::Late` to a row, so that
   filter could never match a single real deposit: `RationBridgeFunding`
   declined every genuinely late deposit with exactly this message,
   every time, against a real ledger-core. Masked by every unit test
   that exercised this path (`bridge/service_test.go`,
   `grpcserver/server_test.go`, `agent/internal/loop/runner_test.go`),
   which all hand-constructed fake `Deposit{Status:
   DEPOSIT_STATUS_LATE}` fixtures directly — fixtures that also
   happened to satisfy the real, intended definition (past
   `expected_at_unix`, zero `received_at_unix`), so the bug was
   invisible to the green test suite.

   `findRelevantLateDeposit` now computes lateness the same time-based
   way `ledger-core`'s own `list_late` does —
   `ReceivedAtUnix == 0 && ExpectedAtUnix < s.now()` — instead of
   trusting the `status` field at all. No proto/wire change needed; the
   existing test suite (including the fixtures above, which already
   satisfied this definition) passes unchanged. If you still see this
   decline for a deposit that is unambiguously overdue, check
   `expected_at_unix`/`received_at_unix` on the raw `ListDeposits`
   response directly — that is now exactly what this code checks too.
4. **`"computed grant of $X is below the $5 minimum-useful-grant
   floor"`** — `granted < MinGrantMinorUnits` (default 500 = $5.00,
   `RATION_BRIDGE_MIN_GRANT_MINOR_UNITS`). The explanation string
   itself reports `headroom`, `bridge cap` (60% of the expected late
   deposit, `RATION_BRIDGE_MAX_FRACTION`), and the expected deposit
   amount — if headroom or the bridge cap is unexpectedly tiny, go back
   to the tenant's balance/reserve-floor state (previous entry) or to
   whether the deposit's `expected_minor_units` is actually what you
   think it is (`ListDeposits`).

**If the explanation string itself is missing or empty** (not one of
the four above), that is a real bug, not expected behavior — check that
`s.ledger.RecordAgentDecision` isn't failing silently (`recordDecision`
returns its error up through `RationBridgeFunding`, so a failure here
should surface as an error from the RPC, not a silent `$0`) and that
`internal/llm/tools.go`'s tool-call wrapper around `RationBridgeFunding`
is actually propagating the `Explanation` field back to whatever
surfaced the "$0" symptom (CLI output, TUI activity feed) rather than
dropping it.

Also check `ration_bridge_grants_total{result="declined"}` (`pkg/metrics/
metrics.go`, section 2's named metric) against `{result="granted"}` +
`{result="partial"}` over time — a sudden step-change to all-declined
across *every* tenant (not just one) points at a policy or config
regression (e.g. `RATION_BRIDGE_MAX_FRACTION` accidentally set to `0`),
not a per-tenant data issue.

## "audit-log's mirror might be silently falling behind ledger-core"

**Symptom:** no specific error — this is a *preventive* check, not a
reactive one. `services/ledger-core/src/audit_mirror.rs`'s mirroring of
every `ApplyEntry`/`RecordAgentDecision` outcome into `audit-log` is
explicitly best-effort and asynchronous (never allowed to block or fail
the original RPC); a network blip, `audit-log` being down, or a
serialization bug there fails *silently* from the caller's perspective
— `audit_mirror_spawn_append_failures_total` (`pkg/metrics`,
docs/THREAT-MODEL.md's "Known gaps" #3) increments, but nothing
compares that against what actually landed. `VerifyChain`/`verify-chain`
(above) only proves the chain is internally consistent — it says
nothing about whether every record that *should* be there actually
*is*.

**Check:** run `ration audit-check` (`services/cli/cmd/ration/
audit_check.go`). For every tenant (or just `--tenant <id>`), it counts
`ledger-core`'s own `ListEntries`/`ListAgentDecisions` records and
`audit-log`'s own mirrored `ListRecords` count of each kind for that
same tenant, fully paginated on both sides, and reports any tenant/kind
pair whose counts diverge by more than `--tolerance` (default 2 — the
two counts are two independent, non-atomic RPC round-trips, so a
record landing on `ledger-core` between the two reads is expected
noise, not a real gap). Exits 0 when everything matches; exits 1 and
prints every divergent pair otherwise, so this is safe to run from a
monitoring cron job, not just by hand:

```
ration audit-check                       # every tenant
ration audit-check --tenant <tenant-id>   # one tenant
ration audit-check --tolerance 0          # strict, no slack for in-flight writes
```

A real, sustained divergence (not a one-off blip that clears on the
next run) means the mirror has a real, ongoing gap — check
`audit_mirror_spawn_append_failures_total` first (a nonzero, growing
counter directly explains it), then whether `AUDIT_LOG_ADDR` is even
configured on the `ledger-core` instance in question (unset disables
mirroring entirely, by design — see `AuditMirror::disabled`), then
`audit-log`'s own process health/logs for the window the gap opened in.
