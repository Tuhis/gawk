// R30 ST5: the reassembler's per-frame arrival accounting — the stripe
// detector's only input, so its truthfulness IS the detector's truthfulness.

import { describe, expect, it } from 'vitest';

import { computeParity } from './parity';
import { Reassembler } from './reassembler';
import { encodeParityChunk } from './parity';
import { encodeVideoChunk } from './wire';

function chunk(frameId: number, index: number, count: number, payload: Uint8Array): Uint8Array {
  return encodeVideoChunk(
    { keyframe: false, frameId, chunkIndex: index, chunkCount: count, timestampUs: BigInt(frameId) * 1000n },
    payload,
  );
}

function harness() {
  const accounting: Array<[number, number]> = [];
  const r = new Reassembler({
    onConfig: () => {},
    onFrame: () => {},
    onFrameAccounting: (expected, arrived) => accounting.push([expected, arrived]),
  });
  return { r, accounting };
}

describe('reassembler frame accounting (R30)', () => {
  it('reports (expected, expected) for a fully delivered frame', () => {
    const { r, accounting } = harness();
    const p = new Uint8Array([1, 2, 3]);
    for (let i = 0; i < 3; i++) r.push(chunk(1, i, 3, p));
    expect(accounting).toEqual([[3, 3]]);
  });

  it('reports real arrivals for an evicted frame', () => {
    const { r, accounting } = harness();
    const p = new Uint8Array([1]);
    // Frame 1 gets 2 of its 5 chunks, then 8 more frames start and evict it.
    r.push(chunk(1, 0, 5, p));
    r.push(chunk(1, 3, 5, p));
    for (let f = 2; f <= 9; f++) r.push(chunk(f, 0, 2, p));
    expect(accounting).toContainEqual([5, 2]);
  });

  it('counts a parity-recovered chunk as a repair, never a delivery', () => {
    const { r, accounting } = harness();
    // A 3-chunk frame whose middle chunk is lost, repaired by P.
    const payloads = [new Uint8Array([1, 1]), new Uint8Array([2, 2]), new Uint8Array([3, 3])];
    const parity = computeParity(payloads, 1);
    const frameBytes = 6;
    r.push(chunk(7, 0, 3, payloads[0]));
    r.push(chunk(7, 2, 3, payloads[2]));
    r.push(
      encodeParityChunk({ frameId: 7, parityIndex: 0, chunkCount: 3, frameBytes }, parity[0]),
    );
    expect(r.getStats().framesRecoveredByParity).toBe(1);
    // The frame COMPLETED (recovered), but only 2 of 3 chunks were delivered
    // by the network — which is exactly what the stripe detector must see.
    expect(accounting).toEqual([[3, 2]]);
  });

  it('reports duplicates once: a re-delivered chunk is not a second arrival', () => {
    const { r, accounting } = harness();
    const p = new Uint8Array([9]);
    r.push(chunk(4, 0, 2, p));
    r.push(chunk(4, 0, 2, p)); // duplicate (a striping-transition overlap)
    r.push(chunk(4, 1, 2, p));
    expect(accounting).toEqual([[2, 2]]);
  });
});
