#!/usr/bin/env bash
# Generates Go protobuf/gRPC stubs into pkg/pb/*.
# Rust stubs are generated at build time by services/ledger-core/build.rs (tonic-build),
# not here.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/pkg/pb"
mkdir -p "$OUT"

if ! command -v protoc-gen-go >/dev/null 2>&1; then
  echo "installing protoc-gen-go / protoc-gen-go-grpc..."
  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
fi

protoc \
  --proto_path="$ROOT/proto" \
  --go_out="$OUT" --go_opt=paths=source_relative \
  --go-grpc_out="$OUT" --go-grpc_opt=paths=source_relative \
  "$ROOT"/proto/*.proto

echo "generated Go stubs in $OUT"
