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
});
