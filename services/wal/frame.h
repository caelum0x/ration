#ifndef RATION_WAL_FRAME_H
#define RATION_WAL_FRAME_H

/*
 * On-disk frame layout for the write-ahead log.
 *
 * Every record in the log file is a single "frame":
 *
 *   +-------------------+-------------------+----------------------+
 *   | length  (4 bytes) | crc32   (4 bytes) | payload (length bytes)|
 *   +-------------------+-------------------+----------------------+
 *
 * - length: number of payload bytes that follow the header. Does NOT
 *   include the 8-byte header itself. Encoded as an unsigned 32-bit
 *   little-endian integer.
 * - crc32:  CRC-32 (IEEE 802.3 / zlib polynomial 0xEDB88320) of the
 *   payload bytes only (the header itself is not checksummed - it is
 *   validated structurally instead, e.g. length vs. remaining file size).
 *   Encoded as an unsigned 32-bit little-endian integer.
 * - payload: exactly `length` opaque bytes, caller-defined content.
 *
 * The header is always exactly WAL_FRAME_HEADER_SIZE (8) bytes, written
 * as two raw little-endian uint32 fields rather than a padded C struct,
 * so the on-disk format never depends on compiler struct layout,
 * alignment, or host byte order. wal_frame_header_encode/decode are the
 * only place that convert between the in-memory representation and the
 * on-disk bytes.
 */

#include <stdint.h>

#define WAL_FRAME_HEADER_SIZE 8u

/* Sanity cap on a single frame's payload size. This is deliberately well
 * below UINT32_MAX so a corrupted length field is very likely to be
 * rejected immediately instead of causing a huge allocation attempt. */
#define WAL_MAX_PAYLOAD_SIZE (64u * 1024u * 1024u) /* 64 MiB */

typedef struct {
    uint32_t length;
    uint32_t crc32;
} wal_frame_header_t;

/* Encode `hdr` into the on-disk byte layout described above. `out` must
 * point to at least WAL_FRAME_HEADER_SIZE bytes. */
static inline void wal_frame_header_encode(const wal_frame_header_t *hdr,
                                            uint8_t out[WAL_FRAME_HEADER_SIZE]) {
    uint32_t length = hdr->length;
    uint32_t crc = hdr->crc32;

    out[0] = (uint8_t)(length & 0xFFu);
    out[1] = (uint8_t)((length >> 8) & 0xFFu);
    out[2] = (uint8_t)((length >> 16) & 0xFFu);
    out[3] = (uint8_t)((length >> 24) & 0xFFu);

    out[4] = (uint8_t)(crc & 0xFFu);
    out[5] = (uint8_t)((crc >> 8) & 0xFFu);
    out[6] = (uint8_t)((crc >> 16) & 0xFFu);
    out[7] = (uint8_t)((crc >> 24) & 0xFFu);
}

/* Decode WAL_FRAME_HEADER_SIZE bytes of on-disk header into `hdr`. */
static inline void wal_frame_header_decode(const uint8_t in[WAL_FRAME_HEADER_SIZE],
                                            wal_frame_header_t *hdr) {
    hdr->length = (uint32_t)in[0] | ((uint32_t)in[1] << 8) |
                  ((uint32_t)in[2] << 16) | ((uint32_t)in[3] << 24);

    hdr->crc32 = (uint32_t)in[4] | ((uint32_t)in[5] << 8) |
                 ((uint32_t)in[6] << 16) | ((uint32_t)in[7] << 24);
}

#endif /* RATION_WAL_FRAME_H */
