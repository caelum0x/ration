# Build context MUST be the repo root (build.rs compiles ../wal's C
# sources and tonic-build compiles every proto in ../../proto, both via
# paths relative to services/ledger-core) — build with:
#   docker build -f services/ledger-core/Dockerfile -t ration/ledger-core .

# 1.82 doesn't build this: the Cargo.lock currently in this repo pulls in
# rand_core 0.10.1 (a transitive dep introduced by docs/SCALE-EXPANSION.md
# section 2's observability work landing in parallel, see the --locked
# note below), which requires Cargo's `edition2024` feature — stabilized
# in Rust 1.85. Pinned to 1.90 (newest verified-available slim tag as of
# this pass) rather than 1.85 exactly, to leave headroom as further deps
# land during this same parallel pass.
FROM rust:1.90-slim AS build

# protobuf-compiler: tonic-build/prost-build need a system `protoc` to
# compile proto/*.proto (build.rs runs this at compile time, same as
# scripts/gen-proto.sh does for local dev).
RUN apt-get update \
    && apt-get install -y --no-install-recommends protobuf-compiler ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY proto/ ./proto/
COPY services/wal/ ./services/wal/
COPY services/ledger-core/ ./services/ledger-core/

WORKDIR /src/services/ledger-core

# Not --locked: Cargo.toml is still gaining deps as the observability
# section of docs/SCALE-EXPANSION.md lands in parallel (see
# docs/SCALE-EXPANSION.md's "safe to parallelize" execution note) — a
# stale Cargo.lock would hard-fail a --locked build. Once that section
# settles, re-add --locked here and make sure Cargo.lock is committed and
# current.
RUN cargo build --release

# A writable data dir for the WAL + SQLite index, created in the build
# stage (which has a shell) since distroless has none.
RUN mkdir -p /data

# distroless/cc (not scratch/static): rusqlite's `bundled` feature
# statically links libsqlite3 into the binary itself, but the Rust binary
# produced by the default glibc target still dynamically links libc and
# libgcc — distroless/cc ships exactly those two, nothing else.
FROM gcr.io/distroless/cc-debian12:nonroot

# 65532 is distroless's standard nonroot uid:gid (the `:nonroot` tag runs
# as this user already; stated explicitly here so the directory is
# writable by whichever uid actually runs the container).
COPY --from=build --chown=65532:65532 /data /data
COPY --from=build /src/services/ledger-core/target/release/ledger-core /usr/local/bin/ledger-core

ENV RATION_LEDGER_WAL_PATH=/data/ledger.wal \
    RATION_LEDGER_DB_PATH=/data/ledger.db \
    RUST_LOG=info

EXPOSE 7001
ENTRYPOINT ["/usr/local/bin/ledger-core"]
