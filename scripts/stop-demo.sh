#!/usr/bin/env bash
# Stops every process started by scripts/run-demo.sh, using the PIDs it
# recorded in .ration-demo.pid. Safe to run even if some (or all) of those
# processes already exited on their own.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_FILE="$ROOT/.ration-demo.pid"

if [ ! -f "$PID_FILE" ]; then
  echo "stop-demo: no $PID_FILE found — nothing to stop."
  exit 0
fi

while read -r name pid; do
  [ -n "${pid:-}" ] || continue
  if kill -0 "$pid" 2>/dev/null; then
    echo "stopping $name (pid $pid)"
    kill "$pid" 2>/dev/null || true
  else
    echo "$name (pid $pid) already stopped"
  fi
done < "$PID_FILE"

rm -f "$PID_FILE"
echo "done."
