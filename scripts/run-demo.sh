#!/usr/bin/env bash
# Starts the Ration demo stack: ledger-core (Rust, release binary), the
# four section-1 split services (auth, tenant-registry, card-issuance in
# AIRWALLEX_MODE=mock, webhook-ingestion), the gateway BFF fronting them
# (gateway.proto, unchanged externally — docs/SCALE-EXPANSION.md section
# 1, docs/adr/0004), and the agent's polling loop (Go) as background
# processes, waits for every gRPC-listening service to be accepting
# connections on its port, seeds demo tenants via `ration seed`, and
# prints instructions for watching it live in `./bin/tui`.
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
AUTH_GRPC_ADDR="${AUTH_GRPC_ADDR:-localhost:7010}"
TENANT_REGISTRY_GRPC_ADDR="${TENANT_REGISTRY_GRPC_ADDR:-localhost:7011}"
CARD_ISSUANCE_GRPC_ADDR="${CARD_ISSUANCE_GRPC_ADDR:-localhost:7012}"
WEBHOOK_INGESTION_GRPC_ADDR="${WEBHOOK_INGESTION_GRPC_ADDR:-localhost:7013}"
GATEWAY_GRPC_ADDR="${GATEWAY_GRPC_ADDR:-localhost:7002}"
# audit-log (docs/SCALE-EXPANSION.md section 8) was wired into
# docker-compose.yml and every Go/Rust client's env config, but never
# into this bare-metal demo path — meaning the headline "audit trail is
# the point" claim (README, docs/SUBMISSION.md) and the standalone
# verify-chain tool (services/audit-log/cmd/verify-chain,
# docs/THREAT-MODEL.md known-gap 9) were never actually exercised by
# `make run-demo`, the exact path judges follow. Started here so
# ledger-core's AuditMirror (services/ledger-core/src/audit_mirror.rs)
# has somewhere real to mirror every ApplyEntry/RecordAgentDecision to.
AUDIT_LOG_GRPC_ADDR="${AUDIT_LOG_GRPC_ADDR:-localhost:7003}"
AUDIT_LOG_ADDR="${AUDIT_LOG_ADDR:-$AUDIT_LOG_GRPC_ADDR}"
RATION_AUDIT_LOG_DATA_DIR="${RATION_AUDIT_LOG_DATA_DIR:-$ROOT/data}"
CARD_ISSUANCE_ADDR="${CARD_ISSUANCE_ADDR:-$CARD_ISSUANCE_GRPC_ADDR}"
WEBHOOK_INGESTION_ADDR="${WEBHOOK_INGESTION_ADDR:-$WEBHOOK_INGESTION_GRPC_ADDR}"
AIRWALLEX_MODE="mock"
# auth has no insecure default (docs/SCALE-EXPANSION.md section 1, see
# services/auth/internal/config's doc comment) — the demo isn't a real
# deployment, but AUTH_JWT_SIGNING_KEY is still a secret in shape, so
# generate a fresh one per run rather than committing a fixed demo value
# anyone reading this script could reuse.
AUTH_JWT_SIGNING_KEY="${AUTH_JWT_SIGNING_KEY:-$(head -c 32 /dev/urandom | base64 | tr -d '\n')}"
export LEDGER_CORE_ADDR AUTH_GRPC_ADDR TENANT_REGISTRY_GRPC_ADDR CARD_ISSUANCE_GRPC_ADDR \
  WEBHOOK_INGESTION_GRPC_ADDR GATEWAY_GRPC_ADDR AUDIT_LOG_GRPC_ADDR AUDIT_LOG_ADDR \
  RATION_AUDIT_LOG_DATA_DIR CARD_ISSUANCE_ADDR WEBHOOK_INGESTION_ADDR \
  AIRWALLEX_MODE AUTH_JWT_SIGNING_KEY

LEDGER_BIN="$ROOT/services/ledger-core/target/release/ledger-core"
AUTH_BIN="$ROOT/bin/auth"
TENANT_REGISTRY_BIN="$ROOT/bin/tenant-registry"
CARD_ISSUANCE_BIN="$ROOT/bin/card-issuance"
WEBHOOK_INGESTION_BIN="$ROOT/bin/webhook-ingestion"
GATEWAY_BIN="$ROOT/bin/gateway"
AGENT_BIN="$ROOT/bin/agent"
RATION_BIN="$ROOT/bin/ration"
AUDIT_LOG_BIN="$ROOT/bin/audit-log"
VERIFY_CHAIN_BIN="$ROOT/bin/verify-chain"

for bin in "$LEDGER_BIN" "$AUTH_BIN" "$TENANT_REGISTRY_BIN" "$CARD_ISSUANCE_BIN" \
  "$WEBHOOK_INGESTION_BIN" "$GATEWAY_BIN" "$AGENT_BIN" "$RATION_BIN" \
  "$AUDIT_LOG_BIN" "$VERIFY_CHAIN_BIN"; do
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
echo "  audit-log         -> $AUDIT_LOG_GRPC_ADDR"
echo "  ledger-core       -> $LEDGER_CORE_ADDR"
echo "  auth              -> $AUTH_GRPC_ADDR"
echo "  tenant-registry   -> $TENANT_REGISTRY_GRPC_ADDR"
echo "  card-issuance     -> $CARD_ISSUANCE_GRPC_ADDR"
echo "  webhook-ingestion -> $WEBHOOK_INGESTION_GRPC_ADDR"
echo "  gateway           -> $GATEWAY_GRPC_ADDR"
echo

trap cleanup_on_failure ERR

# audit-log starts first so ledger-core's AuditMirror (connected lazily,
# but there's no reason to race it) has a live target as soon as it
# dials AUDIT_LOG_ADDR below.
start_bg "audit-log" "$LOG_DIR/audit-log.log" "$AUDIT_LOG_BIN"
wait_for_port "$AUDIT_LOG_GRPC_ADDR" "audit-log"

start_bg "ledger-core" "$LOG_DIR/ledger-core.log" "$LEDGER_BIN"
wait_for_port "$LEDGER_CORE_ADDR" "ledger-core"

# auth has no ledger-core dependency (docs/SCALE-EXPANSION.md section 1)
# so it can start as soon as ledger-core's port check above passes —
# ordering here is "ledger-core first" for the other three services'
# sake, not auth's.
start_bg "auth" "$LOG_DIR/auth.log" "$AUTH_BIN"
wait_for_port "$AUTH_GRPC_ADDR" "auth"

start_bg "tenant-registry" "$LOG_DIR/tenant-registry.log" "$TENANT_REGISTRY_BIN"
wait_for_port "$TENANT_REGISTRY_GRPC_ADDR" "tenant-registry"

start_bg "card-issuance" "$LOG_DIR/card-issuance.log" "$CARD_ISSUANCE_BIN"
wait_for_port "$CARD_ISSUANCE_GRPC_ADDR" "card-issuance"

start_bg "webhook-ingestion" "$LOG_DIR/webhook-ingestion.log" "$WEBHOOK_INGESTION_BIN"
wait_for_port "$WEBHOOK_INGESTION_GRPC_ADDR" "webhook-ingestion"

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
echo "  4. Independently verify the tamper-evident audit trail every money"
echo "     move and agent decision above was just mirrored into, with no"
echo "     dependency on the audit-log process being trustworthy:"
echo "       ./bin/verify-chain $RATION_AUDIT_LOG_DATA_DIR/audit.log"
echo "     re-derives the hash chain straight from the append-only log file"
echo "     and exits 0 printing 'intact' with the record count, or exits 1"
echo "     naming the first broken sequence."
echo
echo "Logs: $LOG_DIR/{audit-log,ledger-core,auth,tenant-registry,card-issuance,webhook-ingestion,gateway,agent}.log"
echo "Stop everything with: scripts/stop-demo.sh"
