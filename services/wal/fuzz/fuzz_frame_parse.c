/*
 * libFuzzer harness for wal_read_next()'s frame parser.
 *
 * wal_read_next() is the file closest to a real memory-safety bug in
 * this codebase: it parses an 8-byte length+CRC32 header off disk and
 * then trusts that length (after a WAL_MAX_PAYLOAD_SIZE sanity cap and
 * a "does the payload actually fit before the known end of file" check)
 * to size a read into a caller buffer. This harness feeds libFuzzer's
 * mutated bytes straight in as the on-disk *content* of a WAL file —
 * malformed headers, truncated payloads, bit-flipped CRCs, lengths that
 * claim to extend past EOF — and calls wal_read_next() in a loop exactly
 * the way ledger-core's WAL-replay path does, so any way raw bytes can
 * trigger an out-of-bounds read, use of uninitialized memory, or a leak
 * gets caught by ASan/libFuzzer rather than silently corrupting a
 * production ledger.
 *
 * This deliberately does NOT go through wal_recover()'s truncate-on-torn-
 * tail logic — wal_read_next() itself is the lower-level primitive that
 * must never misbehave on malformed input, and wal_recover() is just one
 * caller of it (grpc.rs / ledger.rs's replay path is the other, equally
 * exposed to anything an attacker could get into the WAL file, e.g. via
 * a compromised disk, a crafted backup restore, or a bug elsewhere that
 * writes bytes that aren't actually a valid frame).
 *
 * Build (requires clang; Apple clang on macOS and upstream clang on
 * Linux both ship libFuzzer):
 *
 *   clang -std=c11 -g -O1 \
 *     -fsanitize=fuzzer,address,undefined \
 *     -I.. \
 *     fuzz_frame_parse.c ../wal.c ../crc32.c \
 *     -o fuzz_frame_parse
 *
 * Or: make -C services/wal/fuzz   (see Makefile in this directory)
 *
 * Run for a bounded smoke duration (CI-friendly — this harness existing
 * and running clean matters more here than exhaustive fuzz time):
 *
 *   ./fuzz_frame_parse -max_total_time=30
 *
 * Run longer / for real campaign fuzzing:
 *
 *   mkdir -p corpus
 *   ./fuzz_frame_parse corpus/ -max_total_time=3600
 */

#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE

#include "wal.h"

#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Generous but bounded read buffer. A frame claiming a payload larger
 * than this must come back as WAL_ERR_BUFFER_SMALL, never a crash — that
 * path is exercised deliberately by letting the fuzzer discover large
 * length fields. */
#define FUZZ_READ_BUF_CAP (256u * 1024u)

/* Hard cap on frames read per input so a maliciously-crafted (or just
 * very large, repeatedly-valid-frame) input can't turn one fuzz
 * iteration into an unbounded loop. wal_read_next()'s cursor is strictly
 * monotonic on WAL_OK, so this is purely a defensive ceiling, not a
 * correctness requirement. */
#define FUZZ_MAX_FRAMES_PER_INPUT 100000

/* One fixed path, reused (and O_TRUNC'd) across every call in this
 * process — libFuzzer drives thousands of iterations per second
 * in-process, so avoiding a fresh mkstemp() per call matters for
 * throughput. Namespaced by PID so parallel fuzzer jobs (`-jobs=N`)
 * never collide. */
static char g_path[64];

static void fuzz_path_init(void) {
    if (g_path[0] == '\0') {
        snprintf(g_path, sizeof(g_path), "/tmp/ration_wal_fuzz_%d.wal", (int)getpid());
    }
}

static int write_input_as_wal_file(const uint8_t *data, size_t size) {
    int fd = open(g_path, O_RDWR | O_CREAT | O_TRUNC, 0644);
    if (fd < 0) {
        return -1;
    }
    size_t written = 0;
    while (written < size) {
        ssize_t n = write(fd, data + written, size - written);
        if (n < 0) {
            close(fd);
            return -1;
        }
        written += (size_t)n;
    }
    close(fd);
    return 0;
}

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
    fuzz_path_init();

    if (write_input_as_wal_file(data, size) != 0) {
        /* Harness I/O failure (e.g. out of disk in a constrained CI
         * sandbox) is not a finding about wal.c — just skip this input
         * rather than asserting. */
        return 0;
    }

    wal_status_t open_status;
    wal_t *w = wal_open(g_path, &open_status);
    if (!w) {
        unlink(g_path);
        return 0;
    }

    static uint8_t buf[FUZZ_READ_BUF_CAP];
    for (int i = 0; i < FUZZ_MAX_FRAMES_PER_INPUT; i++) {
        uint32_t out_len = 0;
        wal_status_t status = wal_read_next(w, buf, sizeof(buf), &out_len);
        if (status == WAL_OK) {
            /* A real frame parsed. out_len must never exceed what we
             * handed in as capacity — if it did, that's exactly the
             * class of bug (trusting an on-disk length field past the
             * buffer it's being copied into) this harness exists to
             * catch; ASan would already have caught the overflow
             * itself, but assert the contract explicitly too. */
            if (out_len > sizeof(buf)) {
                __builtin_trap();
            }
            continue;
        }
        if (status == WAL_ERR_BUFFER_SMALL) {
            /* Frame is well-formed but larger than our fixed read
             * buffer. wal_read_next() guarantees the cursor is left
             * untouched in this case, so retrying forever would spin —
             * stop reading this (synthetic) file here, same as a real
             * caller would switch to a larger buffer and retry once,
             * not in a tight loop. */
            break;
        }
        /* WAL_ERR_EOF, WAL_ERR_CORRUPT, WAL_ERR_IO, or anything else:
         * all terminal for this input. Malformed/truncated/bit-flipped
         * input is expected to surface as WAL_ERR_CORRUPT, not a crash —
         * that is the property under test. */
        break;
    }

    wal_close(w);
    unlink(g_path);
    return 0;
}
