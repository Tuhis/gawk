// Adaptive delta-gap grace (docs/35 finding 4).
//
// DELTA_GAP_GRACE_MS shipped as a per-CONNECTION constant: 60 ms, "a couple of
// frame intervals", sized when a frame's datagrams arrived back-to-back on one
// QUIC connection so "outstanding past the grace" meant "lost". R30 striping
// spreads each frame across N legs with mutual skew, which turns that spread
// into per-frame completion jitter — a live Firefox 154 session measured
// p95−min arrival jitter of 101 ms median (268 ms p95) against the 60 ms
// grace, and paid ~1 gap resync per second for it while losing only 0.5 % of
// frames outright.
//
// The fix reads the jitter the buffer already measures. What makes the signal
// the right one is that it discriminates the two failure modes for free: a
// LATE frame arrives, so it inflates p95 and widens the grace; a LOST frame
// never arrives, so it contributes nothing and the grace stays at its floor
// and freezes fast. Both halves are asserted below.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  DELTA_GAP_GRACE_MS,
  GRACE_ENVELOPE,
  MAX_DELTA_GAP_GRACE_MS,
  ReorderBuffer,
  deltaGapGraceMs,
  resetGraceController,
  updateGraceController,
  type ReleasedFrame,
} from './reorder-buffer';
import { HEADROOM_MS, OFFSET_DOWN_DWELL_MS, OFFSET_WARMUP_MS } from './playout';
import {
  RESILIENT_DELTA_GAP_GRACE_MS,
  setLossAllowanceFrames,
  setViewerDeliveryModeFlag,
} from './resilient';

// The allowance would step OVER the first hole in a GOP and never reach the
// resync, which is the behaviour this file is about. Pin it off, as
// reorder-buffer.test.ts does, so every assertion below is about the grace.
beforeEach(() => {
  setLossAllowanceFrames(0);
  resetGraceController();
});
afterEach(() => {
  setLossAllowanceFrames(1);
  setViewerDeliveryModeFlag('live');
  resetGraceController();
});

// Drives the controller the way viewer.ts's stats tick does. Returns the clock
// it stopped at so callers can keep counting from there.
function feedJitter(jitterMs: number, fromMs: number, forMs: number, stepMs = 1000): number {
  let t = fromMs;
  for (const end = fromMs + forMs; t <= end; t += stepMs) updateGraceController(jitterMs, t);
  return t;
}

// Past the warmup with `jitterMs` established, so the assertions that follow
// are about the clamp/slew and not about the warmup hold.
function warmTo(jitterMs: number, fromMs = 10_000): number {
  return feedJitter(jitterMs, fromMs, OFFSET_WARMUP_MS + 10_000);
}

describe('adaptive delta-gap grace', () => {
  it('is the shipped 60 ms constant before any jitter has been measured', () => {
    expect(deltaGapGraceMs()).toBe(DELTA_GAP_GRACE_MS);
  });

  it('holds the floor through the estimator warmup', () => {
    // Real jitter, but not yet enough window to trust it.
    feedJitter(200, 10_000, OFFSET_WARMUP_MS - 1000);
    expect(deltaGapGraceMs()).toBe(DELTA_GAP_GRACE_MS);
  });

  it('widens to the measured jitter plus headroom once warm', () => {
    warmTo(120);
    expect(deltaGapGraceMs()).toBeCloseTo(120 + HEADROOM_MS, 5);
  });

  it('never exceeds the ceiling, whatever the spike', () => {
    // The 3748 ms outlier the Firefox session actually logged. An unclamped
    // grace here would climb into KEYFRAME_WAIT_MS territory and degenerate
    // into keyframe-only playback — docs/14's 2 fps failure, from the other
    // direction.
    warmTo(3748);
    expect(deltaGapGraceMs()).toBe(MAX_DELTA_GAP_GRACE_MS);
    expect(MAX_DELTA_GAP_GRACE_MS).toBeLessThan(RESILIENT_DELTA_GAP_GRACE_MS + 1);
  });

  it('never drops below the floor on a pristine link', () => {
    warmTo(0);
    expect(deltaGapGraceMs()).toBe(DELTA_GAP_GRACE_MS);
  });

  it('takes a large rise in one step and lowers only after the dwell', () => {
    const t = warmTo(200);
    const raised = deltaGapGraceMs();
    expect(raised).toBeCloseTo(200 + HEADROOM_MS, 5);

    // The link cleans up. Patience must not follow it straight down: dropping
    // the grace the moment jitter dips re-enters the freeze cycle on the next
    // burst.
    const afterShortCalm = feedJitter(10, t, OFFSET_DOWN_DWELL_MS / 2);
    expect(deltaGapGraceMs()).toBe(raised);

    // Past the dwell it descends, slowly.
    feedJitter(10, afterShortCalm, OFFSET_DOWN_DWELL_MS + 5000);
    const descended = deltaGapGraceMs();
    expect(descended).toBeLessThan(raised);
    expect(descended).toBeGreaterThan(DELTA_GAP_GRACE_MS);
  });

  it('leaves resilient mode on its deliberate constant', () => {
    warmTo(200);
    setViewerDeliveryModeFlag('resilient');
    expect(deltaGapGraceMs()).toBe(RESILIENT_DELTA_GAP_GRACE_MS);
  });

  it('seeds and floors at the same value, so an unwarmed viewer is byte-identical', () => {
    expect(GRACE_ENVELOPE.seedMs).toBe(DELTA_GAP_GRACE_MS);
    expect(GRACE_ENVELOPE.minMs).toBe(DELTA_GAP_GRACE_MS);
    expect(GRACE_ENVELOPE.maxMs).toBe(MAX_DELTA_GAP_GRACE_MS);
  });
});

// --- What the grace actually does to the buffer ------------------------------

function harness(startMs = 1000) {
  const released: ReleasedFrame[] = [];
  const clock = { t: startMs };
  const rb = new ReorderBuffer(
    (f) => released.push(f),
    () => clock.t,
  );
  return { rb, released, clock, ids: () => released.map((f) => f.frameId) };
}

const kf = (frameId: number) => ({
  frameId,
  timestampUs: BigInt(frameId),
  config: null,
  data: new Uint8Array([frameId & 0xff]),
});
const delta = (frameId: number) => ({
  frameId,
  timestampUs: BigInt(frameId),
  data: new Uint8Array([frameId & 0xff]),
});

describe('freeze-on-gap under the adaptive grace', () => {
  it('releases a straggler that arrives inside the measured jitter', () => {
    warmTo(120); // grace -> 154 ms
    const { rb, clock, ids } = harness();

    rb.pushKeyframe(kf(1));
    rb.pushDelta(delta(2));
    rb.pushDelta(delta(4)); // hole at 3

    // 100 ms later: past the shipped 60 ms grace, inside the measured jitter.
    clock.t += 100;
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(0);

    // The straggler lands and decode order is intact — no freeze, no GOP lost.
    clock.t += 20;
    rb.pushDelta(delta(3));
    expect(ids()).toEqual([1, 2, 3, 4]);
    expect(rb.getStats().gapResyncs).toBe(0);
  });

  it('still freezes fast when the frame is genuinely lost', () => {
    // A clean link: frames that go missing are LOST, not late, so they never
    // reach the jitter estimator and patience stays at the floor.
    warmTo(0);
    const { rb, clock, ids } = harness();

    rb.pushKeyframe(kf(1));
    rb.pushDelta(delta(2));
    rb.pushDelta(delta(4));

    clock.t += DELTA_GAP_GRACE_MS + 1;
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(1);
    expect(ids()).toEqual([1, 2]);
  });

  it('still declares the gap once the widened grace is spent', () => {
    warmTo(120); // grace -> 154 ms
    const { rb, clock } = harness();

    rb.pushKeyframe(kf(1));
    rb.pushDelta(delta(2));
    rb.pushDelta(delta(4));

    clock.t += 120 + HEADROOM_MS + 1;
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(1);
  });
});
