// S5 (docs/12 Decision 7): the reorder buffer merges reliable stream keyframes
// with lossy datagram deltas by frameId, releasing in decode order with two
// bounded waits and NO fixed playout offset. A controlled clock drives the
// time-based decisions deterministically.

import { describe, expect, it } from 'vitest';

import {
  ReorderBuffer,
  DELTA_GAP_GRACE_MS,
  KEYFRAME_WAIT_MS,
  MAX_BUFFERED_FRAMES,
  type ReleasedFrame,
} from './reorder-buffer';

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
