CC ?= cc
CFLAGS ?= -std=c11 -Wall -Wextra -O2 -g -D_FILE_OFFSET_BITS=64
AR ?= ar
ARFLAGS := rcs

LIB_SRCS := wal.c crc32.c
LIB_OBJS := $(LIB_SRCS:.c=.o)
LIB := libwal.a

TEST_BIN := test_wal
BENCH_BIN := bench_wal

.PHONY: all test bench clean

all: $(LIB)

$(LIB): $(LIB_OBJS)
	$(AR) $(ARFLAGS) $@ $^

%.o: %.c wal.h frame.h crc32.h
	$(CC) $(CFLAGS) -c $< -o $@

$(TEST_BIN): tests/test_wal.c $(LIB)
	$(CC) $(CFLAGS) -I. $< $(LIB) -o $@

test: $(TEST_BIN)
	./$(TEST_BIN)

bench: $(BENCH_BIN)

$(BENCH_BIN): bench/bench_wal.c $(LIB)
	$(CC) $(CFLAGS) -I. $< $(LIB) -o $@

clean:
	rm -f $(LIB_OBJS) $(LIB) $(TEST_BIN) $(BENCH_BIN)
	rm -f bench/*.o tests/*.o
	rm -rf $(TEST_BIN).dSYM $(BENCH_BIN).dSYM
