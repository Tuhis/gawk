import { describe, expect, it } from 'vitest';

import { packetizeFrame, packetizeFrameWithParity } from './packetizer';
import { computeParity, parseParityChunk, recoverChunks } from './parity';
import { MAX_CHUNK_PAYLOAD, parseVideoChunk } from './wire';

function patternBytes(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  for (let i = 0; i < length; i++) out[i] = (i * 31 + seed) & 0xff;
  return out;
}

const info = { frameId: 42, keyframe: false, timestampUs: 123_456n };

describe('packetizeFrameWithParity', () => {
  it('is byte-identical to packetizeFrame at k=0', () => {
    const data = patternBytes(5000, 3);
    const plain = packetizeFrame(info, data);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 0);
    expect(parity).toEqual([]);
    expect(datagrams).toHaveLength(plain.length);
    for (let i = 0; i < plain.length; i++) {
      expect(Array.from(datagrams[i])).toEqual(Array.from(plain[i]));
    }
  });

  it('appends k parity datagrams carrying the frame shape', () => {
    const data = patternBytes(5000, 4);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    expect(parity).toHaveLength(2);
    for (let i = 0; i < parity.length; i++) {
      const { header } = parseParityChunk(parity[i]);
      expect(header.frameId).toBe(info.frameId);
      expect(header.parityIndex).toBe(i);
      expect(header.chunkCount).toBe(datagrams.length);
      expect(header.frameBytes).toBe(data.length);
    }
  });

  it('computes parity over chunk PAYLOADS, not whole datagrams', () => {
    // The viewer reassembles payloads, so parity must cover payloads. Getting
    // this wrong would still round-trip through a happy-path test and fail
    // only under actual loss.
    const data = patternBytes(3000, 9);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    const payloads = datagrams.map((d) => parseVideoChunk(d).payload);
    const expected = computeParity(payloads, 2);
    for (let i = 0; i < 2; i++) {
      expect(Array.from(parseParityChunk(parity[i]).payload)).toEqual(Array.from(expected[i]));
    }
  });

  it('produces parity that actually repairs two lost chunks end to end', () => {
    const data = patternBytes(5000, 11);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    const payloads: (Uint8Array | null)[] = datagrams.map((d) => parseVideoChunk(d).payload);
    const n = payloads.length;
    expect(n).toBeGreaterThan(2);
    payloads[1] = null;
    payloads[n - 1] = null; // include the short final chunk
    const symbols = parity.map((p) => parseParityChunk(p).payload);
    recoverChunks(payloads, symbols, data.length);
    const rebuilt = new Uint8Array(data.length);
    let off = 0;
    for (const p of payloads) {
      rebuilt.set(p!, off);
      off += p!.length;
    }
    expect(Array.from(rebuilt)).toEqual(Array.from(data));
  });

  it('emits one symbol for a single-chunk frame', () => {
    const data = patternBytes(10, 5);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    expect(datagrams).toHaveLength(1);
    expect(parity).toHaveLength(1);
  });

  it('emits no parity when the frame needs more chunks than the code covers', () => {
    // Beyond 255 chunks the Q coefficients wrap and the code stops being MDS.
    // A frame that large must degrade to plain datagrams, not throw.
    const data = patternBytes(MAX_CHUNK_PAYLOAD * 300, 6);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    expect(datagrams.length).toBeGreaterThan(255);
    expect(parity).toEqual([]);
  });

  it('never exceeds the datagram cap, even at a full final chunk', () => {
    const data = patternBytes(MAX_CHUNK_PAYLOAD * 9, 7);
    const { datagrams, parity } = packetizeFrameWithParity(info, data, 2);
    for (const d of [...datagrams, ...parity]) expect(d.length).toBeLessThanOrEqual(1200);
  });
});
