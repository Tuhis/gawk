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
    // Deferred (docs/35 §12 finding 2): a recovered frame's report waits out
    // the leg-skew window, because the "lost" chunk may merely be in flight
    // on a slower stripe leg. Here it never arrives — genuine loss — so when
    // the watermark rolls past, the report says exactly that: 2 of 3
    // delivered, the repair never counted as a delivery.
    expect(accounting).toEqual([]);
    for (let f = 100; f < 120; f++) r.push(chunk(f, 0, 1, new Uint8Array([9])));
    expect(accounting).toContainEqual([3, 2]);
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

// docs/35 §12 finding 2: eager parity recovery (R29's "eager by design")
// races the slowest stripe leg — a frame completes by recovery while its
// last chunks are merely in flight, and those stragglers then arrive to a
// deleted assembly. Two defects fall out, both found by the striped e2e
// pass on a ZERO-loss loopback: the stragglers created phantom assemblies
// that died as framesDroppedIncomplete (~130/window against a 30 fps
// stream), and the recovered frame's accounting reported its raced chunks
// as lost, which would falsely fire burst-threshold-loss on healthy
// striped sessions.
describe('post-recovery stragglers (docs/35 §12 finding 2)', () => {
  it('a straggler behind the emit watermark creates no phantom assembly', () => {
    const { r } = harness();
    const p = new Uint8Array([1, 2]);
    for (let i = 0; i < 3; i++) r.push(chunk(10, i, 3, p));
    expect(r.getStats().framesCompleted).toBe(1);
    // The straggler: a chunk of the already-emitted frame re-arrives (the
    // striping-transition overlap shape, or a slow leg after recovery).
    r.push(chunk(10, 1, 3, p));
    expect(r.getStats().staleChunks).toBe(1);
    // Fill the assembly table with 8 fresh frames: if the straggler built a
    // phantom, the 8th insert evicts it as framesDroppedIncomplete.
    for (let f = 11; f <= 18; f++) r.push(chunk(f, 0, 2, p));
    expect(r.getStats().framesDroppedIncomplete).toBe(0);
  });

  it('credits stragglers of a recovered frame before reporting its accounting', () => {
    const { r, accounting } = harness();
    const payloads = [new Uint8Array([1, 1]), new Uint8Array([2, 2]), new Uint8Array([3, 3])];
    const parity = computeParity(payloads, 1);
    // Chunk 1 is "in flight on the slow leg": the frame recovers without it.
    r.push(chunk(7, 0, 3, payloads[0]));
    r.push(chunk(7, 2, 3, payloads[2]));
    r.push(encodeParityChunk({ frameId: 7, parityIndex: 0, chunkCount: 3, frameBytes: 6 }, parity[0]));
    expect(r.getStats().framesRecoveredByParity).toBe(1);
    // Accounting for a RECOVERED frame is deferred: reporting arrived=2 now
    // would call a raced leg a lossy link.
    expect(accounting).toEqual([]);
    // The straggler lands and must be credited...
    r.push(chunk(7, 1, 3, payloads[1]));
    // ...so when the deferral window rolls past the frame, it reports fully
    // delivered — the truth on this link.
    for (let f = 100; f < 120; f++) {
      const q = new Uint8Array([9]);
      r.push(chunk(f, 0, 1, q));
    }
    expect(accounting).toContainEqual([3, 3]);
    expect(accounting).not.toContainEqual([3, 2]);
  });
});
