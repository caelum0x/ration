# The TUI is an interactive operator tool, not a long-running deployed
# service (see infra/terraform/README.md) — this Dockerfile exists for a
# consistent `docker run` dev experience, not for ECS deployment.
#
# Build context MUST be the repo root — build with:
#   docker build -f services/tui/Dockerfile -t ration/tui .
# Run with -it so bubbletea gets a real TTY:
#   docker run -it --rm -e GATEWAY_GRPC_ADDR=... -e LEDGER_CORE_ADDR=... ration/tui

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/tui/go.mod services/tui/go.sum ./services/tui/
WORKDIR /src/services/tui
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/tui/ ./services/tui/

WORKDIR /src/services/tui
RUN go build -trimpath -ldflags="-s -w" -o /out/tui ./cmd/tui

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tui /usr/local/bin/tui

ENTRYPOINT ["/usr/local/bin/tui"]
