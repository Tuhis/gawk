// Striped deep-buffer reordering (docs/91 D9). Each connection carries whole
// GOPs, so GOP k+1 can arrive complete before GOP k. The reorder buffer holds
// it, waits for GOP k until the playout deadline — not the 250 ms delta-gap
// grace — then skips to the next keyframe, and drops a GOP that turns up after
// the skip, and every duplicate, without mistaking either for a broadcaster
// restart. The reassembler half: GOP k's deltas must survive completing after
// GOP k+1's.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { ReorderBuffer, type ReleasedFrame } from './reorder-buffer';
import { RESILIENT_DELTA_GAP_GRACE_MS, setLossAllowanceFrames, setViewerDeliveryModeFlag } from './resilient';
import { Reassembler, type AssembledFrame } from './reassembler';
import { encodeVideoChunk } from './wire';

// 4-frame GOPs at 10 fps: frame n has timestamp n × 100 ms. A GOP is pushed
// no earlier than its last frame's timestamp (complete(k)), as a live source
// would allow, so the arrival baseline is 0 and frame n is due at
// n × 100 + OFFSET_MS. (A GOP arriving earlier would lower the baseline: the
// fastest connection sets the schedule and the others are judged against it.)
const GOP = 4;
const FRAME_MS = 100;
const OFFSET_MS = 1000;
const due = (frameId: number) => frameId * FRAME_MS + OFFSET_MS;
const complete = (k: number) => (k * GOP + GOP - 1) * FRAME_MS;

function harness(outOfOrder = true) {
  const released: ReleasedFrame[] = [];
  const clock = { t: 0 };
  let restarts = 0;
  const rb = new ReorderBuffer((f) => released.push(f), () => clock.t, {
    playoutOffsetMs: () => OFFSET_MS,
    decodeLeadMs: () => 0,
    gopsOutOfOrder: () => outOfOrder,
    onRestart: () => restarts++,
  });
  // Advances the clock in small steps so every due frame is released on time.
  const runTo = (t: number) => {
    while (clock.t < t) {
      clock.t = Math.min(t, clock.t + 10);
      rb.tick();
    }
  };
  // One whole GOP, as one connection delivers it: keyframe, then its deltas.
  const pushGop = (k: number, frameIdBase = 0, tsBase = 0) => {
    for (let i = 0; i < GOP; i++) {
      const frameId = frameIdBase + k * GOP + i;
      const f = {
        frameId,
        timestampUs: BigInt(tsBase + (k * GOP + i) * FRAME_MS * 1000),
        data: new Uint8Array([frameId & 0xff]),
      };
      if (i === 0) rb.pushKeyframe({ ...f, config: null });
      else rb.pushDelta(f);
    }
  };
  return {
    rb,
    clock,
    runTo,
    pushGop,
    ids: () => released.map((f) => f.frameId),
    restarts: () => restarts,
  };
}

const range = (from: number, to: number) => Array.from({ length: to - from }, (_, i) => from + i);

beforeEach(() => {
  setViewerDeliveryModeFlag('deep');
  setLossAllowanceFrames(0);
});
afterEach(() => {
  setViewerDeliveryModeFlag('live');
  setLossAllowanceFrames(1);
});

describe('ReorderBuffer with GOPs out of order (docs/91 D9)', () => {
  it('holds a complete later GOP while the earlier one is still arriving, then plays both in order', () => {
    const h = harness();
    h.runTo(complete(0));
    h.pushGop(0);
    h.runTo(complete(2));
    h.pushGop(2); // GOP 2 wins the race on its own connection
    h.runTo(due(3) + 50); // GOP 0 played out; frame 4 is missing
    expect(h.ids()).toEqual(range(0, 4));
    h.pushGop(1); // GOP 1 arrives before its deadline
    h.runTo(due(11));
    expect(h.ids()).toEqual(range(0, 12));
    expect(h.rb.getStats()).toMatchObject({ gapResyncs: 0, holeSkips: 0, lateFramesDropped: 0 });
  });

  it('plays a three-way interleave in frameId order', () => {
    const h = harness();
    // Arrival order across three connections: 0, 2, 1, 4, 3, 5 — each
    // straggler well inside its deadline.
    for (const [k, at] of [[0, complete(0)], [2, complete(2)], [1, complete(2) + 50], [4, complete(4)], [3, complete(4) + 50], [5, complete(5)]] as const) {
      h.runTo(at);
      h.pushGop(k);
    }
    h.runTo(due(23));
    expect(h.ids()).toEqual(range(0, 24));
    expect(h.rb.getStats().gapResyncs).toBe(0);
  });

  it('waits for a missing GOP past the delta-gap grace, until its deadline', () => {
    const h = harness();
    h.runTo(complete(0));
    h.pushGop(0);
    h.runTo(complete(2));
    h.pushGop(2);
    // Past the grace an unstriped carrier viewer would allow.
    h.runTo(due(3) + RESILIENT_DELTA_GAP_GRACE_MS + 100);
    expect(h.ids()).toEqual(range(0, 4));
    expect(h.rb.getStats().holeSkips).toBe(0);
    // Just short of GOP 2's keyframe coming due, still waiting.
    h.runTo(due(8) - 10);
    expect(h.ids()).toEqual(range(0, 4));
    // At it: GOP 1 can no longer play on time, so it is skipped.
    h.runTo(due(8));
    expect(h.ids()).toEqual([...range(0, 4), 8]);
    expect(h.rb.getStats()).toMatchObject({ holeSkips: 1, gapResyncs: 1 });
    h.runTo(due(11));
    expect(h.ids()).toEqual([...range(0, 4), ...range(8, 12)]);
  });

  it('drops a GOP that arrives after it was skipped, without treating it as a restart', () => {
    const h = harness();
    h.runTo(complete(0));
    h.pushGop(0);
    h.runTo(complete(2));
    h.pushGop(2);
    h.runTo(due(9));
    h.pushGop(1); // too late: 8 and 9 are already out
    h.runTo(due(11));
    expect(h.ids()).toEqual([...range(0, 4), ...range(8, 12)]);
    expect(h.restarts()).toBe(0);
    expect(h.rb.getStats()).toMatchObject({ lateFramesDropped: GOP });
    // And the stream carries on.
    h.pushGop(3);
    h.runTo(due(15));
    expect(h.ids()).toEqual([...range(0, 4), ...range(8, 16)]);
    // And a copy of a GOP that did play is a duplicate.
    h.pushGop(2);
    expect(h.rb.getStats()).toMatchObject({ lateFramesDropped: GOP, duplicateFrames: GOP });
  });

  it('drops duplicate keyframes and deltas by frameId, held or already played', () => {
    const h = harness();
    h.runTo(complete(0));
    h.pushGop(0);
    h.pushGop(0); // served twice while still held (StripeState expiry)
    h.runTo(complete(1));
    h.pushGop(1);
    h.runTo(due(7));
    h.pushGop(1); // and again after it played (a width transition)
    h.pushGop(2);
    h.runTo(due(11));
    expect(h.ids()).toEqual(range(0, 12));
    expect(h.restarts()).toBe(0);
    // A re-arrival of a frame already played is a duplicate, not a late
    // frame: late is reserved for frames that were skipped as a hole
    // (review round 1, finding 5).
    const s = h.rb.getStats();
    expect(s.duplicateFrames).toBe(2 * GOP);
    expect(s.lateFramesDropped).toBe(0);
  });

  it('still recognises a broadcaster restart: a keyframe far behind on a new timeline', () => {
    const h = harness();
    // An old session 100 s in.
    h.pushGop(0, 1000, 100_000_000);
    h.runTo(due(3) + 50);
    expect(h.ids()).toHaveLength(GOP);
    // The new session numbers from 0 and starts its clock at 0.
    h.rb.pushKeyframe({ frameId: 0, timestampUs: 0n, config: null, data: new Uint8Array([0]) });
    expect(h.restarts()).toBe(1);
  });

  it('leaves the unstriped path alone: a buffered later keyframe still ends the wait at once', () => {
    const h = harness(false);
    h.runTo(complete(0));
    h.pushGop(0);
    h.runTo(complete(2));
    h.pushGop(2);
    h.runTo(due(3) + 50);
    // Unstriped, the keyframe ahead is a resync point the moment frame 4 is
    // missing; nothing waits for a deadline.
    expect(h.rb.getStats()).toMatchObject({ gapResyncs: 1, holeSkips: 1 });
  });
});

describe('Reassembler with frames out of order (docs/91 D9)', () => {
  const chunk = (frameId: number) =>
    encodeVideoChunk(
      { keyframe: false, frameId, chunkIndex: 0, chunkCount: 1, timestampUs: BigInt(frameId * 1000) },
      new Uint8Array([frameId & 0xff]),
    );

  function reassembler(outOfOrder: boolean) {
    const frames: AssembledFrame[] = [];
    const r = new Reassembler({
      onConfig: () => {},
      onFrame: (f) => frames.push(f),
      framesOutOfOrder: () => outOfOrder,
    });
    return { r, ids: () => frames.map((f) => f.frameId) };
  }

  it("emits GOP k's deltas after GOP k+1's completed, and drops a duplicate", () => {
    const { r, ids } = reassembler(true);
    r.noteStreamKeyframe(4);
    for (const id of [5, 6, 7]) r.push(chunk(id)); // GOP 1 first
    r.noteStreamKeyframe(0);
    for (const id of [1, 2, 3]) r.push(chunk(id)); // then GOP 0
    r.push(chunk(2)); // served twice: dropped at the chunk, no phantom assembly
    expect(ids()).toEqual([5, 6, 7, 1, 2, 3]);
    expect(r.getStats()).toMatchObject({ framesDroppedLate: 0, staleChunks: 1, framesDroppedIncomplete: 0 });
  });

  it('keeps the watermark when frames arrive in order', () => {
    const { r, ids } = reassembler(false);
    r.noteStreamKeyframe(4);
    for (const id of [5, 6, 7]) r.push(chunk(id));
    for (const id of [1, 2, 3]) r.push(chunk(id));
    expect(ids()).toEqual([5, 6, 7]);
  });
});
