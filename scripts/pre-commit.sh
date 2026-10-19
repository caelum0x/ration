#!/usr/bin/env bash
# scripts/pre-commit.sh — the static-analysis gate for `make lint` and, if
# installed as a git hook, for `git commit`.
#
# Runs every repo-wide static check CI (.github/workflows/ci.yml) runs for
# Go/Rust/C/shell, minus the actual test suites (that's `make test` — this
# script is lint-speed, not test-suite-speed) and minus `cargo audit` (a
# network call against the advisory DB; CI runs it, this script doesn't, to
# keep local runs usable offline).
#
# Deliberately checks the whole tree, not just staged files: a partial,
# staged-only lint pass is exactly the kind of thing that looks green
# locally and then goes red in CI the moment an untouched-but-affected file
# is in scope. `make lint` passing should reliably predict CI's lint jobs
# passing.
#
# Install as a git hook (optional):
#   ln -sf ../../scripts/pre-commit.sh .git/hooks/pre-commit
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

STATUS=0

section() { printf '\n== %s ==\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; STATUS=1; }
require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'pre-commit.sh: missing required tool "%s" (%s)\n' "$1" "$2" >&2
    exit 127
  fi
}

# --- Go: gofmt, go vet, golangci-lint, for every discovered module --------
section "Go"
require gofmt "install Go: https://go.dev/dl/"
require golangci-lint "install: https://golangci-lint.run/usage/install/"

mapfile -d '' -t go_modules < <(find . -name go.mod -not -path './.git/*' -print0 | sort -z)
for modfile in "${go_modules[@]}"; do
  mod_dir="$(dirname "$modfile")"
  echo "-- $mod_dir --"

  unformatted="$(gofmt -l "$mod_dir")"
  if [ -n "$unformatted" ]; then
    fail "$mod_dir: gofmt -l found unformatted file(s):"$'\n'"$unformatted"
  fi

  (cd "$mod_dir" && go vet ./...) || fail "$mod_dir: go vet"
  (cd "$mod_dir" && golangci-lint run ./...) || fail "$mod_dir: golangci-lint"
done

# --- Rust: cargo fmt --check, cargo clippy -D warnings ---------------------
section "Rust (ledger-core)"
require cargo "install Rust: https://rustup.rs"

(cd services/ledger-core && cargo fmt --check) || fail "services/ledger-core: cargo fmt --check"
(cd services/ledger-core && cargo clippy --all-targets -- -D warnings) \
  || fail "services/ledger-core: cargo clippy -D warnings"

# --- C: services/wal builds clean under -Wall -Wextra ----------------------
section "C (wal)"
require cc "install a C compiler (clang or gcc)"

( cd services/wal && make clean >/dev/null && make all ) || fail "services/wal: make all"

# --- Proto: protoc --fatal_warnings ----------------------------------------
# See .github/workflows/ci.yml's proto-lint job for why this is the protoc
# fallback (not `buf lint`) — same reasoning applies here.
section "Proto"
require protoc "install: https://grpc.io/docs/protoc-installation/"

protoc --proto_path=proto --fatal_warnings -o /dev/null proto/*.proto \
  || fail "proto/*.proto: protoc --fatal_warnings"

# --- Shell: shellcheck every script in scripts/ (best-effort — not every
# contributor machine has shellcheck, so this is a warn-and-continue check
# rather than a hard `require`, unlike the gates above which are load-bearing
# for the things this repo claims to guarantee (ledger correctness, proto
# contracts)). ---
section "Shell (scripts/*.sh)"
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck scripts/*.sh || fail "scripts/*.sh: shellcheck"
else
  echo "shellcheck not installed — skipping (install: https://www.shellcheck.net/) — not a hard requirement, but CI-equivalent coverage needs it locally too"
fi

if [ "$STATUS" -eq 0 ]; then
  printf '\nAll lint checks passed.\n'
else
  printf '\nOne or more lint checks failed — see FAIL lines above.\n' >&2
fi
exit "$STATUS"
