#!/usr/bin/env bash
# Drives docs/DEMO-SCRIPT.md's exact sequence end to end, narrated with
# short pauses so it reads well as a recording (asciinema, or just a
# terminal someone is watching live) — this is the script behind the
# hackathon submission's demo video. No LLM credentials, no Airwallex
# sandbox credentials, no Docker: everything here runs against
# scripts/run-demo.sh's real bare-metal stack in AIRWALLEX_MODE=mock.
#
# Usage:
#   make build                        # once, or whenever source changes
#   asciinema rec demo.cast -c scripts/record-demo.sh
#   agg demo.cast demo.gif            # or any .cast -> video pipeline
#
# Safe to just run directly too (no asciinema) to watch/verify the exact
# sequence before recording for real.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

say() {
  echo
  echo "# $*"
  sleep 1.5
}

pause() { sleep "${1:-2}"; }

./scripts/stop-demo.sh >/dev/null 2>&1 || true
rm -rf data .ration-demo.pid

say "Ration — Airwallex Agentic Banking Hackathon, Starter Kit 5 (Platform Spend Controller)"
pause 1
say "Starting the real stack: 8 services (Rust ledger-core + 7 Go services), AIRWALLEX_MODE=mock, no credentials needed."
pause 1

./scripts/run-demo.sh | tee /tmp/ration-record-demo-seed.out
# Specifically the medium (EUR) demo tenant, not just the first one
# seeded: its headroom (50,000.00 EUR) comfortably exceeds the 60%
# bridge cap against the 40,000.00 EUR late-deposit amount below, so the
# first grant is a genuine PARTIAL grant (reserve floor untouched) and
# the second request is declined specifically by the 24h velocity cap —
# the exact scenario this recording (and docs/DEMO-SCRIPT.md) narrates.
# The small demo tenant's headroom is smaller than that cap, which
# produces a "no headroom available" decline instead — a real, correct,
# but different hard-rule path than the one being demonstrated here.
TENANT_ID="$(grep "Northwind Freight Marketplace" /tmp/ration-record-demo-seed.out | grep -oE '[0-9a-f-]{36}' | head -1)"
if [ -z "$TENANT_ID" ]; then
  echo "record-demo: could not find the Northwind demo tenant id in run-demo.sh output" >&2
  exit 1
fi

say "Tenant: $TENANT_ID — a platform with real reserve-floor-protected capital already funded."
./bin/ration status --tenant "$TENANT_ID"
pause 3

say "A deposit this tenant was expecting is now late. Forcing that scenario:"
./bin/ration simulate-deposit --tenant "$TENANT_ID" --amount 4000000 --late
pause 2

say "Waiting for the agent's next poll to pick it up and compute a rationing decision..."
pause 11

say "ration status — the agent's decision, with a computed (not templated) rationale:"
./bin/ration status --tenant "$TENANT_ID"
pause 4

say "The same late deposit is still outstanding. Watching the 24h velocity cap engage on the next poll..."
pause 11

say "ration status again — a second grant is correctly DECLINED, no exception for the LLM:"
./bin/ration status --tenant "$TENANT_ID"
pause 4

say "Independently verifying the tamper-evident audit trail — re-derives the hash chain from the append-only log file on disk, no trust in the live audit-log process required:"
./bin/verify-chain ./data/audit.log
pause 2

say "Cross-checking ledger-core's own entry/decision counts against audit-log's mirror, per tenant:"
./bin/ration audit-check
pause 2

say "Done. Shutting the stack down."
./scripts/stop-demo.sh
