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
