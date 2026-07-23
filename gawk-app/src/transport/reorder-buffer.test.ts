// S5 (docs/12 Decision 7): the reorder buffer merges reliable stream keyframes
// with lossy datagram deltas by frameId, releasing in decode order with two
// bounded waits and NO fixed playout offset. A controlled clock drives the
// time-based decisions deterministically.

import { afterEach, describe, expect, it } from 'vitest';

import {
  ReorderBuffer,
  DELTA_GAP_GRACE_MS,
  KEYFRAME_WAIT_MS,
  MAX_BUFFERED_FRAMES,
  type ReleasedFrame,
} from './reorder-buffer';
import { LIVE_EDGE_WINDOW_MS, QUANTILE_BIN_MS, QUANTILE_RANGE_MS } from './live-edge';
import { PlayoutController, RESILIENT_PLAYOUT_PROFILE } from './playout';
import {
  RESILIENT_DELTA_GAP_GRACE_MS,
  RESILIENT_KEYFRAME_WAIT_MS,
  RESILIENT_MAX_BUFFERED_FRAMES,
  setViewerDeliveryModeFlag,
} from './resilient';

function harness() {
  const released: ReleasedFrame[] = [];
  const clock = { t: 1000 };
  const rb = new ReorderBuffer((f) => released.push(f), () => clock.t);
  return { rb, released, clock, ids: () => released.map((f) => f.frameId) };
}

const kf = (frameId: number, config: ReleasedFrame['config'] = null) => ({
  frameId,
  timestampUs: BigInt(frameId),
  config,
  data: new Uint8Array([frameId & 0xff]),
});
const delta = (frameId: number) => ({
  frameId,
  timestampUs: BigInt(frameId),
  data: new Uint8Array([frameId & 0xff]),
});

describe('ReorderBuffer', () => {
  it('holds deltas that arrive before their keyframe, then releases key-then-contiguous', () => {
    const { rb, released, ids } = harness();
    rb.pushDelta(delta(1));
    rb.pushDelta(delta(2));
    expect(released).toEqual([]); // nothing decodable without the keyframe

    rb.pushKeyframe(kf(0));
    expect(ids()).toEqual([0, 1, 2]);
    expect(released[0].keyframe).toBe(true);
    expect(released[1].keyframe).toBe(false);
  });

  it('reorders an out-of-order delta within a session', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(2)); // arrives before 1
    expect(ids()).toEqual([0]); // 2 held, 1 missing
    rb.pushDelta(delta(1));
    expect(ids()).toEqual([0, 1, 2]);
  });

  it('releases a keyframe that is the contiguous next frame, keeping buffered deltas', () => {
    const { rb, released, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(1));
    rb.pushDelta(delta(3)); // ahead; 2 missing
    expect(ids()).toEqual([0, 1]);
    rb.pushKeyframe(kf(2)); // the in-sync next frame IS a keyframe
    expect(ids()).toEqual([0, 1, 2, 3]);
    expect(released.find((f) => f.frameId === 2)!.keyframe).toBe(true);
  });

  it('carries an embedded config on keyframe releases only', () => {
    const { rb, released } = harness();
    const cfg = { codec: 'avc1.42E01F', extradata: new Uint8Array([1, 2, 3]) };
    rb.pushKeyframe(kf(0, cfg));
    rb.pushDelta(delta(1));
    expect(released[0].config).toBe(cfg);
    expect(released[1].config).toBeNull();
  });

  it('does not release before the first keyframe (initial sync)', () => {
    const { rb, released } = harness();
    rb.pushDelta(delta(7));
    rb.pushDelta(delta(8));
    expect(released).toEqual([]);
  });

  it('freezes on a delta gap after the grace and resyncs at the next keyframe', () => {
    const { rb, released, ids, clock } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(2)); // next=1 missing; buffered at t=1000
    expect(ids()).toEqual([0]);

    clock.t += DELTA_GAP_GRACE_MS - 5; // within grace: still waiting for 1
    rb.tick();
    expect(ids()).toEqual([0]);

    clock.t += 10; // past grace: declare gap, freeze
    rb.tick();
    expect(ids()).toEqual([0]);
    expect(rb.getStats().gapResyncs).toBeGreaterThanOrEqual(1);

    rb.pushKeyframe(kf(3)); // reliable keyframe arrives; drop the orphan delta 2
    expect(ids()).toEqual([0, 3]);
    expect(released.find((f) => f.frameId === 3)!.keyframe).toBe(true);
    rb.pushDelta(delta(4));
    expect(ids()).toEqual([0, 3, 4]);
  });

  it('survives a keyframe that arrives ~500ms behind its trailing deltas (R10 field finding)', () => {
    // The 2026-07-14 Chrome diagnostics trace (docs/14): keyframes ride the
    // store-and-forward stream path and can land hundreds of ms after their
    // trailing deltas arrived via datagrams — a ~236 KB keyframe to a congested
    // peer regularly took > 500 ms. The deltas held while waiting must survive
    // that long, or every GOP degenerates into keyframe-only 2 fps playback:
    // deltas expire → keyframe releases alone → instant gap → freeze → repeat.
    const { rb, clock, ids } = harness();

    // Waiting for a keyframe (initial sync — same state as after a gap).
    // Deltas 101..105 arrive promptly and buffer; the pipeline keeps ticking.
    for (let id = 101; id <= 105; id++) rb.pushDelta(delta(id));
    for (let i = 0; i < 5; i++) {
      clock.t += 100; // 500 ms of ticks while the keyframe stream transfers
      rb.tick();
    }
    expect(ids()).toEqual([]);

    rb.pushKeyframe(kf(100)); // the late keyframe finally lands
    expect(ids()).toEqual([100, 101, 102, 103, 104, 105]);
    expect(rb.getStats().keyframeWaitDrops).toBe(0);
  });

  it('drops undecodable held frames once they age past KEYFRAME_WAIT_MS', () => {
    const { rb, clock } = harness();
    rb.pushDelta(delta(5)); // waiting for a keyframe that never comes
    expect(rb.getStats().buffered).toBe(1);
    clock.t += KEYFRAME_WAIT_MS + 1;
    rb.tick();
    expect(rb.getStats().buffered).toBe(0);
    expect(rb.getStats().keyframeWaitDrops).toBe(1);
  });

  it('immediately resyncs to a keyframe buffered ahead of a delta gap', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(1));
    expect(ids()).toEqual([0, 1]);

    // A reliable keyframe ahead of the gap (2..4 missing) is a definitive
    // resync — jump to it now rather than wait for lost deltas.
    rb.pushKeyframe(kf(5));
    expect(ids()).toEqual([0, 1, 5]);
    expect(rb.getStats().keyframesReleased).toBe(2);
  });

  it('requestResync holds contiguous deltas until the next keyframe (decoder backpressure)', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(1));
    expect(ids()).toEqual([0, 1]);

    rb.requestResync(); // decoder is deep: stop feeding it until the next keyframe
    rb.pushDelta(delta(2)); // contiguous, but held because we're resyncing
    rb.pushDelta(delta(3));
    expect(ids()).toEqual([0, 1]);

    rb.pushKeyframe(kf(5)); // next keyframe → jump, drop the held deltas 2,3
    expect(ids()).toEqual([0, 1, 5]);
  });

  it('releases contiguously across frameId rollover (uint32 wrap)', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0xffff_fffe));
    rb.pushDelta(delta(0xffff_ffff));
    rb.pushDelta(delta(0)); // the wire frameId wraps: 0 is the contiguous successor
    rb.pushDelta(delta(1));
    expect(ids()).toEqual([0xffff_fffe, 0xffff_ffff, 0, 1]);
  });

  it('treats a post-rollover keyframe as ahead of a pre-rollover gap', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0xffff_fffe));
    rb.pushDelta(delta(0xffff_ffff));
    expect(ids()).toEqual([0xffff_fffe, 0xffff_ffff]);

    // Frames 0..29 lost across the wrap; keyframe 30 is a definitive resync
    // point AHEAD of the gap and must release immediately (no grace wait).
    rb.pushKeyframe(kf(30));
    expect(ids()).toEqual([0xffff_fffe, 0xffff_ffff, 30]);
  });

  it('drops a stale delta at or below the decode position', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(5));
    rb.pushDelta(delta(3)); // 3 <= 5 → stale
    rb.pushDelta(delta(5)); // == position → stale
    expect(ids()).toEqual([5]);
    expect(rb.getStats().deltasDropped).toBe(2);
  });

  it('recovers across a broadcaster restart (frameId reset)', () => {
    const { rb, ids, clock } = harness();
    rb.pushKeyframe(kf(100));
    rb.pushDelta(delta(101));
    expect(ids()).toEqual([100, 101]);

    // Restart: new session numbering from 0. The keyframe looks "older" but is
    // the freshest arrival, so after the gap grace we resync onto it.
    rb.pushKeyframe(kf(0));
    clock.t += DELTA_GAP_GRACE_MS + 1;
    rb.tick();
    expect(ids()).toEqual([100, 101, 0]);

    rb.pushDelta(delta(1));
    expect(ids()).toEqual([100, 101, 0, 1]);
  });

  it('ignores an exact-duplicate keyframe of the current position', () => {
    const { rb, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushKeyframe(kf(0)); // duplicate priming keyframe
    rb.pushDelta(delta(1));
    expect(ids()).toEqual([0, 1]);
  });

  it('bounds the buffer at MAX_BUFFERED_FRAMES', () => {
    const { rb } = harness();
    // Never send the keyframe, so everything stays buffered until the cap.
    for (let i = 1; i <= MAX_BUFFERED_FRAMES + 10; i++) rb.pushDelta(delta(i));
    expect(rb.getStats().buffered).toBeLessThanOrEqual(MAX_BUFFERED_FRAMES);
    expect(rb.getStats().deltasDropped).toBeGreaterThanOrEqual(10);
  });

  it('signals onRestart when a keyframe arrives serially behind the decode position (R5 Q1)', () => {
    // The restart signal drives live-edge baseline resets: a new broadcaster
    // session stamps timestamps on a fresh timeline, so any windowed-min
    // baseline built against the old one is meaningless.
    const restarts: number[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer(
      () => {},
      () => clock.t,
      { onRestart: () => restarts.push(clock.t) },
    );
    rb.pushKeyframe(kf(100));
    rb.pushDelta(delta(101));
    expect(restarts).toEqual([]); // normal flow: no signal

    rb.pushKeyframe(kf(0)); // serially behind 101 → the restart signal
    expect(restarts).toHaveLength(1);

    rb.pushDelta(delta(1));
    rb.pushKeyframe(kf(2)); // ahead again: normal resync material, no signal
    expect(restarts).toHaveLength(1);
  });

  it('does not signal onRestart for an exact-duplicate keyframe', () => {
    const restarts: number[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer(
      () => {},
      () => clock.t,
      { onRestart: () => restarts.push(clock.t) },
    );
    rb.pushKeyframe(kf(5));
    rb.pushKeyframe(kf(5)); // duplicate prime: dropped, not a restart
    expect(restarts).toEqual([]);
  });

  it('a keyframe supersedes a same-id delta already buffered', () => {
    const { rb, released, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(3)); // buffered behind a gap (1,2 missing)
    rb.pushKeyframe(kf(3)); // same id arrives as a keyframe: it wins and resyncs
    expect(ids()).toEqual([0, 3]);
    expect(released.find((f) => f.frameId === 3)!.keyframe).toBe(true);
  });
});

// R5 Q3 (docs/15): opt-in smoothed playout — a decodable frame is released
// only once `now >= timestampMs + arrivalBaseline + offset`. Off by default;
// the offset function is injected so these tests control it live. Pacing adds
// delay, never patience: every drop/resync policy fires unchanged.
describe('ReorderBuffer smoothed playout (R5 Q3)', () => {
  // Timestamps in these tests are millisecond-scale (µs on the wire).
  const tsKf = (frameId: number, tsMs: number) => ({
    frameId,
    timestampUs: BigInt(Math.round(tsMs * 1000)),
    config: null,
    data: new Uint8Array([frameId & 0xff]),
  });
  const tsDelta = (frameId: number, tsMs: number) => ({
    frameId,
    timestampUs: BigInt(Math.round(tsMs * 1000)),
    data: new Uint8Array([frameId & 0xff]),
  });

  function pacedHarness(offset: { value: number }) {
    const released: ReleasedFrame[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer(
      (f) => released.push(f),
      () => clock.t,
      { playoutOffsetMs: () => offset.value },
    );
    return { rb, released, clock, ids: () => released.map((f) => f.frameId) };
  }

  it('releases at the schedule, not on arrival', () => {
    const offset = { value: 150 };
    const { rb, clock, ids } = pacedHarness(offset);
    // Keyframe captured at ts=0ms arrives at t=1000 → baseline delta 1000,
    // due at 0 + 1000 + 150 = 1150.
    rb.pushKeyframe(tsKf(0, 0));
    expect(ids()).toEqual([]); // held: not due yet

    clock.t = 1149;
    rb.tick();
    expect(ids()).toEqual([]);

    clock.t = 1150;
    rb.tick(); // due frame releases from a bare tick — no new arrivals needed
    expect(ids()).toEqual([0]);

    // The following delta (captured 16ms later) is due at 1166.
    rb.pushDelta(tsDelta(1, 16));
    expect(ids()).toEqual([0]);
    clock.t = 1166;
    rb.tick();
    expect(ids()).toEqual([0, 1]);
  });

  it('offset 0 releases immediately (byte-for-byte the unpaced path)', () => {
    const offset = { value: 0 };
    const { rb, ids } = pacedHarness(offset);
    rb.pushKeyframe(tsKf(0, 0));
    rb.pushDelta(tsDelta(1, 16));
    expect(ids()).toEqual([0, 1]);
  });

  it('toggling mid-session re-paces both ways', () => {
    const offset = { value: 0 };
    const { rb, ids } = pacedHarness(offset);
    rb.pushKeyframe(tsKf(0, 0)); // live-edge: immediate
    expect(ids()).toEqual([0]);

    offset.value = 150; // toggle ON: subsequent frames pace
    rb.pushDelta(tsDelta(1, 16)); // baseline ~1000-16... due = 16 + baseline + 150
    expect(ids()).toEqual([0]);

    offset.value = 0; // toggle OFF: held frame releases on the next tick
    rb.tick();
    expect(ids()).toEqual([0, 1]);
  });

  it('keeps the gap policy under pacing: a missing delta still freezes and resyncs', () => {
    const offset = { value: 150 };
    const { rb, clock, ids } = pacedHarness(offset);
    rb.pushKeyframe(tsKf(0, 0));
    clock.t = 1150;
    rb.tick();
    expect(ids()).toEqual([0]);

    // Delta 1 never arrives; 2 does. Past the grace the gap is declared and
    // we freeze awaiting a keyframe — exactly as with pacing off.
    rb.pushDelta(tsDelta(2, 33));
    clock.t += DELTA_GAP_GRACE_MS + 1;
    rb.tick();
    expect(ids()).toEqual([0]);
    expect(rb.getStats().gapResyncs).toBe(1);

    // The next keyframe resyncs, released at ITS schedule.
    rb.pushKeyframe(tsKf(5, 100));
    // Baseline is still the windowed min (~1000ms from frame 0); due ≈ 1250.
    expect(ids()).toEqual([0]);
    clock.t = 1250;
    rb.tick();
    expect(ids()).toEqual([0, 5]);
  });

  it('requestResync (decoder-queue-deep) still drops to the keyframe under pacing', () => {
    const offset = { value: 150 };
    const { rb, clock, ids } = pacedHarness(offset);
    rb.pushKeyframe(tsKf(0, 0));
    clock.t = 1150;
    rb.tick();
    expect(ids()).toEqual([0]);

    rb.pushDelta(tsDelta(1, 16));
    rb.pushKeyframe(tsKf(2, 33));
    rb.requestResync(); // deep decoder queue: jump to the freshest keyframe
    clock.t = 1350; // past every schedule
    rb.tick();
    // The resync jumped to keyframe 2; delta 1 was dropped, not decoded.
    expect(ids()).toEqual([0, 2]);
  });
});

// R12 T1 (docs/17): arrival jitter — windowed p95 of the arrival delta minus
// the windowed min, observed on every insert. The measurement shares the
// estimator the adaptive playout controller (T3) reads.
describe('ReorderBuffer arrival jitter (R12 T1)', () => {
  const tsKf = (frameId: number, tsMs: number) => ({
    frameId,
    timestampUs: BigInt(Math.round(tsMs * 1000)),
    config: null,
    data: new Uint8Array([frameId & 0xff]),
  });
  const tsDelta = (frameId: number, tsMs: number) => ({
    frameId,
    timestampUs: BigInt(Math.round(tsMs * 1000)),
    data: new Uint8Array([frameId & 0xff]),
  });

  function jitterHarness() {
    const released: ReleasedFrame[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer((f) => released.push(f), () => clock.t);
    return { rb, clock };
  }

  it('is null before any frame', () => {
    const { rb } = jitterHarness();
    expect(rb.arrivalJitterMs()).toBeNull();
  });

  it('reads ~0 for a perfectly steady arrival delta', () => {
    const { rb, clock } = jitterHarness();
    rb.pushKeyframe(tsKf(0, 0)); // delta 1000
    for (let i = 1; i <= 10; i++) {
      clock.t = 1000 + i * 1000;
      rb.pushDelta(tsDelta(i, i * 1000)); // delta 1000, every time
    }
    expect(rb.arrivalJitterMs()).toBeLessThan(QUANTILE_BIN_MS);
  });

  it('reads the late tail as p95 − min', () => {
    const { rb, clock } = jitterHarness();
    rb.pushKeyframe(tsKf(0, 0));
    for (let i = 1; i <= 9; i++) {
      clock.t = 1000 + i * 1000;
      rb.pushDelta(tsDelta(i, i * 1000)); // 10 samples at delta 1000
    }
    for (let i = 10; i <= 11; i++) {
      clock.t = 1100 + i * 1000;
      rb.pushDelta(tsDelta(i, i * 1000)); // 2 samples arriving 100 ms late
    }
    const j = rb.arrivalJitterMs();
    expect(j).not.toBeNull();
    expect(j!).toBeGreaterThanOrEqual(100 - QUANTILE_BIN_MS);
    expect(j!).toBeLessThanOrEqual(100 + QUANTILE_BIN_MS);
  });

  it('resets with the broadcaster-restart signal', () => {
    const { rb, clock } = jitterHarness();
    rb.pushKeyframe(tsKf(0, 0));
    clock.t = 2100;
    rb.pushDelta(tsDelta(1, 1000)); // delta 1100: some jitter on record
    // Restart: a keyframe serially behind the decode position, on a fresh
    // timestamp timeline with a completely different arrival delta.
    clock.t = 20_000;
    rb.pushKeyframe(tsKf(0, 5000));
    const j = rb.arrivalJitterMs();
    expect(j).not.toBeNull();
    expect(j!).toBeLessThan(QUANTILE_BIN_MS); // single fresh sample, old ones gone
  });
});

// R12 T2 (docs/17 Decision 4): in adaptive (paced-presentation) mode the
// release gate retargets to target − DECODE_LEAD_MS — frames reach the sink
// just in time for their display slot while the pre-decode pace still bounds
// the decoder frame pool. Fixed mode is untouched (lead 0).
describe('ReorderBuffer decode lead (R12 T2)', () => {
  const tsKf = (frameId: number, tsMs: number) => ({
    frameId,
    timestampUs: BigInt(Math.round(tsMs * 1000)),
    config: null,
    data: new Uint8Array([frameId & 0xff]),
  });

  it('releases at target − lead in adaptive mode', () => {
    const released: ReleasedFrame[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer((f) => released.push(f), () => clock.t, {
      playoutOffsetMs: () => 150,
      decodeLeadMs: () => 35,
    });
    // Keyframe ts=0 arriving at t=1000: baseline 1000, target 1150, release
    // due at 1150 − 35 = 1115.
    rb.pushKeyframe(tsKf(0, 0));
    clock.t = 1114;
    rb.tick();
    expect(released).toHaveLength(0);
    clock.t = 1115;
    rb.tick();
    expect(released).toHaveLength(1);
  });

  // Field findings 2 + 4 (docs/20): R15 briefly had the sink's display target
  // on the audio playhead while this gate stayed on the arrival baseline, and
  // the gap between the two schedules froze video wholesale. The revision made
  // video the master outright — this gate answers to the arrival baseline and
  // nothing else, and audio aligns to it. Pinned here because "audio is
  // somewhere in the release decision" is the exact shape of that bug.
  it('paces on the arrival baseline alone, whatever audio is doing', () => {
    const released: ReleasedFrame[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer((f) => released.push(f), () => clock.t, {
      playoutOffsetMs: () => 150,
      decodeLeadMs: () => 35,
    });
    rb.pushKeyframe(tsKf(0, 0)); // baseline 1000 ⇒ target 1150, release 1115

    clock.t = 1114;
    rb.tick();
    expect(released).toHaveLength(0);
    clock.t = 1115;
    rb.tick();
    expect(released).toHaveLength(1);
  });

  it('exposes the arrival baseline for the pipeline to compute display targets', () => {
    const released: ReleasedFrame[] = [];
    const clock = { t: 1000 };
    const rb = new ReorderBuffer((f) => released.push(f), () => clock.t);
    expect(rb.arrivalBaselineMs()).toBeNull();
    rb.pushKeyframe(tsKf(0, 0)); // arrival delta 1000
    expect(rb.arrivalBaselineMs()).toBe(1000);
  });
});

// R19 (docs/24 Decision 7): the three bounds widen while resilient mode is
// on and revert the moment it's off — the default-mode suites above run with
// it off and are untouched.
describe('ReorderBuffer resilient profile (R19)', () => {
  afterEach(() => setViewerDeliveryModeFlag('live'));

  it('waits out RTT-scale delta stragglers before declaring a gap', () => {
    setViewerDeliveryModeFlag('resilient');
    const { rb, clock } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(2)); // next=1 missing

    clock.t += DELTA_GAP_GRACE_MS + 5; // past the DEFAULT grace: still patient
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(0);

    clock.t += RESILIENT_DELTA_GAP_GRACE_MS; // past the resilient grace
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(1);

    // The cross-carrier straggler arriving inside the widened grace decodes.
    const second = harness();
    setViewerDeliveryModeFlag('resilient');
    second.rb.pushKeyframe(kf(0));
    second.rb.pushDelta(delta(2));
    second.clock.t += RESILIENT_DELTA_GAP_GRACE_MS - 10;
    second.rb.pushDelta(delta(1));
    expect(second.ids()).toEqual([0, 1, 2]);
  });

  it('holds undecodable frames up to the widened keyframe wait', () => {
    setViewerDeliveryModeFlag('resilient');
    const { rb, clock } = harness();
    rb.pushDelta(delta(5)); // no keyframe yet

    clock.t += KEYFRAME_WAIT_MS + 100; // past the DEFAULT wait: still held
    rb.tick();
    expect(rb.getStats().keyframeWaitDrops).toBe(0);

    clock.t += RESILIENT_KEYFRAME_WAIT_MS; // past the resilient wait
    rb.tick();
    expect(rb.getStats().keyframeWaitDrops).toBe(1);
  });

  it('bounds the buffer at the widened frame cap', () => {
    setViewerDeliveryModeFlag('resilient');
    const { rb } = harness();
    rb.pushKeyframe(kf(0));
    // Skip frame 1 so everything above buffers.
    for (let i = 2; i < 2 + RESILIENT_MAX_BUFFERED_FRAMES + 10; i++) rb.pushDelta(delta(i));
    const buffered = rb.getStats().buffered;
    expect(buffered).toBeGreaterThan(MAX_BUFFERED_FRAMES);
    expect(buffered).toBeLessThanOrEqual(RESILIENT_MAX_BUFFERED_FRAMES);
  });

  it('reverts to the default bounds the moment resilient mode is off', () => {
    setViewerDeliveryModeFlag('resilient');
    setViewerDeliveryModeFlag('live');
    const { rb, clock } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(2));
    clock.t += DELTA_GAP_GRACE_MS + 5;
    rb.tick();
    expect(rb.getStats().gapResyncs).toBe(1);
  });
});

// R19 hardening — PLAYOUT-1 (docs/reviews/resilient-mode-review.md): the
// resilient offset envelope goes to 2000 ms, but the signal driving it —
// arrival jitter — was measured by a histogram whose range is 500 ms, so
// `p95 − min` saturated at ~500 and the controller could never climb past
// ~534 ms. The histogram geometry now comes from the active playout profile.
//
// The scenario below is the one the mode exists for: a retransmit stall holds
// a burst of deltas, then delivers them at once. Every held frame lands in the
// SAME arrival bucket as the fresh ones that follow it, which is exactly where
// the per-bucket clamp bit.
describe('ReorderBuffer arrival jitter under a deep stall (R19 PLAYOUT-1)', () => {
  afterEach(() => setViewerDeliveryModeFlag('live'));

  // Frames stamped on the broadcaster clock; the viewer clock runs
  // BASE_DELTA_MS ahead of it (unknown offset + best-case latency), so a
  // frame delivered on time reads an arrival delta of exactly BASE_DELTA_MS.
  const BASE_DELTA_MS = 1000;
  const FRAME_INTERVAL_MS = 1000 / 30;

  // Offset injected as 0: this suite is about the estimator, not pacing.
  function jitterHarness() {
    const clock = { t: 0 };
    const rb = new ReorderBuffer(
      () => {},
      () => clock.t,
      { playoutOffsetMs: () => 0 },
    );
    let frameId = 0;
    let captureMs = 0;
    return {
      rb,
      clock,
      // The opening keyframe, delivered on time like everything else — an
      // arrival delta of 0 here would anchor the windowed min at 0 and make
      // every jitter reading below meaningless.
      keyframe() {
        captureMs += FRAME_INTERVAL_MS;
        clock.t = captureMs + BASE_DELTA_MS;
        rb.pushKeyframe({
          frameId,
          timestampUs: BigInt(Math.round(captureMs * 1000)),
          config: null,
          data: new Uint8Array([0]),
        });
      },
      // Delivers `count` frames on time, advancing the clock with them.
      steady(count: number) {
        for (let i = 0; i < count; i++) {
          captureMs += FRAME_INTERVAL_MS;
          clock.t = captureMs + BASE_DELTA_MS;
          rb.pushDelta({
            frameId: ++frameId,
            timestampUs: BigInt(Math.round(captureMs * 1000)),
            data: new Uint8Array([frameId & 0xff]),
          });
        }
      },
      // A stall of `stallMs`: frames keep being captured but nothing is
      // delivered, then the whole backlog lands in one burst.
      stallThenBurst(stallMs: number) {
        const held: number[] = [];
        for (let held_ms = 0; held_ms < stallMs; held_ms += FRAME_INTERVAL_MS) {
          captureMs += FRAME_INTERVAL_MS;
          held.push(captureMs);
        }
        clock.t = captureMs + BASE_DELTA_MS; // the burst arrives when the link recovers
        for (const ts of held) {
          rb.pushDelta({
            frameId: ++frameId,
            timestampUs: BigInt(Math.round(ts * 1000)),
            data: new Uint8Array([frameId & 0xff]),
          });
        }
      },
    };
  }

  it('measures a 1400 ms stall as >1 s of jitter in resilient mode', () => {
    setViewerDeliveryModeFlag('resilient');
    const h = jitterHarness();
    h.keyframe();
    h.steady(20);
    h.stallThenBurst(1400);

    const jitter = h.rb.arrivalJitterMs();
    expect(jitter).not.toBeNull();
    // The oldest held frame is ~1400 ms late; p95 across the window must land
    // deep in the burst, not against a 500 ms histogram wall.
    expect(jitter!).toBeGreaterThan(1000);
  });

  it('lets the resilient controller reach past 1 s from the measured signal', () => {
    setViewerDeliveryModeFlag('resilient');
    const h = jitterHarness();
    h.keyframe();
    const controller = new PlayoutController(RESILIENT_PLAYOUT_PROFILE);
    // A link that stalls repeatedly, sampled the way the pipeline does it.
    for (let round = 0; round < 12; round++) {
      h.steady(15);
      h.stallThenBurst(1400);
      controller.update(h.rb.arrivalJitterMs(), h.clock.t);
    }
    expect(controller.offsetMs()).toBeGreaterThan(1000);
  });

  it('keeps the default profile on its 500 ms histogram', () => {
    const h = jitterHarness(); // resilient off
    h.keyframe();
    h.steady(20);
    h.stallThenBurst(1400);

    const jitter = h.rb.arrivalJitterMs();
    expect(jitter).not.toBeNull();
    // Live-edge mode never buffers past MAX_PLAYOUT_OFFSET_MS, so the narrow
    // histogram stays exactly as it shipped — the saturation is by design here.
    expect(jitter!).toBeLessThanOrEqual(QUANTILE_RANGE_MS + QUANTILE_BIN_MS);
  });

  it('follows a profile change made after construction', () => {
    // A mode flip is a deliberate reconnect in production, so this only
    // guards the estimator against ever being left on the wrong geometry.
    const h = jitterHarness(); // constructed with resilient off
    h.keyframe();
    h.steady(20);
    h.stallThenBurst(1400);
    expect(h.rb.arrivalJitterMs()!).toBeLessThanOrEqual(QUANTILE_RANGE_MS + QUANTILE_BIN_MS);

    setViewerDeliveryModeFlag('resilient');
    h.steady(20);
    h.stallThenBurst(1400);
    expect(h.rb.arrivalJitterMs()!).toBeGreaterThan(1000);
  });

  // PLAYOUT-3, the second half of the same fix: a 60 s window makes the
  // resilient offset react on a minute timescale. The resilient profile
  // measures over a much shorter window, so a cleaned-up link is reflected
  // in seconds — the down direction stays governed by the controller's dwell
  // and slew, which is where that memory belongs.
  it('ages a stall out of the resilient jitter window within seconds', () => {
    setViewerDeliveryModeFlag('resilient');
    const h = jitterHarness();
    h.keyframe();
    h.steady(20);
    h.stallThenBurst(1400);
    expect(h.rb.arrivalJitterMs()!).toBeGreaterThan(1000);

    // The link cleans up: steady delivery for the whole resilient window.
    const windowMs = RESILIENT_PLAYOUT_PROFILE.jitterWindowMs;
    h.steady(Math.ceil((windowMs + 2000) / FRAME_INTERVAL_MS));
    expect(h.rb.arrivalJitterMs()!).toBeLessThan(QUANTILE_BIN_MS * 2);
    expect(windowMs).toBeLessThan(LIVE_EDGE_WINDOW_MS);
  });
});
