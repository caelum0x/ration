#!/usr/bin/env bash
# Generates Go protobuf/gRPC stubs into pkg/pb/*.
# Rust stubs are generated at build time by services/ledger-core/build.rs (tonic-build),
# not here.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# protoc writes relative to the module root (via --go_opt=module=...), landing
# at $ROOT/pkg/pb/<common|gateway|ledger>/*.pb.go to match each proto's
# go_package option (github.com/caelum0x/ration/pkg/pb/<name>). Do NOT switch
# to paths=source_relative: with multiple protos in one flat proto/ dir, that
# writes flat files straight into pkg/pb/, which collide with (and shadow)
# these package subdirectories.
MODULE="github.com/caelum0x/ration"
mkdir -p "$ROOT/pkg/pb"
export PATH="$PATH:$(go env GOPATH)/bin"

if ! command -v protoc-gen-go >/dev/null 2>&1; then
  echo "installing protoc-gen-go / protoc-gen-go-grpc..."
  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
fi

protoc \
  --proto_path="$ROOT/proto" \
  --go_out="$ROOT" --go_opt=module="$MODULE" \
  --go-grpc_out="$ROOT" --go-grpc_opt=module="$MODULE" \
  "$ROOT"/proto/*.proto

echo "generated Go stubs in $ROOT/pkg/pb"
