# Build context MUST be the repo root (services/webhook-ingestion's
# go.mod has `replace github.com/caelum0x/ration/pkg => ../../pkg`, a
# relative path into the monorepo) — build with:
#   docker build -f services/webhook-ingestion/Dockerfile -t ration/webhook-ingestion .
#
# Multi-stage: a full Go toolchain compiles a fully static binary, then a
# distroless final image runs it. GOWORK=off throughout so the build only
# needs pkg/ + this service's own directory.

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/webhook-ingestion/go.mod services/webhook-ingestion/go.sum ./services/webhook-ingestion/
WORKDIR /src/services/webhook-ingestion
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/webhook-ingestion/ ./services/webhook-ingestion/

WORKDIR /src/services/webhook-ingestion
RUN go build -trimpath -ldflags="-s -w" -o /out/webhook-ingestion ./cmd/webhook-ingestion

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/webhook-ingestion /usr/local/bin/webhook-ingestion

EXPOSE 7013 8081
ENTRYPOINT ["/usr/local/bin/webhook-ingestion"]
