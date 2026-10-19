# Build context MUST be the repo root (services/agent's go.mod has
# `replace github.com/caelum0x/ration/pkg => ../../pkg`) — build with:
#   docker build -f services/agent/Dockerfile -t ration/agent .

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/agent/go.mod services/agent/go.sum ./services/agent/
WORKDIR /src/services/agent
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/agent/ ./services/agent/

WORKDIR /src/services/agent
RUN go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent

# distroless/static (not scratch): the agent calls the real Anthropic API
# over HTTPS and needs the CA root bundle static-debian12 ships.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/agent /usr/local/bin/agent

# No EXPOSE: cmd/agent/main.go never listens on a socket, it's a pure
# poller against ledger-core/gateway.
ENTRYPOINT ["/usr/local/bin/agent"]
