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

wal_status_t wal_recover(const char *path, wal_recover_cb cb, void *ctx,
                          wal_recovery_stats_t *stats) {
    if (!path) {
        return WAL_ERR_INVALID;
    }

    wal_recovery_stats_t local = {0, 0, 0, 0};

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
        off_t remaining = file_size - offset;

        if (remaining < (off_t)WAL_FRAME_HEADER_SIZE) {
            torn = 1;
            break; /* not enough bytes left for even a header: torn tail */
        }

        uint8_t hdr_buf[WAL_FRAME_HEADER_SIZE];
        ssize_t n = pread_full(fd, hdr_buf, WAL_FRAME_HEADER_SIZE, offset);
        if (n < 0) {
            result = WAL_ERR_IO;
            goto out;
        }
        if ((size_t)n != WAL_FRAME_HEADER_SIZE) {
            torn = 1;
            break;
        }

        wal_frame_header_t hdr;
        wal_frame_header_decode(hdr_buf, &hdr);

        if (hdr.length > WAL_MAX_PAYLOAD_SIZE) {
            /* A length field this large is almost certainly a corrupt or
             * torn header rather than a legitimate frame - treat it the
             * same way as a torn tail rather than failing recovery. */
            torn = 1;
            break;
        }

        off_t payload_offset = offset + (off_t)WAL_FRAME_HEADER_SIZE;
        if (payload_offset + (off_t)hdr.length > file_size) {
            torn = 1; /* payload was never fully written */
            break;
        }

        if (hdr.length > scratch_cap) {
            uint8_t *bigger = realloc(scratch, hdr.length);
            if (!bigger) {
                result = WAL_ERR_NOMEM;
                goto out;
            }
            scratch = bigger;
            scratch_cap = hdr.length;
        }

        if (hdr.length > 0) {
            n = pread_full(fd, scratch, hdr.length, payload_offset);
            if (n < 0) {
                result = WAL_ERR_IO;
                goto out;
            }
            if ((size_t)n != hdr.length) {
                torn = 1;
                break;
            }
        }

        uint32_t crc = crc32_compute(scratch, hdr.length);
        if (crc != hdr.crc32) {
            torn = 1; /* bit flip / partial fsync: payload doesn't match its checksum */
            break;
        }

        if (cb) {
            int rc = cb(scratch, hdr.length, ctx);
            if (rc != 0) {
                result = WAL_ERR_INVALID;
                goto out;
            }
        }

        local.frames_recovered++;
        local.bytes_recovered += hdr.length;
        offset = payload_offset + (off_t)hdr.length;
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
