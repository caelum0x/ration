# Build context MUST be the repo root (services/card-issuance's go.mod
# has `replace github.com/caelum0x/ration/pkg => ../../pkg`, a relative
# path into the monorepo) — build with:
#   docker build -f services/card-issuance/Dockerfile -t ration/card-issuance .
#
# Multi-stage: a full Go toolchain compiles a fully static binary, then a
# distroless final image runs it. GOWORK=off throughout so the build only
# needs pkg/ + this service's own directory.

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# Dependency layers first (go.sum unchanged -> this layer caches).
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/card-issuance/go.mod services/card-issuance/go.sum ./services/card-issuance/
WORKDIR /src/services/card-issuance
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/card-issuance/ ./services/card-issuance/

WORKDIR /src/services/card-issuance
RUN go build -trimpath -ldflags="-s -w" -o /out/card-issuance ./cmd/card-issuance

# distroless/static (not scratch): card-issuance makes real outbound
# HTTPS calls (Airwallex API in live mode) and needs the CA root bundle
# that static-debian12 ships, which bare `scratch` does not.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/card-issuance /usr/local/bin/card-issuance

EXPOSE 7012
ENTRYPOINT ["/usr/local/bin/card-issuance"]
