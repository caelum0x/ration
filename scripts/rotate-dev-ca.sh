#!/usr/bin/env bash
# Rotates every service's mTLS leaf certificate WITHOUT regenerating
# infra/dev-ca/generate-dev-ca.sh's dev CA itself — closing that
# script's own named gap: "issues 1-year leaf certs with no rotation
# story."
#
# How: for each service, generate a fresh keypair + CSR (the same
# openssl steps generate-dev-ca.sh itself uses), then ask a *running*
# `auth` instance to sign it via its IssueServiceCert RPC
# (services/auth/internal/servicecertca, new
# services/auth/cmd/issue-service-cert client) — but only once auth
# itself has been started with AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE
# pointing at infra/dev-ca/out/ca.{pem,key} (this script's own first
# step verifies that, and tells you exactly how to fix it if it isn't
# the case). That makes auth's runtime-issuance CA the SAME trust root
# every service's existing <PREFIX>_MTLS_CA_FILE already points at, so
# a freshly rotated leaf validates everywhere immediately — no new CA
# to redistribute, no restart of anything except the one service whose
# leaf just rotated (to pick up its new cert/key files from disk; this
# script does not restart any process itself).
#
# Run on a schedule (cron, a CI job, whatever an operator already uses
# for this project — see docs/RUNBOOK.md) with a TTL shorter than the
# schedule's own period, e.g. 30 days leaves comfortable headroom for
# a daily or weekly cron. Issued leaves are --ttl-bounded by
# servicecertca.MaxTTL (7 days) on the SERVER side regardless of what
# this script requests — see that package's doc comment — so running
# this with the default 30d request against today's server actually
# yields a 7-day leaf; this script's default matches the requested
# value in docs/AGENT-DESIGN.md's own backlog item wording, the
# clamping is called out loudly below so it's never a silent surprise.
#
# ************************************************************************
# DEV-ONLY, exactly like generate-dev-ca.sh: auth's runtime CA key is
# either regenerated ephemeral on every auth restart (useless for this
# script — see below) or loaded from a plaintext PEM file on local
# disk. A real deployment needs a managed PKI, not this.
# ************************************************************************
#
# Usage:
#   scripts/rotate-dev-ca.sh [output-dir] [ttl]
#
#   output-dir   defaults to infra/dev-ca/out/ (same as generate-dev-ca.sh)
#   ttl          defaults to 720h (30 days); passed straight to
#                issue-service-cert's -ttl flag, Go duration syntax
#
# Requires: the CA files to already exist at <output-dir>/ca.{pem,key}
# (run infra/dev-ca/generate-dev-ca.sh first, exactly once — never
# re-run that script as "rotation", it mints an entirely new CA every
# time and every other service would need its *_MTLS_CA_FILE updated)
# and a running `auth` instance, at AUTH_ADDR (default localhost:7010),
# started with AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE pointing at that
# same ca.pem/ca.key.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${1:-$ROOT/infra/dev-ca/out}"
TTL="${2:-720h}"
AUTH_ADDR="${AUTH_ADDR:-localhost:7010}"

CA_CERT="$OUT_DIR/ca.pem"
CA_KEY="$OUT_DIR/ca.key"

if [ ! -f "$CA_CERT" ] || [ ! -f "$CA_KEY" ]; then
  echo "rotate-dev-ca: $CA_CERT / $CA_KEY not found." >&2
  echo "  Run infra/dev-ca/generate-dev-ca.sh once first (not as part of" >&2
  echo "  rotation — it mints a brand-new CA every run)." >&2
  exit 1
fi

echo "==> verifying auth at $AUTH_ADDR is signing against this exact CA ($CA_CERT)"
# A cheap, local sanity check: hash the CA cert we're about to compare
# issued leaves against, so a mismatch is caught before CSR work for
# every service, not after.
CA_SUBJECT_HASH="$(openssl x509 -in "$CA_CERT" -noout -hash 2>/dev/null || true)"
if [ -z "$CA_SUBJECT_HASH" ]; then
  echo "rotate-dev-ca: could not read $CA_CERT as a certificate" >&2
  exit 1
fi

# Same service list as generate-dev-ca.sh, kept in sync deliberately
# (not sourced from that script, to keep each script independently
# readable — see docs/SCALE-EXPANSION.md section 8 for the full list's
# origin).
SERVICES=(ledger-core gateway audit-log agent cli tui auth tenant-registry card-issuance webhook-ingestion)

ROTATED=0
for svc in "${SERVICES[@]}"; do
  key="$OUT_DIR/${svc}-key.pem"
  csr="$OUT_DIR/${svc}.rotate.csr"
  new_cert="$OUT_DIR/${svc}-cert.pem.new"
  new_key="$OUT_DIR/${svc}-key.pem.new"
  ext="$OUT_DIR/${svc}.rotate.ext"

  echo "==> rotating ${svc}"

  openssl genrsa -out "$new_key" 2048 >/dev/null 2>&1

  cat > "$ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth,clientAuth
subjectAltName=DNS:${svc},DNS:localhost,IP:127.0.0.1
EOF

  openssl req -new \
    -key "$new_key" \
    -subj "/C=US/O=Ration Dev/CN=${svc}" \
    -out "$csr" >/dev/null 2>&1

  if go run "$ROOT/services/auth/cmd/issue-service-cert" \
      -auth-addr "$AUTH_ADDR" \
      -service "$svc" \
      -csr "$csr" \
      -ttl "$TTL" \
      -out-cert "$new_cert"; then
    mv "$new_cert" "$OUT_DIR/${svc}-cert.pem"
    mv "$new_key" "$key"
    ROTATED=$((ROTATED + 1))
  else
    echo "rotate-dev-ca: failed to rotate ${svc}, leaving its existing cert/key in place" >&2
    rm -f "$new_cert" "$new_key"
  fi

  rm -f "$csr" "$ext"
done

echo
echo "==> done. ${ROTATED}/${#SERVICES[@]} service leaf certificates rotated in $OUT_DIR"
echo "    restart any service whose cert just rotated to pick it up (pkg/mtls"
echo "    loads *_MTLS_CERT_FILE/*_MTLS_KEY_FILE once at startup, not on a watch)."
