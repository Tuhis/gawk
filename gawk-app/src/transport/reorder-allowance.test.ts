// R29 FP6: the per-GOP frame-loss allowance (docs/34 §6).
//
// Parity reduces how OFTEN a frame is unrecoverable; the allowance bounds what
// one unrecoverable frame COSTS. Without it a single hole still forfeits the
// rest of the GOP, which at a 500 ms GOP is up to ~20 frames thrown away to
// avoid one frame of artifacts.

import { describe, expect, it, beforeEach, afterEach } from 'vitest';

import { ReorderBuffer } from './reorder-buffer';
import { setLossAllowanceFrames, getLossAllowanceFrames } from './resilient';

interface Released {
  frameId: number;
  keyframe: boolean;
}

function harness() {
  const out: Released[] = [];
  let now = 0;
  const buf = new ReorderBuffer(
    (f) => out.push({ frameId: f.frameId, keyframe: f.keyframe }),
    () => now,
  );
  return {
    buf,
    out,
    tick: (ms: number) => {
      now += ms;
      buf.tick();
    },
    push: (frameId: number, keyframe: boolean) => {
      const base = { frameId, timestampUs: BigInt(frameId) * 33_000n, data: new Uint8Array([frameId]) };
      if (keyframe) buf.pushKeyframe({ ...base, config: null });
      else buf.pushDelta(base);
    },
  };
}

afterEach(() => setLossAllowanceFrames(1));

describe('per-GOP loss allowance', () => {
  beforeEach(() => setLossAllowanceFrames(1));

  it('defaults to 1', () => {
    setLossAllowanceFrames(1);
    expect(getLossAllowanceFrames()).toBe(1);
  });

  it('skips one missing frame and keeps decoding within the GOP', () => {
    const h = harness();
    h.push(1, true);
    h.push(2, false);
    // frame 3 is lost
    h.push(4, false);
    h.push(5, false);
    h.tick(2000); // past the gap grace

    const ids = h.out.map((f) => f.frameId);
    expect(ids).toContain(4);
    expect(ids).toContain(5);
    expect(ids).not.toContain(3);
    expect(h.buf.getStats().framesSkippedWithinAllowance).toBe(1);
  });

  it('freezes on the SECOND loss in one GOP', () => {
    const h = harness();
    h.push(1, true);
    // 2 lost
    h.push(3, false);
    h.tick(2000);
    // 4 lost — over budget
    h.push(5, false);
    h.push(6, false);
    h.tick(2000);

    const ids = h.out.map((f) => f.frameId);
    expect(ids).toContain(3); // first loss was absorbed
    expect(ids).not.toContain(5); // second loss froze to the next keyframe
    expect(ids).not.toContain(6);
    expect(h.buf.getStats().gapResyncs).toBeGreaterThan(0);
  });

  it('resets the budget at every keyframe', () => {
    const h = harness();
    h.push(1, true);
    // 2 lost — spends the GOP's budget
    h.push(3, false);
    h.tick(2000);
    // A new GOP: budget back to 1, so one more loss is absorbable.
    h.push(10, true);
    // 11 lost
    h.push(12, false);
    h.tick(2000);

    const ids = h.out.map((f) => f.frameId);
    expect(ids).toContain(10);
    expect(ids).toContain(12);
    expect(h.buf.getStats().framesSkippedWithinAllowance).toBe(2);
  });

  it('a skipped frame is NOT counted as a gap resync', () => {
    // gapResyncs is what docs/13's playbook reads as "delta loss is eating
    // GOPs". A skip is the opposite outcome — the GOP survived — so counting
    // it there would make the existing signal mean two things.
    const h = harness();
    h.push(1, true);
    h.push(3, false);
    h.tick(2000);
    expect(h.buf.getStats().framesSkippedWithinAllowance).toBe(1);
    expect(h.buf.getStats().gapResyncs).toBe(0);
  });

  it('allowance 0 reproduces pre-R29 freeze-on-gap exactly', () => {
    setLossAllowanceFrames(0);
    const h = harness();
    h.push(1, true);
    // 2 lost
    h.push(3, false);
    h.push(4, false);
    h.tick(2000);

    const ids = h.out.map((f) => f.frameId);
    expect(ids).toEqual([1]);
    expect(h.buf.getStats().framesSkippedWithinAllowance).toBe(0);
    expect(h.buf.getStats().gapResyncs).toBeGreaterThan(0);
  });

  it('does not skip when a keyframe is already buffered ahead', () => {
    // A buffered keyframe is a definitive resync point; jumping to it is
    // strictly better than decoding damaged deltas up to it.
    setLossAllowanceFrames(1);
    const h = harness();
    h.push(1, true);
    // 2 lost
    h.push(3, false);
    h.push(4, true); // keyframe ahead
    h.tick(2000);

    const ids = h.out.map((f) => f.frameId);
    expect(ids).toContain(4);
    expect(ids).not.toContain(3);
    expect(h.buf.getStats().framesSkippedWithinAllowance).toBe(0);
  });

  // The budget is for lost DELTA frames. A keyframe rides a reliable stream
  // and routinely lands after the deltas that follow it; skipping it would
  // decode the GOP against a missing reference and then read the keyframe
  // as a broadcaster restart.
  it('does not skip a frame with no delta evidence (an in-flight keyframe)', () => {
    const out: number[] = [];
    let now = 0;
    const restarts: number[] = [];
    const deltaIds = new Set<number>();
    const buf = new ReorderBuffer(
      (f) => out.push(f.frameId),
      () => now,
      { onRestart: () => restarts.push(now), isDeltaFrame: (id) => deltaIds.has(id) },
    );
    const push = (frameId: number, keyframe: boolean) => {
      const base = { frameId, timestampUs: BigInt(frameId) * 16_000n, data: new Uint8Array([frameId]) };
      if (keyframe) buf.pushKeyframe({ ...base, config: null });
      else {
        deltaIds.add(frameId);
        buf.pushDelta(base);
      }
    };
    push(0, true);
    for (let i = 1; i < 30; i++) push(i, false);
    // Keyframe 30 is still on its stream while 31..39 arrive.
    for (let i = 31; i < 40; i++) {
      now += 16;
      push(i, false);
    }
    now += 300;
    buf.tick();
    expect(out).not.toContain(31);
    push(30, true);

    expect(out.slice(-10)).toEqual([30, 31, 32, 33, 34, 35, 36, 37, 38, 39]);
    expect(restarts).toEqual([]);
    expect(buf.getStats().framesSkippedWithinAllowance).toBe(0);
  });

  it('still skips a frame the reassembler saw as a delta', () => {
    const out: number[] = [];
    let now = 0;
    const buf = new ReorderBuffer((f) => out.push(f.frameId), () => now, { isDeltaFrame: (id) => id === 2 });
    buf.pushKeyframe({ frameId: 1, timestampUs: 0n, data: new Uint8Array([1]), config: null });
    buf.pushDelta({ frameId: 3, timestampUs: 3000n, data: new Uint8Array([3]) });
    now += 2000;
    buf.tick();
    expect(out).toEqual([1, 3]);
    expect(buf.getStats().framesSkippedWithinAllowance).toBe(1);
  });
});
