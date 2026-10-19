# The operator CLI is a human-driven tool, not a long-running deployed
# service (see infra/terraform/README.md) — this Dockerfile exists for a
# consistent `docker run` dev experience, not for ECS deployment.
#
# Build context MUST be the repo root — build with:
#   docker build -f services/cli/Dockerfile -t ration/cli .
# Run with:
#   docker run --rm -e GATEWAY_GRPC_ADDR=... -e LEDGER_CORE_ADDR=... ration/cli <subcommand>

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/cli/go.mod services/cli/go.sum ./services/cli/
WORKDIR /src/services/cli
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/cli/ ./services/cli/

WORKDIR /src/services/cli
RUN go build -trimpath -ldflags="-s -w" -o /out/ration ./cmd/ration

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ration /usr/local/bin/ration

ENTRYPOINT ["/usr/local/bin/ration"]
