import { describe, expect, it } from 'vitest';

import { packetizeFrameWithParity } from './packetizer';
import { Reassembler, type AssembledFrame } from './reassembler';

function patternBytes(length: number, seed: number): Uint8Array {
  const out = new Uint8Array(length);
  for (let i = 0; i < length; i++) out[i] = (i * 31 + seed) & 0xff;
  return out;
}

function collector() {
  const frames: AssembledFrame[] = [];
  const r = new Reassembler({ onFrame: (f) => frames.push(f), onConfig: () => {} });
  return { r, frames };
}

const info = (frameId: number) => ({ frameId, keyframe: false, timestampUs: BigInt(frameId) * 1000n });

/** Feeds every datagram except the data-chunk indices in `drop`. */
function feed(r: Reassembler, frameId: number, data: Uint8Array, k: number, drop: number[]) {
  const { datagrams, parity } = packetizeFrameWithParity(info(frameId), data, k);
  datagrams.forEach((d, i) => {
    if (!drop.includes(i)) r.push(d);
  });
  for (const p of parity) r.push(p);
  return datagrams.length;
}

describe('Reassembler parity recovery', () => {
  it('recovers a frame missing one chunk', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 1);
    feed(r, 10, data, 2, [2]);
    expect(frames).toHaveLength(1);
    expect(Array.from(frames[0].data)).toEqual(Array.from(data));
    expect(r.getStats().framesRecoveredByParity).toBe(1);
    expect(r.getStats().framesDroppedIncomplete).toBe(0);
  });

  it('recovers a frame missing two chunks at k=2', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 2);
    const n = feed(r, 11, data, 2, [0, 3]);
    expect(n).toBeGreaterThan(3);
    expect(frames).toHaveLength(1);
    expect(Array.from(frames[0].data)).toEqual(Array.from(data));
    expect(r.getStats().framesRecoveredByParity).toBe(1);
  });

  it('recovers when the LAST (short) chunk is the missing one', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 3);
    const { datagrams, parity } = packetizeFrameWithParity(info(12), data, 2);
    datagrams.slice(0, -1).forEach((d) => r.push(d));
    for (const p of parity) r.push(p);
    expect(frames).toHaveLength(1);
    expect(Array.from(frames[0].data)).toEqual(Array.from(data));
  });

  it('cannot recover two losses at k=1, and counts the failure', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 4);
    feed(r, 13, data, 1, [1, 2]);
    expect(frames).toHaveLength(0);
    expect(r.getStats().framesRecoveredByParity).toBe(0);
    // The frame is still held (a straggler could still complete it); the
    // reassembler must not have thrown.
    expect(r.getStats().badDatagrams).toBe(0);
  });

  it('emits the frame without parity when nothing was lost', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 5);
    feed(r, 14, data, 2, []);
    expect(frames).toHaveLength(1);
    expect(r.getStats().framesRecoveredByParity).toBe(0);
    expect(r.getStats().parityChunksReceived).toBe(2);
  });

  it('ignores parity that arrives after the frame already completed', () => {
    const { r, frames } = collector();
    const data = patternBytes(3000, 6);
    const { datagrams, parity } = packetizeFrameWithParity(info(15), data, 2);
    for (const d of datagrams) r.push(d);
    expect(frames).toHaveLength(1);
    for (const p of parity) r.push(p);
    expect(frames).toHaveLength(1);
    expect(r.getStats().badDatagrams).toBe(0);
  });

  it('does not inflate framesDroppedIncomplete on a CLEAN link', () => {
    // The bug this pins, caught by the e2e telemetry pass rather than by the
    // test above: the producer sends parity AFTER the frame's data chunks, so
    // on a lossless link every frame completes and THEN its parity arrives.
    // Creating an assembly for that parity leaves a frame that can never
    // complete, which later evicts as "incomplete" — 832 phantom drops on a
    // loopback e2e run, inflating the exact counter R29's whole diagnosis
    // rests on and making diagnose() call a perfect session bursty.
    //
    // Asserting "frames still 1" was not enough to catch it. The counter is.
    const { r, frames } = collector();
    for (let i = 1; i <= 40; i++) {
      const { datagrams, parity } = packetizeFrameWithParity(info(i), patternBytes(3000, i), 2);
      for (const d of datagrams) r.push(d);
      for (const p of parity) r.push(p);
    }
    expect(frames).toHaveLength(40);
    expect(r.getStats().framesDroppedIncomplete).toBe(0);
    expect(r.getStats().framesRecoveredByParity).toBe(0);
  });

  it('still attaches parity for a frame that has NOT been emitted yet', () => {
    // The guard above must key on the emitted watermark, not on "have I seen
    // this frame" — parity legitimately outruns its data chunks under reorder.
    const { r, frames } = collector();
    const data = patternBytes(5000, 21);
    const { datagrams, parity } = packetizeFrameWithParity(info(30), data, 2);
    for (const p of parity) r.push(p);
    datagrams.forEach((d, i) => {
      if (i !== 0) r.push(d);
    });
    expect(frames).toHaveLength(1);
    expect(Array.from(frames[0].data)).toEqual(Array.from(data));
  });

  it('recovers when parity arrives BEFORE the surviving chunks', () => {
    // Ordering is not guaranteed on a lossy link, and the producer sends
    // parity last — so out-of-order arrival is the normal case under reorder,
    // not an edge case.
    const { r, frames } = collector();
    const data = patternBytes(5000, 7);
    const { datagrams, parity } = packetizeFrameWithParity(info(16), data, 2);
    for (const p of parity) r.push(p);
    datagrams.forEach((d, i) => {
      if (i !== 1) r.push(d);
    });
    expect(frames).toHaveLength(1);
    expect(Array.from(frames[0].data)).toEqual(Array.from(data));
  });

  it('counts parity chunks but never treats them as bad datagrams', () => {
    const { r } = collector();
    const data = patternBytes(2000, 8);
    feed(r, 17, data, 2, []);
    expect(r.getStats().parityChunksReceived).toBe(2);
    expect(r.getStats().badDatagrams).toBe(0);
  });

  it('is unaffected when no parity is sent at all (pre-R29 behaviour)', () => {
    const { r, frames } = collector();
    const data = patternBytes(5000, 9);
    feed(r, 18, data, 0, [2]);
    expect(frames).toHaveLength(0);
    expect(r.getStats().parityChunksReceived).toBe(0);
    expect(r.getStats().framesRecoveredByParity).toBe(0);
  });

  it('survives a parity chunk whose frame it has never seen', () => {
    const { r } = collector();
    const { parity } = packetizeFrameWithParity(info(19), patternBytes(3000, 10), 2);
    for (const p of parity) r.push(p);
    expect(r.getStats().badDatagrams).toBe(0);
    expect(r.getStats().parityChunksReceived).toBe(2);
  });
});
