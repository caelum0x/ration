#!/usr/bin/env bash
# One-off helper for this verification pass only (not committed as part
# of the feature) — starts each service individually with inter-service
# delays, the same workaround rounds 6/7's build logs already documented
# for this shared box's sibling-agent load (transient
# "mint operator JWT: DeadlineExceeded" under contention), instead of
# scripts/run-demo.sh's single invocation.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
set -a
source .env
set +a

# .env deliberately leaves AUDIT_LOG_ADDR empty (it's meant to be derived
# from AUDIT_LOG_GRPC_ADDR, same convention CARD_ISSUANCE_ADDR/
# WEBHOOK_INGESTION_ADDR already use) and run-demo.sh already applies this
# exact fallback (`AUDIT_LOG_ADDR="${AUDIT_LOG_ADDR:-$AUDIT_LOG_GRPC_ADDR}"`)
# before launching ledger-core. This script's plain `source .env` never did,
# so ledger-core here always saw AUDIT_LOG_ADDR="" (explicitly set, not
# merely unset) and silently disabled its own AuditMirror on every run
# started this way — the headline "audit trail is the point" claim
# (README, docs/SUBMISSION.md) was never actually exercised by this path.
# Reproduced live: `ration audit-check` showed every tenant's real ledger
# entries against 0 audit entries before this fix.
export AUDIT_LOG_ADDR="${AUDIT_LOG_ADDR:-$AUDIT_LOG_GRPC_ADDR}"

LOG_DIR="$ROOT/data/logs"
mkdir -p "$LOG_DIR"
PID_FILE="$ROOT/.ration-demo.pid"
: > "$PID_FILE"

start() {
  local name="$1" bin="$2"
  shift 2
  "$bin" "$@" >"$LOG_DIR/$name.log" 2>&1 &
  echo "$name $!" >> "$PID_FILE"
  echo "started $name pid $!"
  sleep 3
}

start audit-log "$ROOT/bin/audit-log"
start ledger-core "$ROOT/services/ledger-core/target/release/ledger-core"
start auth "$ROOT/bin/auth"
start tenant-registry "$ROOT/bin/tenant-registry"
start card-issuance "$ROOT/bin/card-issuance"
start webhook-ingestion "$ROOT/bin/webhook-ingestion"
start gateway "$ROOT/bin/gateway"
start agent "$ROOT/bin/agent"

echo "all started, pids:"
cat "$PID_FILE"
