.PHONY: proto build build-wal build-ledger build-go test lint run-demo clean observability-up observability-down

PROTOC_GEN := scripts/gen-proto.sh

proto:
	bash $(PROTOC_GEN)

build-wal:
	$(MAKE) -C services/wal

build-ledger: build-wal
	cd services/ledger-core && cargo build --release

build-go:
	cd services/gateway && go build -o ../../bin/gateway ./cmd/gateway
	cd services/agent && go build -o ../../bin/agent ./cmd/agent
	cd services/tui && go build -o ../../bin/tui ./cmd/tui
	cd services/cli && go build -o ../../bin/ration ./cmd/ration
	cd services/audit-log && go build -o ../../bin/audit-log ./cmd/audit-log

build: proto build-ledger build-go

test:
	$(MAKE) -C services/wal test
	cd services/ledger-core && cargo test
	cd pkg && go test ./...
	cd services/gateway && go test ./...
	cd services/agent && go test ./...
	cd services/cli && go test ./...
	cd services/tui && go test ./...
	cd services/audit-log && go test ./...

lint:
	bash scripts/pre-commit.sh

run-demo: build
	./scripts/run-demo.sh

clean:
	rm -rf bin services/wal/*.o services/wal/*.a services/wal/test_wal
	cd services/ledger-core && cargo clean || true

# docs/SCALE-EXPANSION.md section 2: otel-collector + jaeger + prometheus
# + grafana only, without rebuilding/starting the four app services —
# useful when iterating on infra/otel-collector/config.yaml,
# infra/prometheus/prometheus.yml, or a Grafana dashboard without
# restarting ledger-core/gateway/agent/audit-log in the process.
observability-up:
	docker compose up -d otel-collector jaeger prometheus grafana

observability-down:
	docker compose stop otel-collector jaeger prometheus grafana
