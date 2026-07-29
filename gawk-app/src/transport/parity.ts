// Forward parity for the datagram delta path (R29, docs/34) — the TypeScript
// mirror of gawk-server/wire/parity.go. Golden vectors keep the two
// byte-identical; see parity.test.ts.
//
// A delta frame split into n data chunks gets up to two parity symbols:
//
//   P = d0 ^ d1 ^ ... ^ d(n-1)
//   Q = (g^0 * d0) ^ (g^1 * d1) ^ ... ^ (g^(n-1) * d(n-1))    g = 2 in GF(2^8)
//
// The RAID-6 P/Q scheme: MDS for k <= 2 (any two erasures among the n+2
// transmitted chunks reconstruct the frame) from two 256-entry tables.
//
// P alone IS the k=1 code. That prefix property is what lets the relay serve
// every subscriber a prefix of one computation (docs/34 §5.1).

import {
  MAX_CHUNK_PAYLOAD,
  MAX_DATAGRAM_SIZE,
  TYPE_PARITY_CHUNK,
  TYPE_RELAY_CAPABILITIES,
  WIRE_VERSION,
  WireError,
} from './wire';

/**
 * Fixed header size of a ParityChunk datagram.
 *
 * Deliberately 13, not 20: a parity symbol is as long as the longest data
 * chunk (up to MAX_CHUNK_PAYLOAD = 1180), so a 20-byte header carrying a
 * timestamp would produce a 1201-byte datagram and breach MAX_DATAGRAM_SIZE.
 * The cost is that reconstruction needs one surviving data chunk to source
 * the timestamp from — which holds whenever recovery is possible at all.
 */
export const PARITY_CHUNK_HEADER_SIZE = 13;

/** Largest k the P/Q scheme supports. */
export const MAX_PARITY_SYMBOLS = 2;

/**
 * Bounds n. g^i has period 255, so past this the Q coefficients repeat, two
 * chunks share one, and the 2-erasure solve divides by zero — the code
 * silently stops being MDS. An explicit guard, not an assumption.
 */
export const MAX_PARITY_DATA_CHUNKS = 255;

/** Exact size of a RelayCapabilities message. */
export const RELAY_CAPABILITIES_SIZE = 5;

/** The relay understands ParityChunk and filters it per subscriber. */
export const CAP_PARITY_CHUNKS = 1 << 0;

/**
 * The relay accepts ?stripe=N&leg=j subscribe sessions and StripeState
 * datagrams (R30, docs/35). A viewer that never sees this bit never dials a
 * leg, so a new viewer against an old relay stays byte-identical to pre-R30.
 * Capability growth is new bits in this word, never new bytes — the message
 * is parsed strictly by size on both producer mirrors.
 */
export const CAP_STRIPED_DELIVERY = 1 << 1;

// --- GF(2^8), primitive polynomial 0x11D, generator 2 ----------------------

const GF_EXP = new Uint8Array(512);
const GF_LOG = new Uint8Array(256);

{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    GF_EXP[i] = x;
    GF_LOG[x] = i;
    const hi = (x & 0x80) !== 0;
    x = (x << 1) & 0xff;
    if (hi) x ^= 0x1d;
  }
  for (let i = 255; i < 512; i++) GF_EXP[i] = GF_EXP[i - 255];
}

export function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return GF_EXP[GF_LOG[a] + GF_LOG[b]];
}

function gfDiv(a: number, b: number): number {
  if (b === 0) throw new WireError('division by zero in GF(2^8)');
  if (a === 0) return 0;
  return GF_EXP[GF_LOG[a] - GF_LOG[b] + 255];
}

/** g^i for g = 2 — the Q coefficient of data chunk i. */
export function gfPow2(i: number): number {
  return GF_EXP[i % 255];
}

// --- Parity computation ----------------------------------------------------

/**
 * Returns min(k, chunks.length) parity symbols, each as long as the longest
 * chunk (shorter chunks are treated as zero-padded). k = 0 returns []. The
 * input chunks are not modified.
 */
export function computeParity(chunks: Uint8Array[], k: number): Uint8Array<ArrayBuffer>[] {
  if (!Number.isInteger(k) || k < 0 || k > MAX_PARITY_SYMBOLS) {
    throw new WireError(`parity k=${k}, must be an integer in [0, ${MAX_PARITY_SYMBOLS}]`);
  }
  const n = chunks.length;
  if (k === 0 || n === 0) return [];
  if (n > MAX_PARITY_DATA_CHUNKS) {
    throw new WireError(`parity unsupported: ${n} chunks, max ${MAX_PARITY_DATA_CHUNKS}`);
  }
  // n === 1: P duplicates the chunk and a second symbol would duplicate it
  // again, so min(k, n) keeps that from being wire waste.
  const symbols = Math.min(k, n);

  let width = 0;
  for (const c of chunks) if (c.length > width) width = c.length;
  if (width > MAX_CHUNK_PAYLOAD) {
    throw new WireError(`parity unsupported: chunk of ${width} bytes, max ${MAX_CHUNK_PAYLOAD}`);
  }

  const out: Uint8Array<ArrayBuffer>[] = [];
  for (let j = 0; j < symbols; j++) out.push(new Uint8Array(width));
  for (let i = 0; i < n; i++) {
    const c = chunks[i];
    const p = out[0];
    for (let b = 0; b < c.length; b++) p[b] ^= c[b];
    if (symbols > 1) {
      const coeff = gfPow2(i);
      const q = out[1];
      for (let b = 0; b < c.length; b++) q[b] ^= gfMul(coeff, c[b]);
    }
  }
  return out;
}

// --- Recovery --------------------------------------------------------------

/**
 * Reconstructs missing data chunks in place. A null entry means "not
 * received". `frameBytes` is the total encoded frame length — the only thing
 * that says how long the final (short) chunk is.
 *
 * The symbol width comes from a surviving parity symbol rather than being
 * inferred: every symbol is exactly as long as the longest data chunk, and a
 * symbol is present whenever recovery is possible at all.
 *
 * Throws WireError when there are more data erasures than usable parity
 * symbols. That is a routine outcome on a lossy link — callers count it.
 */
export function recoverChunks(
  chunks: (Uint8Array | null)[],
  parity: (Uint8Array | null)[],
  frameBytes: number,
): void {
  const n = chunks.length;
  if (n === 0) throw new WireError('parity recovery: no chunks');
  if (n > MAX_PARITY_DATA_CHUNKS) {
    throw new WireError(`parity recovery: ${n} chunks, max ${MAX_PARITY_DATA_CHUNKS}`);
  }

  const missing: number[] = [];
  for (let i = 0; i < n; i++) if (chunks[i] === null) missing.push(i);
  if (missing.length === 0) return;

  const haveP = parity.length > 0 ? parity[0] : null;
  const haveQ = parity.length > 1 ? parity[1] : null;
  const avail = (haveP ? 1 : 0) + (haveQ ? 1 : 0);
  if (missing.length > avail) {
    throw new WireError(`parity recovery: ${missing.length} data erasures, ${avail} parity symbols`);
  }

  const width = haveP ? haveP.length : haveQ ? haveQ.length : 0;
  if (width === 0) throw new WireError('parity recovery: no parity symbol to size the block');
  if (haveP && haveQ && haveP.length !== haveQ.length) {
    throw new WireError(`parity recovery: symbols disagree on width (${haveP.length} vs ${haveQ.length})`);
  }

  // frameBytes must describe n chunks of this width with a short final one,
  // or the header is lying and reconstruction would emit silent garbage.
  const lastLen = frameBytes - (n - 1) * width;
  if (lastLen <= 0 || lastLen > width) {
    throw new WireError(`parity recovery: frameBytes ${frameBytes} inconsistent with ${n} chunks of width ${width}`);
  }
  for (let i = 0; i < n; i++) {
    const c = chunks[i];
    if (c === null) continue;
    const want = i === n - 1 ? lastLen : width;
    if (c.length !== want) {
      throw new WireError(`parity recovery: chunk ${i} has length ${c.length}, want ${want}`);
    }
  }

  // Work on zero-padded copies so the arithmetic sees a rectangle.
  const padded: Uint8Array[] = [];
  for (let i = 0; i < n; i++) {
    const buf = new Uint8Array(width);
    const c = chunks[i];
    if (c !== null) buf.set(c);
    padded.push(buf);
  }

  if (missing.length === 1) {
    const x = missing[0];
    if (haveP) {
      const acc = new Uint8Array(haveP);
      for (let i = 0; i < n; i++) {
        if (i === x) continue;
        const c = padded[i];
        for (let b = 0; b < width; b++) acc[b] ^= c[b];
      }
      padded[x] = acc;
    } else {
      // Only Q survived: d_x = (Q ^ sum(g^i * d_i, i != x)) / g^x
      const acc = new Uint8Array(haveQ!);
      for (let i = 0; i < n; i++) {
        if (i === x) continue;
        const coeff = gfPow2(i);
        const c = padded[i];
        for (let b = 0; b < width; b++) acc[b] ^= gfMul(coeff, c[b]);
      }
      const inv = gfPow2(x);
      for (let b = 0; b < width; b++) acc[b] = gfDiv(acc[b], inv);
      padded[x] = acc;
    }
  } else {
    const [x, y] = missing;
    const pm = new Uint8Array(haveP!); // d_x ^ d_y
    const qm = new Uint8Array(haveQ!); // g^x*d_x ^ g^y*d_y
    for (let i = 0; i < n; i++) {
      if (i === x || i === y) continue;
      const coeff = gfPow2(i);
      const c = padded[i];
      for (let b = 0; b < width; b++) {
        pm[b] ^= c[b];
        qm[b] ^= gfMul(coeff, c[b]);
      }
    }
    const gx = gfPow2(x);
    const gy = gfPow2(y);
    const den = gx ^ gy;
    if (den === 0) {
      // Unreachable while n <= MAX_PARITY_DATA_CHUNKS — that bound's purpose.
      throw new WireError(`parity recovery: coefficients for chunks ${x} and ${y} collide`);
    }
    const dx = new Uint8Array(width);
    for (let b = 0; b < width; b++) dx[b] = gfDiv(gfMul(gy, pm[b]) ^ qm[b], den);
    const dy = new Uint8Array(width);
    for (let b = 0; b < width; b++) dy[b] = pm[b] ^ dx[b];
    padded[x] = dx;
    padded[y] = dy;
  }

  for (const i of missing) {
    chunks[i] = i === n - 1 ? padded[i].subarray(0, lastLen) : padded[i];
  }
}

// --- ParityChunk wire format ----------------------------------------------

export interface ParityChunkHeader {
  frameId: number;
  parityIndex: number; // 0 = P, 1 = Q
  chunkCount: number; // n, the DATA chunk count of the frame
  frameBytes: number; // total encoded frame length
}

export function encodeParityChunk(header: ParityChunkHeader, payload: Uint8Array): Uint8Array<ArrayBuffer> {
  if (payload.length > MAX_CHUNK_PAYLOAD) {
    throw new WireError(`parity payload too large: ${payload.length} bytes, max ${MAX_CHUNK_PAYLOAD}`);
  }
  if (header.chunkCount === 0 || header.chunkCount > MAX_PARITY_DATA_CHUNKS) {
    throw new WireError(`bad parity chunk count ${header.chunkCount}, max ${MAX_PARITY_DATA_CHUNKS}`);
  }
  if (header.parityIndex < 0 || header.parityIndex >= MAX_PARITY_SYMBOLS) {
    throw new WireError(`bad parity index ${header.parityIndex}, max ${MAX_PARITY_SYMBOLS - 1}`);
  }
  const dgram = new Uint8Array(PARITY_CHUNK_HEADER_SIZE + payload.length);
  const view = new DataView(dgram.buffer);
  dgram[0] = WIRE_VERSION;
  dgram[1] = TYPE_PARITY_CHUNK;
  view.setUint32(2, header.frameId, false);
  dgram[6] = header.parityIndex;
  view.setUint16(7, header.chunkCount, false);
  view.setUint32(9, header.frameBytes, false);
  dgram.set(payload, PARITY_CHUNK_HEADER_SIZE);
  return dgram;
}

export function parseParityChunk(dgram: Uint8Array): { header: ParityChunkHeader; payload: Uint8Array } {
  if (dgram.length < PARITY_CHUNK_HEADER_SIZE) {
    throw new WireError(
      `datagram too short: ${dgram.length} bytes, need at least ${PARITY_CHUNK_HEADER_SIZE} for parity chunk`,
    );
  }
  if (dgram.length > MAX_DATAGRAM_SIZE) {
    throw new WireError(`parity datagram too large: ${dgram.length} bytes, max ${MAX_DATAGRAM_SIZE}`);
  }
  if (dgram[0] !== WIRE_VERSION) throw new WireError(`bad wire version 0x${dgram[0].toString(16)}`);
  if (dgram[1] !== TYPE_PARITY_CHUNK) {
    throw new WireError(`bad type 0x${dgram[1].toString(16)}, want parity chunk 0x0e`);
  }
  const view = new DataView(dgram.buffer, dgram.byteOffset, dgram.byteLength);
  const header: ParityChunkHeader = {
    frameId: view.getUint32(2, false),
    parityIndex: dgram[6],
    chunkCount: view.getUint16(7, false),
    frameBytes: view.getUint32(9, false),
  };
  if (header.chunkCount === 0 || header.chunkCount > MAX_PARITY_DATA_CHUNKS) {
    throw new WireError(`bad parity chunk count ${header.chunkCount}, max ${MAX_PARITY_DATA_CHUNKS}`);
  }
  if (header.parityIndex >= MAX_PARITY_SYMBOLS) {
    throw new WireError(`bad parity index ${header.parityIndex}, max ${MAX_PARITY_SYMBOLS - 1}`);
  }
  return { header, payload: dgram.subarray(PARITY_CHUNK_HEADER_SIZE) };
}

// --- RelayCapabilities wire format -----------------------------------------

export interface RelayCapabilities {
  flags: number;
  parityLevel: number;
}

export function encodeRelayCapabilities(c: RelayCapabilities): Uint8Array<ArrayBuffer> {
  if (c.parityLevel > MAX_PARITY_SYMBOLS) {
    throw new WireError(`bad parity level ${c.parityLevel}, max ${MAX_PARITY_SYMBOLS}`);
  }
  const buf = new Uint8Array(RELAY_CAPABILITIES_SIZE);
  const view = new DataView(buf.buffer);
  buf[0] = WIRE_VERSION;
  buf[1] = TYPE_RELAY_CAPABILITIES;
  view.setUint16(2, c.flags, false);
  buf[4] = c.parityLevel;
  return buf;
}

export function parseRelayCapabilities(b: Uint8Array): RelayCapabilities {
  if (b.length !== RELAY_CAPABILITIES_SIZE) {
    throw new WireError(`relay capabilities: ${b.length} bytes, want exactly ${RELAY_CAPABILITIES_SIZE}`);
  }
  if (b[0] !== WIRE_VERSION) throw new WireError(`bad wire version 0x${b[0].toString(16)}`);
  if (b[1] !== TYPE_RELAY_CAPABILITIES) {
    throw new WireError(`bad type 0x${b[1].toString(16)}, want relay capabilities 0x0f`);
  }
  const view = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const c: RelayCapabilities = { flags: view.getUint16(2, false), parityLevel: b[4] };
  if (c.parityLevel > MAX_PARITY_SYMBOLS) {
    throw new WireError(`bad parity level ${c.parityLevel}, max ${MAX_PARITY_SYMBOLS}`);
  }
  return c;
}
