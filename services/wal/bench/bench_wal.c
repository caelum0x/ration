#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE

#include "wal.h"
#include "frame.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

/*
 * Append throughput micro-benchmark. Every wal_append() fsync()s, so this
 * mostly measures how fast the underlying disk can do small synchronous
 * writes - that is the real cost model for a WAL, not something to hide.
 *
 * Usage: bench_wal [n_frames] [payload_size_bytes]
 */

static double now_seconds(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int main(int argc, char **argv) {
    const char *path = "bench_wal.log";
    long n_frames = 10000;
    long payload_size = 256;

    if (argc > 1) {
        n_frames = atol(argv[1]);
    }
    if (argc > 2) {
        payload_size = atol(argv[2]);
    }
    if (n_frames <= 0) {
        n_frames = 10000;
    }
    if (payload_size <= 0) {
        payload_size = 256;
    }

    unlink(path);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    if (!w) {
        fprintf(stderr, "bench: wal_open failed: %d\n", (int)st);
        return 1;
    }

    uint8_t *payload = malloc((size_t)payload_size);
    if (!payload) {
        fprintf(stderr, "bench: out of memory\n");
        wal_close(w);
        return 1;
    }
    memset(payload, 0xAB, (size_t)payload_size);

    double start = now_seconds();
    for (long i = 0; i < n_frames; i++) {
        wal_status_t rc = wal_append(w, payload, (uint32_t)payload_size);
        if (rc != WAL_OK) {
            fprintf(stderr, "bench: append %ld failed: %d\n", i, (int)rc);
            free(payload);
            wal_close(w);
            unlink(path);
            return 1;
        }
    }
    double elapsed = now_seconds() - start;

    double total_bytes = (double)n_frames * (double)(payload_size + (long)WAL_FRAME_HEADER_SIZE);
    double mb = total_bytes / (1024.0 * 1024.0);
    double appends_per_sec = elapsed > 0.0 ? (double)n_frames / elapsed : 0.0;
    double mb_per_sec = elapsed > 0.0 ? mb / elapsed : 0.0;

    printf("wal bench: %ld frames x %ld byte payload (fsync per append)\n", n_frames, payload_size);
    printf("  elapsed:     %.3f s\n", elapsed);
    printf("  appends/sec: %.1f\n", appends_per_sec);
    printf("  throughput:  %.2f MB/s\n", mb_per_sec);

    free(payload);
    wal_close(w);
    unlink(path);
    return 0;
}
