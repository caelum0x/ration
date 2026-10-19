# Build context MUST be the repo root (services/auth's go.mod has
# `replace github.com/caelum0x/ration/pkg => ../../pkg`, a relative path
# into the monorepo) — build with:
#   docker build -f services/auth/Dockerfile -t ration/auth .

FROM golang:1.26-alpine AS build

ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/auth/go.mod services/auth/go.sum ./services/auth/
WORKDIR /src/services/auth
RUN go mod download

WORKDIR /src
COPY pkg/ ./pkg/
COPY services/auth/ ./services/auth/

WORKDIR /src/services/auth
RUN go build -trimpath -ldflags="-s -w" -o /out/auth ./cmd/auth

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/auth /usr/local/bin/auth

EXPOSE 7010
ENTRYPOINT ["/usr/local/bin/auth"]
