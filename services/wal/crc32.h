#ifndef RATION_WAL_CRC32_H
#define RATION_WAL_CRC32_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* CRC-32 (IEEE 802.3 / zlib / "CRC-32/ISO-HDLC") checksum of data[0..len).
 * Standalone, no external dependencies (not linked against zlib). Matches
 * the well-known check value: crc32_compute("123456789", 9) == 0xCBF43926.
 */
uint32_t crc32_compute(const uint8_t *data, size_t len);

#ifdef __cplusplus
}
#endif

#endif /* RATION_WAL_CRC32_H */
