.PHONY: proto build build-wal build-ledger build-go test run-demo clean

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

build: proto build-ledger build-go

test:
	$(MAKE) -C services/wal test
	cd services/ledger-core && cargo test
	cd services/gateway && go test ./...
	cd services/agent && go test ./...

run-demo: build
	./scripts/run-demo.sh

clean:
	rm -rf bin services/wal/*.o services/wal/*.a services/wal/test_wal
	cd services/ledger-core && cargo clean || true
