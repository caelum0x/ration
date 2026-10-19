# Build context MUST be the repo root (services/audit-log's go.mod has
# `replace github.com/caelum0x/ration/pkg => ../../pkg`) — build with:
#   docker build -f services/audit-log/Dockerfile -t ration/audit-log .

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/audit-log/go.mod services/audit-log/go.sum ./services/audit-log/
WORKDIR /src/services/audit-log
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/audit-log/ ./services/audit-log/

WORKDIR /src/services/audit-log
RUN go build -trimpath -ldflags="-s -w" -o /out/audit-log ./cmd/audit-log

# A writable data dir for the hash-chained append-only log, created in
# the build stage (which has a shell) since distroless has none.
RUN mkdir -p /data

# distroless/static (not scratch): audit-log itself makes no outbound
# HTTPS calls (it only ever accepts inbound gRPC from ledger-core/
# card-issuance), so it needs no CA root bundle, but `static-debian12`
# still gives it /etc/passwd's nonroot entry and tzdata-free timezone
# handling that bare `scratch` doesn't — same reasoning as
# services/cli and services/tui's Dockerfiles.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build --chown=65532:65532 /data /data
COPY --from=build /out/audit-log /usr/local/bin/audit-log

ENV AUDIT_LOG_GRPC_ADDR=0.0.0.0:7003 \
    RATION_AUDIT_LOG_DATA_DIR=/data

EXPOSE 7003
ENTRYPOINT ["/usr/local/bin/audit-log"]
