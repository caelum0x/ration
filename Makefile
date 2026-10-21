.PHONY: proto build build-wal build-ledger build-go build-sdk test lint run-demo clean observability-up observability-down

PROTOC_GEN := scripts/gen-proto.sh

proto:
	bash $(PROTOC_GEN)

build-wal:
	$(MAKE) -C services/wal

build-ledger: build-wal
	cd services/ledger-core && cargo build --release

build-go:
	cd services/gateway && go build -o ../../bin/gateway ./cmd/gateway
	cd services/auth && go build -o ../../bin/auth ./cmd/auth
	cd services/tenant-registry && go build -o ../../bin/tenant-registry ./cmd/tenant-registry
	cd services/card-issuance && go build -o ../../bin/card-issuance ./cmd/card-issuance
	cd services/webhook-ingestion && go build -o ../../bin/webhook-ingestion ./cmd/webhook-ingestion
	cd services/agent && go build -o ../../bin/agent ./cmd/agent
	cd services/tui && go build -o ../../bin/tui ./cmd/tui
	cd services/cli && go build -o ../../bin/ration ./cmd/ration
	cd services/audit-log && go build -o ../../bin/audit-log ./cmd/audit-log
	cd services/audit-log && go build -o ../../bin/verify-chain ./cmd/verify-chain

# sdk/go/ration and sdk/rust/ration-client (docs/SCALE-EXPANSION.md
# section 7) are libraries, not deployables — "build" here means a
# compile check (go build / cargo build), there is no bin/ artifact to
# produce for either.
build-sdk:
	cd sdk/go/ration && go build ./...
	cd sdk/rust/ration-client && cargo build --release

build: proto build-ledger build-go build-sdk

test:
	$(MAKE) -C services/wal test
	cd services/ledger-core && cargo test
	cd pkg && go test ./...
	cd services/gateway && go test ./...
	cd services/auth && go test ./...
	cd services/tenant-registry && go test ./...
	cd services/card-issuance && go test ./...
	cd services/webhook-ingestion && go test ./...
	cd services/agent && go test ./...
	cd services/cli && go test ./...
	cd services/tui && go test ./...
	cd services/audit-log && go test ./...
	cd sdk/go/ration && go test ./...
	cd sdk/rust/ration-client && cargo test

lint:
	bash scripts/pre-commit.sh

run-demo: build
	./scripts/run-demo.sh

clean:
	rm -rf bin services/wal/*.o services/wal/*.a services/wal/test_wal
	cd services/ledger-core && cargo clean || true
	cd sdk/rust/ration-client && cargo clean || true

# docs/SCALE-EXPANSION.md section 2: otel-collector + jaeger + prometheus
# + grafana only, without rebuilding/starting the four app services —
# useful when iterating on infra/otel-collector/config.yaml,
# infra/prometheus/prometheus.yml, or a Grafana dashboard without
# restarting ledger-core/gateway/agent/audit-log in the process.
observability-up:
	docker compose up -d otel-collector jaeger prometheus grafana

observability-down:
	docker compose stop otel-collector jaeger prometheus grafana
