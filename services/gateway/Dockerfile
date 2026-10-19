# Build context MUST be the repo root (services/gateway's go.mod has
# `replace github.com/caelum0x/ration/pkg => ../../pkg`, a relative path
# into the monorepo) — build with:
#   docker build -f services/gateway/Dockerfile -t ration/gateway .
#
# Multi-stage: a full Go toolchain compiles a fully static binary, then a
# distroless final image runs it. GOWORK=off throughout so the build only
# needs pkg/ + this service's own directory, not every go.work member
# (agent/cli/tui) copied into the build context.

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# Dependency layers first (go.sum unchanged -> this layer caches).
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/gateway/go.mod services/gateway/go.sum ./services/gateway/
WORKDIR /src/services/gateway
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/gateway/ ./services/gateway/

WORKDIR /src/services/gateway
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# distroless/static (not scratch): gateway makes real outbound HTTPS calls
# (Airwallex API in live mode) and needs the CA root bundle that
# static-debian12 ships, which bare `scratch` does not.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gateway /usr/local/bin/gateway

EXPOSE 7002 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
