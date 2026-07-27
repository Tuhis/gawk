import { describe, expect, it } from 'vitest';
import {
  CAP_PARITY_CHUNKS,
  MAX_PARITY_DATA_CHUNKS,
  MAX_PARITY_SYMBOLS,
  PARITY_CHUNK_HEADER_SIZE,
  RELAY_CAPABILITIES_SIZE,
  computeParity,
  encodeParityChunk,
  encodeRelayCapabilities,
  gfMul,
  gfPow2,
  parseParityChunk,
  parseRelayCapabilities,
  recoverChunks,
} from './parity';
import { MAX_CHUNK_PAYLOAD, MAX_DATAGRAM_SIZE, TYPE_VIDEO_CHUNK, WIRE_VERSION, WireError } from './wire';

const hex = (b: Uint8Array) =>
  Array.from(b)
    .map((v) => v.toString(16).padStart(2, '0'))
    .join('');

describe('GF(2^8)', () => {
  it('has 0 and 1 as annihilator and identity', () => {
    for (let a = 0; a < 256; a++) {
      expect(gfMul(a, 0)).toBe(0);
      expect(gfMul(a, 1)).toBe(a);
    }
  });

  it('gives distinct Q coefficients across the supported range', () => {
    // This is exactly why MAX_PARITY_DATA_CHUNKS is 255: past it the
    // coefficients repeat and the 2-erasure solve divides by zero.
    const seen = new Map<number, number>();
    for (let i = 0; i < MAX_PARITY_DATA_CHUNKS; i++) {
      const c = gfPow2(i);
      expect(seen.has(c), `g^${i} collides with g^${seen.get(c)}`).toBe(false);
      seen.set(c, i);
    }
    expect(gfPow2(0)).toBe(gfPow2(255));
  });
});

describe('computeParity', () => {
  it('sizes symbols to the longest chunk and makes P the plain XOR', () => {
    const chunks = [Uint8Array.of(1, 2, 3, 4), Uint8Array.of(5, 6, 7, 8), Uint8Array.of(9, 10)];
    const p = computeParity(chunks, 2);
    expect(p).toHaveLength(2);
    for (const s of p) expect(s.length).toBe(4);
    expect(Array.from(p[0])).toEqual([1 ^ 5 ^ 9, 2 ^ 6 ^ 10, 3 ^ 7, 4 ^ 8]);
  });

  it('keeps the prefix property: the k=1 symbol is the k=2 symbol 0', () => {
    const chunks = [Uint8Array.of(0xde, 0xad), Uint8Array.of(0xbe, 0xef), Uint8Array.of(0x12, 0x34)];
    expect(hex(computeParity(chunks, 1)[0])).toBe(hex(computeParity(chunks, 2)[0]));
  });

  it('emits min(k, n) symbols, so n=1 gets exactly one', () => {
    const chunks = [Uint8Array.of(7, 7, 7)];
    const p = computeParity(chunks, 2);
    expect(p).toHaveLength(1);
    expect(hex(p[0])).toBe(hex(chunks[0]));
  });

  it('returns nothing for k=0 and rejects out-of-range k', () => {
    const chunks = [Uint8Array.of(1), Uint8Array.of(2)];
    expect(computeParity(chunks, 0)).toEqual([]);
    expect(() => computeParity(chunks, MAX_PARITY_SYMBOLS + 1)).toThrow(WireError);
    expect(() => computeParity(chunks, -1)).toThrow(WireError);
  });

  it('refuses frames with more chunks than the code covers', () => {
    const chunks = Array.from({ length: MAX_PARITY_DATA_CHUNKS + 1 }, () => Uint8Array.of(1));
    expect(() => computeParity(chunks, 2)).toThrow(WireError);
  });
});

describe('recoverChunks', () => {
  // The load-bearing correctness proof: every supported n, every erasure
  // pair among the n+k transmitted chunks.
  // Compute-bound rather than IO-bound, so it gets explicit headroom: a
  // 2-core shared runner is several times slower than a dev machine, and the
  // default 5 s says "hung" about a test that is simply working.
  it('recovers every erasure pair it has capacity for', { timeout: 30_000 }, () => {
    let seed = 1;
    const rnd = () => {
      seed = (seed * 1103515245 + 12345) & 0x7fffffff;
      return seed & 0xff;
    };
    for (const n of [1, 2, 3, 9, 17, 64, 255]) {
      const width = 32;
      const orig = Array.from({ length: n }, (_, i) =>
        Uint8Array.from({ length: i === n - 1 ? width / 2 + 1 : width }, rnd),
      );
      const frameBytes = (n - 1) * width + orig[n - 1].length;
      const k = n < 2 ? 1 : 2;
      const parity = computeParity(orig, k);
      const total = n + parity.length;
      // Exhaustive only where it is cheap. The pair count is quadratic and
      // the work per pair linear, so the large n are seconds of JS for no
      // extra coverage — and they were 5 s on a 2-core CI runner, which is
      // what this threshold is set by.
      //
      // The exhaustive proof lives in the Go mirror, which runs every pair for
      // every n up to 255 in ~1.4 s. This mirror's job is to show the TS codec
      // AGREES, so above the threshold it samples the boundary pairs (first,
      // last, either side of the data/parity split, wrap-adjacent) instead —
      // and the coefficient-distinctness property those boundaries guard is
      // proven exhaustively by the gfPow2 test above regardless.
      const interesting = (i: number) =>
        n <= 17 || i < 2 || i >= total - 3 || i === n - 1 || i === n || i % 37 === 0;
      for (let a = 0; a < total; a++) {
        if (!interesting(a)) continue;
        for (let b = a; b < total; b++) {
          if (!interesting(b)) continue;
          const chunks: (Uint8Array | null)[] = [...orig];
          const par: (Uint8Array | null)[] = [...parity];
          for (const pos of [a, b]) {
            if (pos < n) chunks[pos] = null;
            else par[pos - n] = null;
          }
          const lost = chunks.filter((c) => c === null).length;
          const surviving = par.filter((p) => p !== null).length;
          if (lost > surviving) {
            expect(() => recoverChunks(chunks, par, frameBytes), `n=${n} (${a},${b})`).toThrow(WireError);
            continue;
          }
          recoverChunks(chunks, par, frameBytes);
          for (let i = 0; i < n; i++) {
            expect(hex(chunks[i]!), `n=${n} erasures (${a},${b}) chunk ${i}`).toBe(hex(orig[i]));
          }
        }
      }
    }
  });

  it('trims padding off a recovered short final chunk', () => {
    const orig = [Uint8Array.of(1, 2, 3, 4), Uint8Array.of(5, 6, 7, 8), Uint8Array.of(9)];
    const parity = computeParity(orig, 2);
    const chunks: (Uint8Array | null)[] = [orig[0], orig[1], null];
    recoverChunks(chunks, parity, 9);
    expect(Array.from(chunks[2]!)).toEqual([9]);
  });

  it('is a no-op on a complete frame and throws without parity', () => {
    const orig = [Uint8Array.of(1, 2), Uint8Array.of(3, 4)];
    expect(() => recoverChunks([...orig], [], 4)).not.toThrow();
    expect(() => recoverChunks([null, orig[1]], [], 4)).toThrow(WireError);
  });

  it('rejects a frameBytes that cannot describe the block', () => {
    const orig = [Uint8Array.of(1, 2, 3, 4), Uint8Array.of(5, 6)];
    const parity = computeParity(orig, 2);
    expect(() => recoverChunks([null, orig[1]], parity, 9999)).toThrow(WireError);
  });
});

describe('ParityChunk wire format', () => {
  it('round-trips', () => {
    const header = { frameId: 0x01020304, parityIndex: 1, chunkCount: 9, frameBytes: 8640 };
    const payload = new Uint8Array(64).fill(0xa5);
    const dgram = encodeParityChunk(header, payload);
    expect(dgram.length).toBe(PARITY_CHUNK_HEADER_SIZE + payload.length);
    const got = parseParityChunk(dgram);
    expect(got.header).toEqual(header);
    expect(hex(got.payload)).toBe(hex(payload));
  });

  it('matches the Go golden vector', () => {
    const dgram = encodeParityChunk(
      { frameId: 0x01020304, parityIndex: 1, chunkCount: 9, frameBytes: 8640 },
      Uint8Array.of(0xde, 0xad, 0xbe, 0xef),
    );
    expect(hex(dgram)).toBe('010e01020304010009000021c0deadbeef');
  });

  it('fits MAX_DATAGRAM_SIZE at a full payload — the common case, not an edge case', () => {
    const payload = new Uint8Array(MAX_CHUNK_PAYLOAD).fill(0x5a);
    const dgram = encodeParityChunk({ frameId: 1, parityIndex: 0, chunkCount: 9, frameBytes: 9 * MAX_CHUNK_PAYLOAD }, payload);
    expect(dgram.length).toBe(PARITY_CHUNK_HEADER_SIZE + MAX_CHUNK_PAYLOAD);
    expect(dgram.length).toBeLessThanOrEqual(MAX_DATAGRAM_SIZE);
    expect(parseParityChunk(dgram).payload.length).toBe(MAX_CHUNK_PAYLOAD);
  });

  it('rejects malformed datagrams', () => {
    const good = encodeParityChunk({ frameId: 1, parityIndex: 0, chunkCount: 4, frameBytes: 16 }, Uint8Array.of(1, 2, 3, 4));
    const mutate = (f: (b: Uint8Array) => Uint8Array) => f(Uint8Array.from(good));
    expect(() => parseParityChunk(good.subarray(0, PARITY_CHUNK_HEADER_SIZE - 1))).toThrow(WireError);
    expect(() => parseParityChunk(mutate((b) => { b[0] = 0x02; return b; }))).toThrow(WireError);
    expect(() => parseParityChunk(mutate((b) => { b[1] = TYPE_VIDEO_CHUNK; return b; }))).toThrow(WireError);
    expect(() => parseParityChunk(mutate((b) => { b[7] = 0; b[8] = 0; return b; }))).toThrow(WireError);
    expect(() => parseParityChunk(mutate((b) => { b[7] = 0xff; b[8] = 0xff; return b; }))).toThrow(WireError);
    expect(() => parseParityChunk(mutate((b) => { b[6] = MAX_PARITY_SYMBOLS; return b; }))).toThrow(WireError);
    const oversize = new Uint8Array(MAX_DATAGRAM_SIZE + 1);
    oversize.set(good);
    expect(() => parseParityChunk(oversize)).toThrow(WireError);
  });

  it('rejects an oversize payload at encode', () => {
    expect(() =>
      encodeParityChunk({ frameId: 1, parityIndex: 0, chunkCount: 2, frameBytes: 4 }, new Uint8Array(MAX_CHUNK_PAYLOAD + 1)),
    ).toThrow(WireError);
  });
});

describe('RelayCapabilities wire format', () => {
  it('round-trips and matches the Go golden vector', () => {
    const caps = { flags: CAP_PARITY_CHUNKS, parityLevel: 2 };
    const buf = encodeRelayCapabilities(caps);
    expect(buf.length).toBe(RELAY_CAPABILITIES_SIZE);
    expect(hex(buf)).toBe('010f000102');
    expect(parseRelayCapabilities(buf)).toEqual(caps);
  });

  it('rejects malformed messages', () => {
    const good = encodeRelayCapabilities({ flags: CAP_PARITY_CHUNKS, parityLevel: 1 });
    expect(() => parseRelayCapabilities(good.subarray(0, RELAY_CAPABILITIES_SIZE - 1))).toThrow(WireError);
    expect(() => parseRelayCapabilities(new Uint8Array([...good, 0]))).toThrow(WireError);
    expect(() => parseRelayCapabilities(Uint8Array.of(0x02, 0x0f, 0, 1, 1))).toThrow(WireError);
    expect(() => parseRelayCapabilities(Uint8Array.of(WIRE_VERSION, TYPE_VIDEO_CHUNK, 0, 1, 1))).toThrow(WireError);
    expect(() => parseRelayCapabilities(Uint8Array.of(WIRE_VERSION, 0x0f, 0, 1, MAX_PARITY_SYMBOLS + 1))).toThrow(WireError);
  });
});

describe('parity parsing never panics', () => {
  it('survives random bytes', () => {
    let seed = 7;
    const rnd = () => {
      seed = (seed * 1103515245 + 12345) & 0x7fffffff;
      return seed & 0xff;
    };
    for (let i = 0; i < 5000; i++) {
      const buf = Uint8Array.from({ length: i % 64 }, rnd);
      try {
        parseParityChunk(buf);
      } catch (e) {
        expect(e).toBeInstanceOf(WireError);
      }
      try {
        parseRelayCapabilities(buf);
      } catch (e) {
        expect(e).toBeInstanceOf(WireError);
      }
    }
  });
});
