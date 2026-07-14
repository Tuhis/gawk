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

  it('a keyframe supersedes a same-id delta already buffered', () => {
    const { rb, released, ids } = harness();
    rb.pushKeyframe(kf(0));
    rb.pushDelta(delta(3)); // buffered behind a gap (1,2 missing)
    rb.pushKeyframe(kf(3)); // same id arrives as a keyframe: it wins and resyncs
    expect(ids()).toEqual([0, 3]);
    expect(released.find((f) => f.frameId === 3)!.keyframe).toBe(true);
  });
});
