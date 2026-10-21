#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE
#define _DARWIN_C_SOURCE

#include "wal.h"
#include "frame.h"
#include "crc32.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/stat.h>

static int g_failures = 0;

#define EXPECT(cond)                                                        \
    do {                                                                    \
        if (!(cond)) {                                                      \
            fprintf(stderr, "  FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); \
            g_failures++;                                                   \
        }                                                                   \
    } while (0)

#define RUN(test_fn)                                                        \
    do {                                                                    \
        printf("-- %s\n", #test_fn);                                       \
        test_fn();                                                         \
    } while (0)

/* ---------------------------------------------------------------- */
/* test fixture helpers                                              */
/* ---------------------------------------------------------------- */

static void make_tmp_dir(char *out, size_t cap) {
    snprintf(out, cap, "/tmp/ration_wal_test_XXXXXX");
    if (!mkdtemp(out)) {
        perror("mkdtemp");
        exit(1);
    }
}

static void remove_tmp_dir(const char *dir, const char *file_path) {
    unlink(file_path);
    rmdir(dir);
}

static off_t file_size_of(const char *path) {
    struct stat st;
    if (stat(path, &st) != 0) {
        return -1;
    }
    return st.st_size;
}

/* Truncate `path` to `new_size` bytes - used to simulate a crash that
 * left a torn frame (partial header and/or partial payload) at the tail
 * of the WAL file. */
static void truncate_file_to(const char *path, off_t new_size) {
    if (truncate(path, new_size) != 0) {
        perror("truncate");
        exit(1);
    }
}

/* Flip one byte inside the file at `offset` - used to simulate silent
 * corruption of an already-fsynced frame. */
static void corrupt_byte_at(const char *path, off_t offset) {
    int fd = open(path, O_RDWR);
    if (fd < 0) {
        perror("open for corruption");
        exit(1);
    }
    uint8_t byte;
    if (pread(fd, &byte, 1, offset) != 1) {
        perror("pread for corruption");
        exit(1);
    }
    byte ^= 0xFFu;
    if (pwrite(fd, &byte, 1, offset) != 1) {
        perror("pwrite for corruption");
        exit(1);
    }
    close(fd);
}

typedef struct {
    uint8_t payloads[16][256];
    uint32_t lens[16];
    int count;
} capture_t;

static int capture_cb(const uint8_t *payload, uint32_t len, void *ctx) {
    capture_t *cap = (capture_t *)ctx;
    if (cap->count >= 16 || len > sizeof(cap->payloads[0])) {
        return 1;
    }
    if (len > 0) {
        memcpy(cap->payloads[cap->count], payload, len);
    }
    cap->lens[cap->count] = len;
    cap->count++;
    return 0;
}

/* ---------------------------------------------------------------- */
/* tests                                                              */
/* ---------------------------------------------------------------- */

static void test_frame_header_roundtrip(void) {
    wal_frame_header_t hdr = {0x01020304u, 0x0A0B0C0Du};
    uint8_t buf[WAL_FRAME_HEADER_SIZE];
    wal_frame_header_encode(&hdr, buf);

    /* explicit little-endian on disk */
    EXPECT(buf[0] == 0x04 && buf[1] == 0x03 && buf[2] == 0x02 && buf[3] == 0x01);
    EXPECT(buf[4] == 0x0D && buf[5] == 0x0C && buf[6] == 0x0B && buf[7] == 0x0A);

    wal_frame_header_t decoded;
    wal_frame_header_decode(buf, &decoded);
    EXPECT(decoded.length == hdr.length);
    EXPECT(decoded.crc32 == hdr.crc32);
}

static void test_crc32_known_vectors(void) {
    EXPECT(crc32_compute(NULL, 0) == 0x00000000u);

    const uint8_t check[] = "123456789";
    /* Standard CRC-32/ISO-HDLC check value for the ASCII string "123456789". */
    EXPECT(crc32_compute(check, 9) == 0xCBF43926u);
}

static void test_append_and_read_back(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);
    EXPECT(st == WAL_OK);

    const char *msgs[] = {"hello", "", "write-ahead log", "x"};
    for (size_t i = 0; i < sizeof(msgs) / sizeof(msgs[0]); i++) {
        wal_status_t rc = wal_append(w, (const uint8_t *)msgs[i], (uint32_t)strlen(msgs[i]));
        EXPECT(rc == WAL_OK);
    }
    wal_close(w);

    w = wal_open(path, &st);
    EXPECT(w != NULL);

    uint8_t buf[256];
    for (size_t i = 0; i < sizeof(msgs) / sizeof(msgs[0]); i++) {
        uint32_t len = 0;
        wal_status_t rc = wal_read_next(w, buf, sizeof(buf), &len);
        EXPECT(rc == WAL_OK);
        EXPECT(len == strlen(msgs[i]));
        EXPECT(memcmp(buf, msgs[i], len) == 0);
    }

    uint32_t len = 0;
    EXPECT(wal_read_next(w, buf, sizeof(buf), &len) == WAL_ERR_EOF);

    wal_close(w);
    remove_tmp_dir(dir, path);
}

static void test_recovery_of_clean_file(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *msgs[] = {"alpha", "beta", "gamma"};
    for (size_t i = 0; i < sizeof(msgs) / sizeof(msgs[0]); i++) {
        EXPECT(wal_append(w, (const uint8_t *)msgs[i], (uint32_t)strlen(msgs[i])) == WAL_OK);
    }
    wal_close(w);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_OK);
    EXPECT(stats.frames_recovered == 3);
    EXPECT(stats.torn_tail_dropped == 0);
    EXPECT(stats.torn_bytes_dropped == 0);
    EXPECT(cap.count == 3);
    for (size_t i = 0; i < sizeof(msgs) / sizeof(msgs[0]); i++) {
        EXPECT(cap.lens[i] == strlen(msgs[i]));
        EXPECT(memcmp(cap.payloads[i], msgs[i], cap.lens[i]) == 0);
    }

    remove_tmp_dir(dir, path);
}

/* The core requirement: a crash that tears the last frame must not be
 * fatal. The valid prefix survives, the torn frame is dropped, and the
 * file is left in a clean, appendable state. */
static void test_torn_tail_mid_payload_is_recovered(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *frame1 = "first-frame-payload";
    const char *frame2 = "second-frame-payload";
    const char *frame3 = "third-frame-will-be-torn";

    EXPECT(wal_append(w, (const uint8_t *)frame1, (uint32_t)strlen(frame1)) == WAL_OK);
    EXPECT(wal_append(w, (const uint8_t *)frame2, (uint32_t)strlen(frame2)) == WAL_OK);
    off_t offset_before_frame3 = file_size_of(path);
    EXPECT(wal_append(w, (const uint8_t *)frame3, (uint32_t)strlen(frame3)) == WAL_OK);
    off_t full_size = file_size_of(path);
    wal_close(w);

    /* Simulate a crash partway through writing frame3's payload: keep
     * the header and a few payload bytes, drop the rest. */
    off_t torn_size = offset_before_frame3 + (off_t)WAL_FRAME_HEADER_SIZE + 4;
    EXPECT(torn_size < full_size);
    truncate_file_to(path, torn_size);
    EXPECT(file_size_of(path) == torn_size);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_OK);
    EXPECT(stats.frames_recovered == 2);
    EXPECT(stats.torn_tail_dropped == 1);
    EXPECT(stats.torn_bytes_dropped == (size_t)(torn_size - offset_before_frame3));

    EXPECT(cap.count == 2);
    EXPECT(cap.lens[0] == strlen(frame1));
    EXPECT(memcmp(cap.payloads[0], frame1, cap.lens[0]) == 0);
    EXPECT(cap.lens[1] == strlen(frame2));
    EXPECT(memcmp(cap.payloads[1], frame2, cap.lens[1]) == 0);

    /* File must have been truncated back to exactly the valid prefix. */
    EXPECT(file_size_of(path) == offset_before_frame3);

    /* And the log must be healthy afterward: reopen, append a new frame,
     * and confirm the full sequence reads back as frame1, frame2, new. */
    wal_t *w2 = wal_open(path, &st);
    EXPECT(w2 != NULL);
    const char *new_frame = "replacement-frame";
    EXPECT(wal_append(w2, (const uint8_t *)new_frame, (uint32_t)strlen(new_frame)) == WAL_OK);

    uint8_t buf[256];
    uint32_t len = 0;
    EXPECT(wal_read_next(w2, buf, sizeof(buf), &len) == WAL_OK);
    EXPECT(len == strlen(frame1) && memcmp(buf, frame1, len) == 0);

    EXPECT(wal_read_next(w2, buf, sizeof(buf), &len) == WAL_OK);
    EXPECT(len == strlen(frame2) && memcmp(buf, frame2, len) == 0);

    EXPECT(wal_read_next(w2, buf, sizeof(buf), &len) == WAL_OK);
    EXPECT(len == strlen(new_frame) && memcmp(buf, new_frame, len) == 0);

    EXPECT(wal_read_next(w2, buf, sizeof(buf), &len) == WAL_ERR_EOF);

    wal_close(w2);
    remove_tmp_dir(dir, path);
}

static void test_torn_tail_mid_header_is_recovered(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *frame1 = "only-surviving-frame";
    EXPECT(wal_append(w, (const uint8_t *)frame1, (uint32_t)strlen(frame1)) == WAL_OK);
    off_t offset_before_frame2 = file_size_of(path);

    const char *frame2 = "frame-whose-header-gets-torn";
    EXPECT(wal_append(w, (const uint8_t *)frame2, (uint32_t)strlen(frame2)) == WAL_OK);
    wal_close(w);

    /* Leave only 3 of the 8 header bytes for frame2. */
    off_t torn_size = offset_before_frame2 + 3;
    truncate_file_to(path, torn_size);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_OK);
    EXPECT(stats.frames_recovered == 1);
    EXPECT(stats.torn_tail_dropped == 1);
    EXPECT(cap.count == 1);
    EXPECT(cap.lens[0] == strlen(frame1));
    EXPECT(memcmp(cap.payloads[0], frame1, cap.lens[0]) == 0);
    EXPECT(file_size_of(path) == offset_before_frame2);

    remove_tmp_dir(dir, path);
}

/* CRC must catch silent corruption too, not just truncation. */
static void test_corrupted_payload_is_dropped_as_torn(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *frame1 = "good-frame";
    off_t offset_before_frame2 = 0;
    EXPECT(wal_append(w, (const uint8_t *)frame1, (uint32_t)strlen(frame1)) == WAL_OK);
    offset_before_frame2 = file_size_of(path);

    const char *frame2 = "frame-that-gets-corrupted";
    EXPECT(wal_append(w, (const uint8_t *)frame2, (uint32_t)strlen(frame2)) == WAL_OK);
    wal_close(w);

    /* Flip one byte inside frame2's payload (just after its header),
     * leaving the length field intact but making the CRC mismatch. */
    corrupt_byte_at(path, offset_before_frame2 + (off_t)WAL_FRAME_HEADER_SIZE);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_OK);
    EXPECT(stats.frames_recovered == 1);
    EXPECT(stats.torn_tail_dropped == 1);
    EXPECT(cap.count == 1);
    EXPECT(memcmp(cap.payloads[0], frame1, cap.lens[0]) == 0);
    EXPECT(file_size_of(path) == offset_before_frame2);

    remove_tmp_dir(dir, path);
}

/* The core gap this test closes: a CRC mismatch is NOT always a torn
 * tail from a crash. If it happens somewhere in the MIDDLE of the file
 * (with fully valid, already-durable frames after it - here frame3,
 * which a real crash could never leave behind, since nothing runs
 * after a crash to write more good frames past a torn one), the old
 * behavior (treat any bad-CRC frame as "torn tail", truncate from
 * there to EOF) would silently and permanently destroy frame3 forever.
 * The fix must detect this is not tail corruption, refuse to truncate
 * anything, and report a hard error instead. */
static void test_mid_log_corruption_with_valid_frame_after_is_not_truncated(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *frame1 = "good-frame-one";
    const char *frame2 = "frame-that-gets-corrupted-but-is-not-the-tail";
    const char *frame3 = "good-frame-three-must-survive";

    EXPECT(wal_append(w, (const uint8_t *)frame1, (uint32_t)strlen(frame1)) == WAL_OK);
    off_t offset_before_frame2 = file_size_of(path);
    EXPECT(wal_append(w, (const uint8_t *)frame2, (uint32_t)strlen(frame2)) == WAL_OK);
    EXPECT(wal_append(w, (const uint8_t *)frame3, (uint32_t)strlen(frame3)) == WAL_OK);
    off_t full_size = file_size_of(path);
    wal_close(w);

    /* Flip one payload byte inside frame2 (leaving its length field, and
     * every byte of frame1/frame3, untouched) - simulates disk bit rot
     * after both frames were already fsynced, not a crash. */
    corrupt_byte_at(path, offset_before_frame2 + (off_t)WAL_FRAME_HEADER_SIZE);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_ERR_CORRUPT_MID_LOG);
    EXPECT(stats.frames_recovered == 1);
    EXPECT(stats.corrupt_frame_offset == (size_t)offset_before_frame2);
    EXPECT(stats.torn_tail_dropped == 0);
    EXPECT(stats.torn_bytes_dropped == 0);

    /* Only frame1 (the valid prefix before the corruption) was delivered
     * to the callback - frame2 is corrupt and frame3, though itself
     * perfectly valid, is never reached because recovery must not
     * silently skip past corruption and resume replaying. */
    EXPECT(cap.count == 1);
    EXPECT(cap.lens[0] == strlen(frame1));
    EXPECT(memcmp(cap.payloads[0], frame1, cap.lens[0]) == 0);

    /* The file must be left COMPLETELY untouched - frame3's bytes must
     * still be sitting on disk, recoverable by a human/tool later, not
     * truncated away. */
    EXPECT(file_size_of(path) == full_size);

    remove_tmp_dir(dir, path);
}

/* Sibling case: when the bad-CRC frame IS the last frame in the file
 * (nothing follows it at all), this must behave exactly like the
 * existing torn-tail case - vacuous "nothing after" is not evidence of
 * mid-log corruption, it's the classic crash-at-the-tail shape, and
 * truncating is correct and safe (this guards against an off-by-one in
 * the lookahead treating "no more bytes" as "the suffix is trivially
 * valid, so this must be mid-log corruption"). */
static void test_corrupted_last_frame_with_nothing_after_still_truncates(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *frame1 = "good-frame-one";
    const char *frame2 = "last-frame-gets-corrupted";

    EXPECT(wal_append(w, (const uint8_t *)frame1, (uint32_t)strlen(frame1)) == WAL_OK);
    off_t offset_before_frame2 = file_size_of(path);
    EXPECT(wal_append(w, (const uint8_t *)frame2, (uint32_t)strlen(frame2)) == WAL_OK);
    wal_close(w);

    corrupt_byte_at(path, offset_before_frame2 + (off_t)WAL_FRAME_HEADER_SIZE);

    capture_t cap = {.count = 0};
    wal_recovery_stats_t stats;
    wal_status_t rc = wal_recover(path, capture_cb, &cap, &stats);

    EXPECT(rc == WAL_OK);
    EXPECT(stats.frames_recovered == 1);
    EXPECT(stats.torn_tail_dropped == 1);
    EXPECT(cap.count == 1);
    EXPECT(file_size_of(path) == offset_before_frame2);

    remove_tmp_dir(dir, path);
}

static void test_append_rejects_oversized_payload(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    off_t before = file_size_of(path);
    wal_status_t rc = wal_append(w, (const uint8_t *)"x", (uint32_t)WAL_MAX_PAYLOAD_SIZE + 1);
    EXPECT(rc == WAL_ERR_TOO_LARGE);
    EXPECT(file_size_of(path) == before);

    wal_close(w);
    remove_tmp_dir(dir, path);
}

static void test_read_next_buffer_too_small_then_retry(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);

    const char *payload = "this-payload-is-longer-than-four-bytes";
    EXPECT(wal_append(w, (const uint8_t *)payload, (uint32_t)strlen(payload)) == WAL_OK);
    wal_close(w);

    w = wal_open(path, &st);
    EXPECT(w != NULL);

    uint8_t small_buf[4];
    uint32_t len = 0;
    EXPECT(wal_read_next(w, small_buf, sizeof(small_buf), &len) == WAL_ERR_BUFFER_SMALL);

    uint8_t big_buf[256];
    EXPECT(wal_read_next(w, big_buf, sizeof(big_buf), &len) == WAL_OK);
    EXPECT(len == strlen(payload));
    EXPECT(memcmp(big_buf, payload, len) == 0);

    wal_close(w);
    remove_tmp_dir(dir, path);
}

static void test_sync_is_safe(void) {
    char dir[64];
    make_tmp_dir(dir, sizeof(dir));
    char path[128];
    snprintf(path, sizeof(path), "%s/wal.log", dir);

    wal_status_t st;
    wal_t *w = wal_open(path, &st);
    EXPECT(w != NULL);
    EXPECT(wal_append(w, (const uint8_t *)"data", 4) == WAL_OK);
    EXPECT(wal_sync(w) == WAL_OK);
    wal_close(w);
    remove_tmp_dir(dir, path);
}

int main(void) {
    RUN(test_frame_header_roundtrip);
    RUN(test_crc32_known_vectors);
    RUN(test_append_and_read_back);
    RUN(test_recovery_of_clean_file);
    RUN(test_torn_tail_mid_payload_is_recovered);
    RUN(test_torn_tail_mid_header_is_recovered);
    RUN(test_corrupted_payload_is_dropped_as_torn);
    RUN(test_mid_log_corruption_with_valid_frame_after_is_not_truncated);
    RUN(test_corrupted_last_frame_with_nothing_after_still_truncates);
    RUN(test_append_rejects_oversized_payload);
    RUN(test_read_next_buffer_too_small_then_retry);
    RUN(test_sync_is_safe);

    if (g_failures == 0) {
        printf("\nAll tests passed.\n");
        return 0;
    }
    fprintf(stderr, "\n%d assertion(s) failed.\n", g_failures);
    return 1;
}
