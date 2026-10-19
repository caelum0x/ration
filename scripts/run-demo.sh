#!/usr/bin/env bash
# Starts the Ration demo stack: ledger-core (Rust, release binary),
# gateway (Go, AIRWALLEX_MODE=mock), and the agent's polling loop (Go) as
# background processes, waits for ledger-core/gateway to be accepting
# connections on their gRPC ports, seeds demo tenants via `ration seed`,
# and prints instructions for watching it live in `./bin/tui`.
#
# Run `make build` first. Stop everything with scripts/stop-demo.sh (or
# `kill $(cat .ration-demo.pid | awk '{print $2}')`) when you're done.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# --- load .env (falling back to .env.example's defaults) --------------------
# Values already set in the environment win; everything else comes from
# .env if present, else .env.example, matching every service's own
# envOrDefault("KEY", default) fallback behavior.
load_env_file() {
  local file="$1"
  [ -f "$file" ] || return 0
  while IFS='=' read -r key value; do
    case "$key" in
      ''|'#'*) continue ;;
    esac
    # Already set in the real environment (e.g. exported by the caller)
    # takes priority over the file.
    if [ -z "${!key:-}" ]; then
      export "$key=$value"
    fi
  done < "$file"
}

load_env_file "$ROOT/.env"
load_env_file "$ROOT/.env.example"

LEDGER_CORE_ADDR="${LEDGER_CORE_ADDR:-localhost:7001}"
GATEWAY_GRPC_ADDR="${GATEWAY_GRPC_ADDR:-localhost:7002}"
AIRWALLEX_MODE="mock"
export LEDGER_CORE_ADDR GATEWAY_GRPC_ADDR AIRWALLEX_MODE

LEDGER_BIN="$ROOT/services/ledger-core/target/release/ledger-core"
GATEWAY_BIN="$ROOT/bin/gateway"
AGENT_BIN="$ROOT/bin/agent"
RATION_BIN="$ROOT/bin/ration"

for bin in "$LEDGER_BIN" "$GATEWAY_BIN" "$AGENT_BIN" "$RATION_BIN"; do
  if [ ! -x "$bin" ]; then
    echo "run-demo: missing binary $bin — run 'make build' first." >&2
    exit 1
  fi
done

PID_FILE="$ROOT/.ration-demo.pid"
LOG_DIR="$ROOT/data/logs"
mkdir -p "$LOG_DIR"
: > "$PID_FILE"

cleanup_on_failure() {
  echo "run-demo: startup failed, stopping anything already running..." >&2
  while read -r _name pid; do
    kill "$pid" 2>/dev/null || true
  done < "$PID_FILE"
}

start_bg() {
  local name="$1" logfile="$2"
  shift 2
  "$@" >"$logfile" 2>&1 &
  local pid=$!
  echo "$name $pid" >> "$PID_FILE"
  echo "  started $name (pid $pid), logging to $logfile"
}

# --- wait_for_port: block until something is accepting TCP connections on
# host:port, or fail after a timeout. Used instead of a fixed sleep so the
# demo doesn't flake on a slow machine and doesn't hang forever on a
# genuinely dead process. -------------------------------------------------
wait_for_port() {
  local hostport="$1" name="$2" timeout_secs="${3:-15}"
  local host="${hostport%:*}" port="${hostport##*:}"
  local waited=0
  while ! (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; do
    exec 3>&- 2>/dev/null || true
    sleep 0.5
    waited=$((waited + 1))
    if [ "$waited" -ge $((timeout_secs * 2)) ]; then
      echo "run-demo: timed out after ${timeout_secs}s waiting for $name on $hostport" >&2
      return 1
    fi
  done
  exec 3>&- 2>/dev/null || true
  echo "  $name is accepting connections on $hostport (after ${waited}00ms)"
}

echo "Ration demo — starting services (AIRWALLEX_MODE=$AIRWALLEX_MODE)"
echo "  ledger-core -> $LEDGER_CORE_ADDR"
echo "  gateway     -> $GATEWAY_GRPC_ADDR"
echo

trap cleanup_on_failure ERR

start_bg "ledger-core" "$LOG_DIR/ledger-core.log" "$LEDGER_BIN"
wait_for_port "$LEDGER_CORE_ADDR" "ledger-core"

start_bg "gateway" "$LOG_DIR/gateway.log" "$GATEWAY_BIN"
wait_for_port "$GATEWAY_GRPC_ADDR" "gateway"

start_bg "agent" "$LOG_DIR/agent.log" "$AGENT_BIN"
# The agent has no listening port of its own (it's a pure poll loop against
# ledger-core/gateway) — give it a moment to finish its first tick's log
# lines before we move on, instead of a hard port check.
sleep 1

trap - ERR

echo
echo "Seeding demo tenants..."
"$RATION_BIN" seed

echo
echo "All services are up. PIDs recorded in $PID_FILE:"
cat "$PID_FILE"
echo
echo "Next steps:"
echo "  1. In a second terminal, run:  ./bin/tui"
echo "     (reads LEDGER_CORE_ADDR/GATEWAY_GRPC_ADDR from the environment,"
echo "      same defaults as above if you don't export anything.)"
echo "  2. Trigger a late deposit for one of the seeded tenants, e.g.:"
echo "       ./bin/ration simulate-deposit --tenant <tenant-id> --amount 50000 --late"
echo "  3. Wait for the agent's next poll (RATION_AGENT_POLL_INTERVAL_SECONDS,"
echo "     default 10s) then check:"
echo "       ./bin/ration status --tenant <tenant-id>"
echo "     for a recorded agent decision, or watch it land live in ./bin/tui's"
echo "     activity feed."
echo
echo "Logs: $LOG_DIR/{ledger-core,gateway,agent}.log"
echo "Stop everything with: scripts/stop-demo.sh"
