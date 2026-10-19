#include "crc32.h"

/*
 * Standard reflected CRC-32 (polynomial 0xEDB88320, the IEEE 802.3 / zlib
 * / gzip variant), computed with a 256-entry table built on the stack on
 * every call. Building the table costs 256 * 8 cheap iterations (a few
 * hundred ns) which is negligible next to the fsync() that follows every
 * WAL append, and it keeps this file free of any shared mutable state -
 * no lazy-init races, no global table to reason about.
 */
uint32_t crc32_compute(const uint8_t *data, size_t len) {
    static const uint32_t poly = 0xEDB88320u;
    uint32_t table[256];

    for (uint32_t i = 0; i < 256; i++) {
        uint32_t c = i;
        for (int k = 0; k < 8; k++) {
            c = (c & 1u) ? (poly ^ (c >> 1)) : (c >> 1);
        }
        table[i] = c;
    }

    uint32_t crc = 0xFFFFFFFFu;
    for (size_t i = 0; i < len; i++) {
        crc = table[(crc ^ data[i]) & 0xFFu] ^ (crc >> 8);
    }
    return crc ^ 0xFFFFFFFFu;
}
