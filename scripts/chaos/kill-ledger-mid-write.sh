#!/usr/bin/env bash
# kill-ledger-mid-write.sh — the chaos test from docs/SCALE-EXPANSION.md
# section 5 that actually proves the C WAL + Rust recovery logic holds
# under real process failure, not just inside a unit test's idealized
# crash simulation.
#
# What it does, for real:
#   1. Builds the ledger-core release binary and the chaosload tool
#      (services/gateway/cmd/chaosload).
#   2. Starts ledger-core as a real background process against a fresh
#      WAL/SQLite pair in a scratch directory, on a dedicated port
#      (127.0.0.1:7099) so it never collides with a dev server on the
#      default 7001.
#   3. Creates a tenant and funds it, then launches `chaosload load` —
#      8 concurrent goroutines hammering ApplyEntry for the configured
#      duration.
#   4. Concurrently, a supervisor loop SIGKILLs the running ledger-core
#      process and restarts it (same WAL/DB paths, so WAL recovery runs
#      on every restart) at random 1-3s intervals throughout that same
#      window.
#   5. Once the load generator finishes, waits for ledger-core to be up
#      (post its final WAL recovery) and runs `chaosload verify`, which
#      cross-checks every op the load generator believed happened
#      against ListEntries/GetBalance.
#
# Usage: scripts/chaos/kill-ledger-mid-write.sh [duration_seconds]
#   (default 30s — long enough to reliably get multiple kill/restart
#   cycles mid-load, short enough to run in CI)
#
# Exit codes:
#   0 — verify found zero invariant violations across however many
#       kill/restart cycles actually happened. This is the pass.
#   1 — verify found at least one violation (lost entry, double-applied
#       entry, reserve-floor breach, or a GetBalance/ListEntries
#       reconciliation mismatch). This is a REAL finding, not a harness
#       bug — see the printed violation list and $WORKDIR/*.log.
#   2 — setup/harness failure (build failed, ledger-core never came up,
#       etc.) — inconclusive, not itself evidence of a WAL/recovery bug.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DURATION="${1:-30}"
WORKDIR="$(mktemp -d /tmp/ration-chaos.XXXXXX)"
WAL_PATH="$WORKDIR/ledger.wal"
DB_PATH="$WORKDIR/ledger.db"
ADDR="127.0.0.1:7099"
LEDGER_BIN="$ROOT/services/ledger-core/target/release/ledger-core"
LOADER_BIN="$WORKDIR/chaosload"
LOG_FILE="$WORKDIR/ops.jsonl"
LEDGER_LOG="$WORKDIR/ledger-core.log"
KILL_LOG="$WORKDIR/kills.log"
RESERVE_FLOOR=10000 # $100.00 minor units

LEDGER_PID=""
SUPERVISOR_PID=""

cleanup() {
  if [[ -n "$SUPERVISOR_PID" ]] && kill -0 "$SUPERVISOR_PID" 2>/dev/null; then
    kill "$SUPERVISOR_PID" 2>/dev/null || true
    wait "$SUPERVISOR_PID" 2>/dev/null || true
  fi
  if [[ -n "$LEDGER_PID" ]] && kill -0 "$LEDGER_PID" 2>/dev/null; then
    kill -9 "$LEDGER_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "chaos: workdir=$WORKDIR addr=$ADDR duration=${DURATION}s"

echo "chaos: building ledger-core release binary, wal lib, and chaosload tool..."
make -C "$ROOT" build-wal >"$WORKDIR/build-wal.log" 2>&1
(cd "$ROOT/services/ledger-core" && cargo build --release) >"$WORKDIR/build-ledger.log" 2>&1
(cd "$ROOT/services/gateway" && go build -o "$LOADER_BIN" ./cmd/chaosload) >"$WORKDIR/build-loader.log" 2>&1

if [[ ! -x "$LEDGER_BIN" ]]; then
  echo "chaos: FAIL (setup) ledger-core binary missing at $LEDGER_BIN — see $WORKDIR/build-ledger.log" >&2
  exit 2
fi
if [[ ! -x "$LOADER_BIN" ]]; then
  echo "chaos: FAIL (setup) chaosload binary failed to build — see $WORKDIR/build-loader.log" >&2
  exit 2
fi

start_ledger() {
  RATION_LEDGER_WAL_PATH="$WAL_PATH" RATION_LEDGER_DB_PATH="$DB_PATH" LEDGER_CORE_ADDR="$ADDR" \
    "$LEDGER_BIN" >>"$LEDGER_LOG" 2>&1 &
  LEDGER_PID=$!
}

wait_for_ready() {
  for _ in $(seq 1 75); do
    if "$LOADER_BIN" ping --addr "$ADDR" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

start_ledger
if ! wait_for_ready; then
  echo "chaos: FAIL (setup) ledger-core never became ready — see $LEDGER_LOG" >&2
  exit 2
fi
echo "chaos: ledger-core up, pid=$LEDGER_PID"

TENANT_ID="$("$LOADER_BIN" setup --addr "$ADDR" --reserve-floor "$RESERVE_FLOOR" --fund 10000000)"
if [[ -z "$TENANT_ID" ]]; then
  echo "chaos: FAIL (setup) chaosload setup did not return a tenant id" >&2
  exit 2
fi
echo "chaos: tenant=$TENANT_ID reserve_floor=$RESERVE_FLOOR"

# Supervisor: randomly SIGKILL + restart ledger-core throughout the load
# window. Runs as its own detached background loop so its timing is
# fully decoupled from the load generator — it does not know or care
# whether chaosload is mid-request when it kills the process, which is
# exactly the point.
(
  END=$(($(date +%s) + DURATION))
  KILLS=0
  while [[ $(date +%s) -lt $END ]]; do
    SLEEP=$(((RANDOM % 3) + 1)) # 1-3s between kills
    sleep "$SLEEP"
    if [[ $(date +%s) -ge $END ]]; then
      break
    fi
    if [[ -n "$LEDGER_PID" ]] && kill -0 "$LEDGER_PID" 2>/dev/null; then
      kill -9 "$LEDGER_PID" 2>/dev/null || true
      KILLS=$((KILLS + 1))
      echo "$(date +%s) kill #$KILLS pid=$LEDGER_PID" >>"$KILL_LOG"
      sleep 0.2
      start_ledger
      echo "$(date +%s) restarted pid=$LEDGER_PID" >>"$KILL_LOG"
    fi
  done
  echo "$KILLS" >"$WORKDIR/kill-count"
) &
SUPERVISOR_PID=$!

echo "chaos: running ApplyEntry load for ${DURATION}s against $ADDR (ledger-core pid will churn)..."
set +e
"$LOADER_BIN" load --addr "$ADDR" --tenant "$TENANT_ID" --duration "${DURATION}s" --concurrency 8 --log "$LOG_FILE"
LOAD_STATUS=$?
set -e

wait "$SUPERVISOR_PID" 2>/dev/null || true
SUPERVISOR_PID=""
KILLS=$(cat "$WORKDIR/kill-count" 2>/dev/null || echo 0)
echo "chaos: load generator exit=$LOAD_STATUS; ledger-core was killed+restarted $KILLS time(s)"

# Make sure ledger-core is up (and has finished its final WAL recovery)
# before verifying.
if ! kill -0 "$LEDGER_PID" 2>/dev/null; then
  start_ledger
fi
if ! wait_for_ready; then
  echo "chaos: FAIL (setup) ledger-core did not come back up for the verify pass — see $LEDGER_LOG" >&2
  exit 2
fi

echo "chaos: verifying via ListEntries/GetBalance..."
set +e
"$LOADER_BIN" verify --addr "$ADDR" --tenant "$TENANT_ID" --log "$LOG_FILE" --reserve-floor "$RESERVE_FLOOR"
VERIFY_STATUS=$?
set -e

if [[ "$VERIFY_STATUS" -eq 0 ]]; then
  echo "chaos: PASS — no lost, double-applied, or reserve-floor-breaching entries across $KILLS kill/restart cycle(s)"
else
  echo "chaos: FAIL — invariant violation detected above" >&2
fi
echo "chaos: artifacts kept at $WORKDIR (ledger-core.log, kills.log, ops.jsonl)"
exit "$VERIFY_STATUS"
