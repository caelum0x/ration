#define _POSIX_C_SOURCE 200809L
#define _DEFAULT_SOURCE

#include "wal.h"
#include "frame.h"
#include "crc32.h"

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

struct wal {
    int fd;
    char *path;
    off_t write_offset; /* end of file / next append position */
    off_t read_offset;  /* independent cursor for wal_read_next */
};

/* ---- small pread/pwrite helpers that loop through short reads/writes
 * and EINTR, so callers never have to think about partial I/O. ---- */

static ssize_t pwrite_full(int fd, const void *buf, size_t count, off_t offset) {
    const uint8_t *p = (const uint8_t *)buf;
    size_t written = 0;
    while (written < count) {
        ssize_t n = pwrite(fd, p + written, count - written, offset + (off_t)written);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        if (n == 0) {
            break; /* shouldn't happen on a regular file; avoid looping forever */
        }
        written += (size_t)n;
    }
    return (ssize_t)written;
}

static ssize_t pread_full(int fd, void *buf, size_t count, off_t offset) {
    uint8_t *p = (uint8_t *)buf;
    size_t total = 0;
    while (total < count) {
        ssize_t n = pread(fd, p + total, count - total, offset + (off_t)total);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        if (n == 0) {
            break; /* EOF */
        }
        total += (size_t)n;
    }
    return (ssize_t)total;
}

static char *dup_path(const char *path) {
    size_t n = strlen(path) + 1;
    char *copy = malloc(n);
    if (!copy) {
        return NULL;
    }
    memcpy(copy, path, n);
    return copy;
}

wal_t *wal_open(const char *path, wal_status_t *out_status) {
    if (!path) {
        if (out_status) {
            *out_status = WAL_ERR_INVALID;
        }
        return NULL;
    }

    int fd = open(path, O_RDWR | O_CREAT, 0644);
    if (fd < 0) {
        if (out_status) {
            *out_status = WAL_ERR_IO;
        }
        return NULL;
    }

    struct stat st;
    if (fstat(fd, &st) != 0) {
        close(fd);
        if (out_status) {
            *out_status = WAL_ERR_IO;
        }
        return NULL;
    }

    wal_t *w = malloc(sizeof(*w));
    if (!w) {
        close(fd);
        if (out_status) {
            *out_status = WAL_ERR_NOMEM;
        }
        return NULL;
    }

    char *path_copy = dup_path(path);
    if (!path_copy) {
        close(fd);
        free(w);
        if (out_status) {
            *out_status = WAL_ERR_NOMEM;
        }
        return NULL;
    }

    w->fd = fd;
    w->path = path_copy;
    w->write_offset = st.st_size;
    w->read_offset = 0;

    if (out_status) {
        *out_status = WAL_OK;
    }
    return w;
}

wal_status_t wal_append(wal_t *w, const uint8_t *payload, uint32_t len) {
    if (!w || (len > 0 && !payload)) {
        return WAL_ERR_INVALID;
    }
    if (len > WAL_MAX_PAYLOAD_SIZE) {
        return WAL_ERR_TOO_LARGE;
    }

    wal_frame_header_t hdr;
    hdr.length = len;
    hdr.crc32 = crc32_compute(payload, len);

    uint8_t hdr_buf[WAL_FRAME_HEADER_SIZE];
    wal_frame_header_encode(&hdr, hdr_buf);

    off_t offset = w->write_offset;

    if (pwrite_full(w->fd, hdr_buf, WAL_FRAME_HEADER_SIZE, offset) != (ssize_t)WAL_FRAME_HEADER_SIZE) {
        return WAL_ERR_IO;
    }
    offset += (off_t)WAL_FRAME_HEADER_SIZE;

    if (len > 0) {
        if (pwrite_full(w->fd, payload, len, offset) != (ssize_t)len) {
            return WAL_ERR_IO;
        }
        offset += (off_t)len;
    }

    if (fsync(w->fd) != 0) {
        return WAL_ERR_IO;
    }

    w->write_offset = offset;
    return WAL_OK;
}

wal_status_t wal_read_next(wal_t *w, uint8_t *buf, uint32_t buf_cap, uint32_t *out_len) {
    if (!w || !out_len || (buf_cap > 0 && !buf)) {
        return WAL_ERR_INVALID;
    }

    if (w->read_offset >= w->write_offset) {
        return WAL_ERR_EOF;
    }

    uint8_t hdr_buf[WAL_FRAME_HEADER_SIZE];
    ssize_t n = pread_full(w->fd, hdr_buf, WAL_FRAME_HEADER_SIZE, w->read_offset);
    if (n < 0) {
        return WAL_ERR_IO;
    }
    if ((size_t)n != WAL_FRAME_HEADER_SIZE) {
        return WAL_ERR_CORRUPT;
    }

    wal_frame_header_t hdr;
    wal_frame_header_decode(hdr_buf, &hdr);

    if (hdr.length > WAL_MAX_PAYLOAD_SIZE) {
        return WAL_ERR_CORRUPT;
    }

    off_t payload_offset = w->read_offset + (off_t)WAL_FRAME_HEADER_SIZE;
    if (payload_offset + (off_t)hdr.length > w->write_offset) {
        return WAL_ERR_CORRUPT;
    }

    if (hdr.length > buf_cap) {
        return WAL_ERR_BUFFER_SMALL; /* cursor untouched; caller can retry with a bigger buffer */
    }

    if (hdr.length > 0) {
        n = pread_full(w->fd, buf, hdr.length, payload_offset);
        if (n < 0) {
            return WAL_ERR_IO;
        }
        if ((size_t)n != hdr.length) {
            return WAL_ERR_CORRUPT;
        }
    }

    uint32_t crc = crc32_compute(buf, hdr.length);
    if (crc != hdr.crc32) {
        return WAL_ERR_CORRUPT;
    }

    w->read_offset = payload_offset + (off_t)hdr.length;
    *out_len = hdr.length;
    return WAL_OK;
}

wal_status_t wal_sync(wal_t *w) {
    if (!w) {
        return WAL_ERR_INVALID;
    }
    if (fsync(w->fd) != 0) {
        return WAL_ERR_IO;
    }
    return WAL_OK;
}

void wal_close(wal_t *w) {
    if (!w) {
        return;
    }
    close(w->fd);
    free(w->path);
    free(w);
}

/* Verdict for a single frame-parse attempt at a given offset, shared by
 * wal_recover()'s main scan and by suffix_is_fully_valid()'s lookahead
 * below - both need to answer exactly the same question ("is there a
 * well-formed frame right here?") so they share one implementation
 * rather than risking the two subtly diverging over time. */
typedef enum {
    FRAME_PARSE_OK = 0,       /* header + payload present, CRC matches */
    FRAME_PARSE_TORN,         /* incomplete header/payload, or an implausible
                                 * length field - consistent with a crash
                                 * mid-write at exactly this position */
    FRAME_PARSE_BAD_CRC,      /* header + full payload present on disk, but
                                 * the checksum doesn't match - NOT a shape a
                                 * crash can produce if anything follows it */
    FRAME_PARSE_NOMEM,        /* realloc() failed growing the scratch buffer */
    FRAME_PARSE_IO,           /* pread() failed with a real I/O error */
} frame_parse_result_t;

/* Attempts to parse exactly one frame starting at `offset`, where the
 * file is known to be `file_size` bytes total. On FRAME_PARSE_OK or
 * FRAME_PARSE_BAD_CRC, `*out_len` and `*out_next_offset` are always set
 * (BAD_CRC still knows the frame's claimed length and therefore where
 * the next frame would start, since it's the payload *content* that
 * failed validation, not the length field). `*payload_buf`/`*payload_cap`
 * are the caller's reusable scratch buffer, grown via realloc as needed. */
static frame_parse_result_t parse_frame_at(int fd, off_t offset, off_t file_size,
                                            uint8_t **payload_buf, uint32_t *payload_cap,
                                            uint32_t *out_len, off_t *out_next_offset) {
    off_t remaining = file_size - offset;
    if (remaining < (off_t)WAL_FRAME_HEADER_SIZE) {
        return FRAME_PARSE_TORN; /* not enough bytes left for even a header */
    }

    uint8_t hdr_buf[WAL_FRAME_HEADER_SIZE];
    ssize_t n = pread_full(fd, hdr_buf, WAL_FRAME_HEADER_SIZE, offset);
    if (n < 0) {
        return FRAME_PARSE_IO;
    }
    if ((size_t)n != WAL_FRAME_HEADER_SIZE) {
        return FRAME_PARSE_TORN;
    }

    wal_frame_header_t hdr;
    wal_frame_header_decode(hdr_buf, &hdr);

    if (hdr.length > WAL_MAX_PAYLOAD_SIZE) {
        /* A length field this large is almost certainly a corrupt or
         * torn header rather than a legitimate frame - treat it the
         * same way as a torn tail rather than failing recovery. */
        return FRAME_PARSE_TORN;
    }

    off_t payload_offset = offset + (off_t)WAL_FRAME_HEADER_SIZE;
    if (payload_offset + (off_t)hdr.length > file_size) {
        return FRAME_PARSE_TORN; /* payload was never fully written */
    }

    if (hdr.length > *payload_cap) {
        uint8_t *bigger = realloc(*payload_buf, hdr.length);
        if (!bigger) {
            return FRAME_PARSE_NOMEM;
        }
        *payload_buf = bigger;
        *payload_cap = hdr.length;
    }

    if (hdr.length > 0) {
        n = pread_full(fd, *payload_buf, hdr.length, payload_offset);
        if (n < 0) {
            return FRAME_PARSE_IO;
        }
        if ((size_t)n != hdr.length) {
            return FRAME_PARSE_TORN;
        }
    }

    *out_len = hdr.length;
    *out_next_offset = payload_offset + (off_t)hdr.length;

    uint32_t crc = crc32_compute(*payload_buf, hdr.length);
    if (crc != hdr.crc32) {
        return FRAME_PARSE_BAD_CRC; /* bit flip / partial fsync: payload doesn't match its checksum */
    }
    return FRAME_PARSE_OK;
}

/* Answers "could [offset, file_size) legitimately be the result of
 * normal, successful appends?" - i.e. does it parse as a complete chain
 * of CRC-valid frames with nothing left over. Used to tell a genuine
 * crash-torn tail (nothing of value follows it) apart from mid-log
 * corruption (good frames follow, so truncating here would destroy
 * them). On an allocation/IO failure while scanning ahead, sets
 * `*hard_err` to the corresponding wal_status_t and returns 0; callers
 * must check `*hard_err` before trusting a 0 return as "not valid"
 * rather than "couldn't tell". */
static int suffix_is_fully_valid(int fd, off_t offset, off_t file_size,
                                  uint8_t **scratch, uint32_t *scratch_cap,
                                  wal_status_t *hard_err) {
    *hard_err = WAL_OK;
    while (offset < file_size) {
        uint32_t frame_len = 0;
        off_t next_offset = 0;
        frame_parse_result_t pr =
            parse_frame_at(fd, offset, file_size, scratch, scratch_cap, &frame_len, &next_offset);

        if (pr == FRAME_PARSE_IO) {
            *hard_err = WAL_ERR_IO;
            return 0;
        }
        if (pr == FRAME_PARSE_NOMEM) {
            *hard_err = WAL_ERR_NOMEM;
            return 0;
        }
        if (pr != FRAME_PARSE_OK) {
            return 0; /* torn or bad-CRC frame in the lookahead window: not a clean suffix */
        }
        offset = next_offset;
    }
    return offset == file_size;
}

wal_status_t wal_recover(const char *path, wal_recover_cb cb, void *ctx,
                          wal_recovery_stats_t *stats) {
    if (!path) {
        return WAL_ERR_INVALID;
    }

    wal_recovery_stats_t local = {0};

    int fd = open(path, O_RDWR | O_CREAT, 0644);
    if (fd < 0) {
        return WAL_ERR_IO;
    }

    struct stat st;
    if (fstat(fd, &st) != 0) {
        close(fd);
        return WAL_ERR_IO;
    }
    off_t file_size = st.st_size;

    uint8_t *scratch = NULL;
    uint32_t scratch_cap = 0;

    off_t offset = 0;
    wal_status_t result = WAL_OK;
    int torn = 0;

    while (offset < file_size) {
        uint32_t frame_len = 0;
        off_t next_offset = 0;
        frame_parse_result_t pr =
            parse_frame_at(fd, offset, file_size, &scratch, &scratch_cap, &frame_len, &next_offset);

        if (pr == FRAME_PARSE_IO) {
            result = WAL_ERR_IO;
            goto out;
        }
        if (pr == FRAME_PARSE_NOMEM) {
            result = WAL_ERR_NOMEM;
            goto out;
        }
        if (pr == FRAME_PARSE_TORN) {
            torn = 1;
            break;
        }
        if (pr == FRAME_PARSE_BAD_CRC) {
            /* Ambiguous on its own: a bad-CRC frame could be (1) a
             * genuine crash-torn tail, if nothing of value follows it,
             * or (2) non-crash corruption (disk bit rot, a stray write)
             * of a frame that is NOT the last one, if good frames exist
             * after it - a real crash cannot produce case 2, since
             * nothing runs after a crash to write more good frames past
             * a torn one. Only resolve to case 2 (refuse to truncate)
             * when there is in fact more data after this frame AND that
             * data is a fully valid frame chain; otherwise fall through
             * to the ordinary torn-tail handling below, exactly as
             * before this distinction existed. */
            if (next_offset < file_size) {
                wal_status_t lookahead_err = WAL_OK;
                int rest_is_valid = suffix_is_fully_valid(fd, next_offset, file_size, &scratch,
                                                           &scratch_cap, &lookahead_err);
                if (lookahead_err != WAL_OK) {
                    result = lookahead_err;
                    goto out;
                }
                if (rest_is_valid) {
                    local.corrupt_frame_offset = (size_t)offset;
                    result = WAL_ERR_CORRUPT_MID_LOG;
                    goto out; /* deliberately do NOT truncate: leave every byte on disk untouched */
                }
            }
            torn = 1;
            break;
        }

        /* FRAME_PARSE_OK */
        if (cb) {
            int rc = cb(scratch, frame_len, ctx);
            if (rc != 0) {
                result = WAL_ERR_INVALID;
                goto out;
            }
        }

        local.frames_recovered++;
        local.bytes_recovered += frame_len;
        offset = next_offset;
    }

    if (torn) {
        local.torn_tail_dropped = 1;
        local.torn_bytes_dropped = (size_t)(file_size - offset);

        if (ftruncate(fd, offset) != 0) {
            result = WAL_ERR_IO;
            goto out;
        }
        if (fsync(fd) != 0) {
            result = WAL_ERR_IO;
            goto out;
        }
    }

out:
    free(scratch);
    close(fd);
    if (stats) {
        *stats = local;
    }
    return result;
}
