#ifndef RATION_WAL_H
#define RATION_WAL_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Crash-safe write-ahead log.
 *
 * Format: an append-only sequence of length-prefixed, CRC32-checksummed
 * frames (see frame.h). Every wal_append() fsync()s the file before
 * returning, so a successful append is durable on disk even if the
 * process is killed immediately afterward.
 *
 * Crash recovery: a process can die mid-append, leaving a "torn" frame at
 * the tail of the file (a partially written header and/or payload, or a
 * fully-written-but-not-fsynced frame with a bad CRC). wal_recover()
 * detects this by CRC/length validation rather than treating it as a
 * fatal error: every frame up to the first invalid one is replayed via
 * the callback, and the torn tail is truncated off the file so the log
 * is left clean and ready for further appends.
 */

typedef struct wal wal_t;

typedef enum {
    WAL_OK = 0,
    WAL_ERR_IO = -1,            /* open/read/write/fsync/truncate failed */
    WAL_ERR_INVALID = -2,       /* bad arguments, or recovery callback aborted */
    WAL_ERR_CORRUPT = -3,       /* frame failed structural/CRC validation */
    WAL_ERR_EOF = -4,           /* wal_read_next: no more frames to read */
    WAL_ERR_TOO_LARGE = -5,     /* payload exceeds WAL_MAX_PAYLOAD_SIZE */
    WAL_ERR_NOMEM = -6,         /* allocation failure */
    WAL_ERR_BUFFER_SMALL = -7,  /* wal_read_next: caller buffer too small */
} wal_status_t;

typedef struct {
    size_t frames_recovered;    /* valid frames replayed, in append order */
    size_t bytes_recovered;     /* sum of payload bytes across those frames */
    int torn_tail_dropped;      /* 1 if a trailing invalid frame was discarded */
    size_t torn_bytes_dropped;  /* bytes truncated off the file for the torn tail */
} wal_recovery_stats_t;

/* Invoked once per valid frame during wal_recover(), in append order.
 * `payload` is valid only for the duration of the call. Return nonzero to
 * abort recovery early (wal_recover then returns WAL_ERR_INVALID; frames
 * already delivered to the callback are unaffected, but nothing after
 * the aborted frame is truncated or replayed). */
typedef int (*wal_recover_cb)(const uint8_t *payload, uint32_t len, void *ctx);

/* Open (creating if necessary) the WAL file at `path` for append/read.
 * The write cursor is positioned at the current end of file - callers
 * should run wal_recover() on `path` first so that end-of-file is a
 * clean frame boundary, not a torn tail. Returns NULL on failure and, if
 * `out_status` is non-NULL, stores the reason. */
wal_t *wal_open(const char *path, wal_status_t *out_status);

/* Append one frame containing `payload[0..len)`. Computes the CRC,
 * writes the header+payload, and fsync()s before returning WAL_OK. On
 * any failure the file is left unchanged from the caller's point of view
 * (either nothing was written, or what was written will be cleaned up by
 * a future wal_recover() as a torn tail). */
wal_status_t wal_append(wal_t *w, const uint8_t *payload, uint32_t len);

/* Read the next frame sequentially (independent cursor from the append
 * position) into `buf` (capacity `buf_cap`). On WAL_OK, `*out_len` is set
 * to the payload length actually written to `buf`. Returns WAL_ERR_EOF
 * once all appended frames have been read. Returns WAL_ERR_BUFFER_SMALL
 * (without advancing the cursor) if `buf_cap` is smaller than the next
 * frame's payload - retry with a larger buffer. */
wal_status_t wal_read_next(wal_t *w, uint8_t *buf, uint32_t buf_cap, uint32_t *out_len);

/* Force any buffered data to stable storage. wal_append() already does
 * this internally after every write, so this is mainly useful for tests
 * and callers that want an explicit durability checkpoint. */
wal_status_t wal_sync(wal_t *w);

/* Close the handle and release associated resources. No-op if `w` is NULL. */
void wal_close(wal_t *w);

/* Scan the WAL file at `path` from the beginning, validating and
 * replaying every well-formed frame via `cb`. If the file ends with a
 * torn frame (incomplete header, incomplete payload, or a CRC mismatch),
 * that frame and anything after it is discarded: the file is truncated
 * back to the end of the last valid frame so it is immediately safe to
 * wal_open() and keep appending to. `stats`, if non-NULL, is filled in
 * with what was recovered. Intended to be called once at startup, before
 * wal_open(). */
wal_status_t wal_recover(const char *path, wal_recover_cb cb, void *ctx,
                          wal_recovery_stats_t *stats);

#ifdef __cplusplus
}
#endif

#endif /* RATION_WAL_H */
