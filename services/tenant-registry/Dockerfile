# Build context MUST be the repo root (services/tenant-registry's
# go.mod has `replace github.com/caelum0x/ration/pkg => ../../pkg`, a
# relative path into the monorepo) — build with:
#   docker build -f services/tenant-registry/Dockerfile -t ration/tenant-registry .

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/tenant-registry/go.mod services/tenant-registry/go.sum ./services/tenant-registry/
WORKDIR /src/services/tenant-registry
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/tenant-registry/ ./services/tenant-registry/

WORKDIR /src/services/tenant-registry
RUN go build -trimpath -ldflags="-s -w" -o /out/tenant-registry ./cmd/tenant-registry

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tenant-registry /usr/local/bin/tenant-registry

EXPOSE 7011
ENTRYPOINT ["/usr/local/bin/tenant-registry"]
