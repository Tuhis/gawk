// AVCC extradata normalization, shared by the viewer's decoder configuration
// (viewer.ts) and the R22 fMP4 muxer (fmp4-muxer.ts) — one normalization,
// two consumers, no dependencies (this must stay a leaf module: the muxer is
// bundled standalone for the e2e muxer check).

import { log } from '../lib/logger';

// Rewrites a possibly out-of-spec avcC record into one Chrome/Safari accept:
// reserved bits forced to 1 (Chrome strictly requires them) and the Firefox
// double-byte SPS/PPS bug undone (the NALU type byte duplicated, shifting the
// real profile_idc — shipped by Firefox broadcasters).
export function normalizeAvccExtradata(extradata: Uint8Array): Uint8Array {
  if (extradata.length < 7 || extradata[0] !== 0x01) return extradata;

  const out: number[] = [];

  // Bytes 0-3: Version, Profile, Compat, Level
  out.push(extradata[0], extradata[1], extradata[2], extradata[3]);

  // Byte 4: Fix reserved bits (set top 6 bits to 1, Chrome strictly requires this)
  out.push(extradata[4] | 0xfc);

  // Byte 5: Fix reserved bits (set top 3 bits to 1)
  const numOfSps = extradata[5] & 0x1f;
  out.push(numOfSps | 0xe0);

  let offset = 6;
  let spsWasBuggy = false;

  // Parse SPS
  for (let i = 0; i < numOfSps; i++) {
    if (offset + 2 > extradata.length) break;
    let len = (extradata[offset] << 8) | extradata[offset + 1];
    offset += 2;
    if (offset + len > extradata.length) break;

    const originalLen = len;
    let naluData = extradata.subarray(offset, offset + len);

    // Detect Firefox double-byte bug: NALU type (e.g. 0x67) is duplicated,
    // shifting the true profile_idc to index 2.
    if (naluData.length > 2 && (naluData[0] & 0x1f) === 7) {
      if (naluData[0] === naluData[1] && naluData[2] === extradata[1]) {
        log.warn('Normalizing Firefox SPS double-byte bug');
        naluData = naluData.subarray(1);
        len -= 1;
        spsWasBuggy = true;
      }
    }

    out.push((len >> 8) & 0xff, len & 0xff);
    for (let j = 0; j < len; j++) out.push(naluData[j]);
    offset += originalLen;
  }

  // Parse PPS
  if (offset < extradata.length) {
    const numOfPps = extradata[offset++];
    out.push(numOfPps);

    for (let i = 0; i < numOfPps; i++) {
      if (offset + 2 > extradata.length) break;
      let len = (extradata[offset] << 8) | extradata[offset + 1];
      offset += 2;
      if (offset + len > extradata.length) break;

      const originalLen = len;
      let naluData = extradata.subarray(offset, offset + len);

      // Fix PPS double-byte bug if SPS was buggy
      if (naluData.length > 2 && (naluData[0] & 0x1f) === 8) {
        if (spsWasBuggy && naluData[0] === naluData[1]) {
          log.warn('Normalizing Firefox PPS double-byte bug');
          naluData = naluData.subarray(1);
          len -= 1;
        }
      }

      out.push((len >> 8) & 0xff, len & 0xff);
      for (let j = 0; j < len; j++) out.push(naluData[j]);
      offset += originalLen;
    }
  }

  return new Uint8Array(out);
}
