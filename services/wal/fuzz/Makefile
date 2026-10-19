# Builds and runs the libFuzzer harness for wal_read_next()'s frame
# parser (see fuzz_frame_parse.c for the full rationale).
#
# Not wired into the top-level `make test` — fuzzing is a separate,
# explicitly-invoked activity, not a per-commit correctness gate.
#
# Requires a clang that bundles the libFuzzer runtime
# (libclang_rt.fuzzer_osx.a / libclang_rt.fuzzer-*.a). On Linux, the
# system clang package normally includes this. On macOS, Apple's
# Command Line Tools clang does NOT bundle it — only upstream/Homebrew
# clang does. If `make build` fails with
# "library ... libclang_rt.fuzzer_osx.a not found", the fix is:
#
#   brew install llvm
#
# which this Makefile then auto-detects below; no manual CC= needed.

UNAME_S := $(shell uname -s)

ifeq ($(origin CC),default)
ifeq ($(UNAME_S),Darwin)
HOMEBREW_LLVM_CLANG := $(firstword $(wildcard /opt/homebrew/opt/llvm*/bin/clang /usr/local/opt/llvm*/bin/clang))
ifneq ($(HOMEBREW_LLVM_CLANG),)
CC := $(HOMEBREW_LLVM_CLANG)
else
CC := clang
endif
else
CC := clang
endif
endif

ifeq ($(UNAME_S),Darwin)
SDK_FLAGS := -isysroot $(shell xcrun --show-sdk-path)
endif

CFLAGS ?= -std=c11 -g -O1 -fsanitize=fuzzer,address,undefined -I.. $(SDK_FLAGS)
BIN := fuzz_frame_parse
SRCS := fuzz_frame_parse.c ../wal.c ../crc32.c

.PHONY: build fuzz smoke clean

build: $(BIN)

$(BIN): $(SRCS) ../wal.h ../frame.h ../crc32.h
	$(CC) $(CFLAGS) $(SRCS) -o $(BIN)

# Short, CI-friendly run: proves the harness builds and runs clean, not
# an exhaustive fuzz campaign. Override with SECONDS=N.
SECONDS ?= 30
smoke: build
	mkdir -p corpus
	./$(BIN) corpus/ -max_total_time=$(SECONDS) -print_final_stats=1

# Long-running real fuzz campaign.
MINUTES ?= 60
fuzz: build
	mkdir -p corpus
	./$(BIN) corpus/ -max_total_time=$$(($(MINUTES) * 60)) -print_final_stats=1

clean:
	rm -f $(BIN)
	rm -rf $(BIN).dSYM
	rm -f crash-* leak-* oom-* timeout-*
