// R16 (docs/21 U2): the presentation tee. An idle tee delegates
// byte-identically (the paced sink behaves exactly as with a bare context
// sink); an armed tee writes only *presented* frames to the generator writer
// (coalesced/superseded frames never cross), stamped from the tee's own
// zero-based clock (U4 black-screen finding: WebKit must never see the
// broadcaster's foreign PTS values); write failures count teeErrors and never
// throw into the paint path.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PacedPresentationSink, type RenderSink } from './render-sink';
import { setInterpolationEnabled } from './interpolation';
import { setPlayoutMode } from './playout';
import {
  TeeRenderSink,
  getVideoTrackGenerator,
  probePresentationTee,
  type TeeFrameWriter,
} from './tee-render-sink';

// Node has no VideoFrame; the tee constructs one from the canvas per capture.
class FakeVideoFrame {
  source: unknown;
  timestamp: number;
  closed = false;
  constructor(source: unknown, init: { timestamp: number }) {
    this.source = source;
    this.timestamp = init.timestamp;
  }
  close() {
    this.closed = true;
  }
}

const teeGlobals = globalThis as {
  VideoFrame?: unknown;
  OffscreenCanvas?: unknown;
  VideoTrackGenerator?: unknown;
};
let savedVideoFrame: unknown;

beforeEach(() => {
  savedVideoFrame = teeGlobals.VideoFrame;
  teeGlobals.VideoFrame = FakeVideoFrame;
});

afterEach(() => {
  teeGlobals.VideoFrame = savedVideoFrame;
  delete teeGlobals.OffscreenCanvas;
  delete teeGlobals.VideoTrackGenerator;
  setInterpolationEnabled(false);
  setPlayoutMode('off');
  vi.restoreAllMocks();
});

const canvas = {} as OffscreenCanvas;

function frame(timestampUs: number) {
  return {
    displayWidth: 640,
    displayHeight: 360,
    timestamp: timestampUs,
    close: vi.fn(),
  } as unknown as VideoFrame;
}

function plainInner() {
  let drawn = 0;
  const calls: { frame: VideoFrame; target?: number }[] = [];
  const inner: RenderSink = {
    kind: 'webgl',
    draw(f: VideoFrame, target?: number) {
      calls.push({ frame: f, target });
      drawn++;
      f.close();
    },
    drawnFrames: () => drawn,
  };
  return { inner, calls };
}

function interpolatingInner() {
  const uploads: number[] = [];
  const presents: number[] = [];
  let drawn = 0;
  const inner = {
    kind: 'webgl' as const,
    draw(f: VideoFrame) {
      uploads.push(f.timestamp);
      presents.push(1);
      drawn++;
      f.close();
    },
    upload(f: VideoFrame) {
      uploads.push(f.timestamp);
      f.close();
    },
    present(alpha: number) {
      presents.push(alpha);
      drawn++;
    },
    drawnFrames: () => drawn,
  };
  return { inner: inner as RenderSink, uploads, presents };
}

function fakeWriter({ reject = false } = {}) {
  const written: FakeVideoFrame[] = [];
  const writer: TeeFrameWriter = {
    write(f: VideoFrame) {
      written.push(f as unknown as FakeVideoFrame);
      return reject ? Promise.reject(new Error('sink gone')) : Promise.resolve();
    },
  };
  return { writer, written };
}

describe('TeeRenderSink idle (unarmed)', () => {
  it('delegates draw/kind/drawnFrames and constructs no VideoFrame', () => {
    delete teeGlobals.VideoFrame; // any capture attempt would throw → teeErrors
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner, canvas);

    expect(tee.kind).toBe('webgl');
    const f = frame(1000);
    tee.draw(f, 42);
    expect(calls).toEqual([{ frame: f, target: 42 }]);
    expect(tee.drawnFrames()).toBe(1);
    expect(tee.teeStats()).toEqual({ armed: false, teedFrames: 0, teeErrors: 0 });
  });

  it('does not expose upload/present over a non-interpolating inner sink', () => {
    const tee = new TeeRenderSink(plainInner().inner, canvas);
    expect(tee.upload).toBeUndefined();
    expect(tee.present).toBeUndefined();
    expect(new PacedPresentationSink(tee).supportsInterpolation).toBe(false);
  });

  it('passes the interpolation surface through so the paced sink sees it', () => {
    const tee = new TeeRenderSink(interpolatingInner().inner, canvas);
    expect(typeof tee.upload).toBe('function');
    expect(typeof tee.present).toBe('function');
    expect(new PacedPresentationSink(tee).supportsInterpolation).toBe(true);
  });

  it('behaves identically under the paced sink: coalescing skips unseen frames', () => {
    // Latest-frame-wins through the tee: two ASAP frames before the tick ⇒
    // the first closes unseen, the second paints — exactly the bare sink.
    const ticks: (() => void)[] = [];
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner, canvas);
    const paced = new PacedPresentationSink(tee, (cb) => ticks.push(cb), () => 0);

    const f1 = frame(0);
    const f2 = frame(33_000);
    paced.draw(f1);
    paced.draw(f2);
    ticks.shift()!();

    expect(f1.close).toHaveBeenCalled();
    expect(calls.map((c) => c.frame)).toEqual([f2]);
    expect(tee.teeStats().teedFrames).toBe(0); // idle: nothing crosses
  });
});

describe('TeeRenderSink armed', () => {
  it('captures the canvas after each draw, stamped from its own zero-based clock', () => {
    // U4 finding: source-frame timestamps are broadcaster-clock µs — foreign,
    // huge, and backwards-jumping on restarts. The generator stream gets the
    // tee's local capture clock instead: zero-based, monotonic, restart-proof.
    const { inner } = plainInner();
    let nowMs = 5_000;
    const tee = new TeeRenderSink(inner, canvas, () => nowMs);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    tee.draw(frame(1_000_000));
    nowMs = 5_033;
    tee.draw(frame(900_000)); // source timestamps jumped backwards (restart)

    expect(written.map((w) => w.timestamp)).toEqual([0, 33_000]);
    expect(written.every((w) => w.source === canvas)).toBe(true);
    expect(tee.teeStats()).toEqual({ armed: true, teedFrames: 2, teeErrors: 0 });
  });

  it('captures every present (real and mid) on the local clock, strictly monotonic', () => {
    const { inner, uploads } = interpolatingInner();
    let nowMs = 100;
    const tee = new TeeRenderSink(inner, canvas, () => nowMs);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    // Real frame A: upload + present(1).
    tee.upload!(frame(1_000_000));
    tee.present!(1);
    // The paced sink uploads the NEXT real frame early, presents the mid…
    nowMs = 116;
    tee.upload!(frame(1_033_000));
    tee.present!(0.5);
    // …then presents the real frame from its already-uploaded texture — the
    // clock stalled, so monotonicity comes from the +1 tie-break.
    tee.present!(1);

    expect(uploads).toEqual([1_000_000, 1_033_000]);
    expect(written.map((w) => w.timestamp)).toEqual([
      0, // A's slot: first capture defines the epoch
      16_000, // synthesized mid, at its own capture time
      16_001, // B's slot: stalled clock still yields a strictly-later stamp
    ]);
    expect(tee.teeStats().teedFrames).toBe(3);
  });

  it('only presented frames cross: paced-sink supersession never reaches the writer', () => {
    const ticks: (() => void)[] = [];
    const tee = new TeeRenderSink(plainInner().inner, canvas);
    const paced = new PacedPresentationSink(tee, (cb) => ticks.push(cb), () => 0);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    paced.draw(frame(0));
    paced.draw(frame(33_000)); // supersedes the first before any tick
    ticks.shift()!();

    expect(written).toHaveLength(1); // only the surviving frame's paint crossed
  });

  it('a rejected write counts teeErrors, closes the frame, and never throws', async () => {
    const tee = new TeeRenderSink(plainInner().inner, canvas);
    const { writer, written } = fakeWriter({ reject: true });
    tee.arm(writer);

    expect(() => tee.draw(frame(5_000))).not.toThrow();
    await Promise.resolve(); // let the rejection settle
    expect(tee.teeStats().teeErrors).toBe(1);
    expect(written[0].closed).toBe(true);
  });

  it('a VideoFrame construction failure counts teeErrors and keeps painting', () => {
    teeGlobals.VideoFrame = class {
      constructor() {
        throw new DOMException('nope', 'InvalidStateError');
      }
    };
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner, canvas);
    tee.arm(fakeWriter().writer);

    expect(() => tee.draw(frame(5_000))).not.toThrow();
    expect(calls).toHaveLength(1); // the inline paint still happened
    expect(tee.teeStats()).toEqual({ armed: true, teedFrames: 0, teeErrors: 1 });
  });
});

describe('probePresentationTee', () => {
  it('fails without VideoTrackGenerator', () => {
    teeGlobals.OffscreenCanvas = class {
      getContext() {
        return { fillRect: () => {} };
      }
    };
    expect(getVideoTrackGenerator()).toBeUndefined();
    expect(probePresentationTee()).toBe(false);
  });

  it('passes when the generator exists and a trial VideoFrame-from-canvas works', () => {
    teeGlobals.VideoTrackGenerator = class {};
    teeGlobals.OffscreenCanvas = class {
      getContext() {
        return { fillRect: () => {} };
      }
    };
    expect(probePresentationTee()).toBe(true);
  });

  it('fails when the trial VideoFrame throws (the pre-registered iOS fallback)', () => {
    teeGlobals.VideoTrackGenerator = class {};
    teeGlobals.OffscreenCanvas = class {
      getContext() {
        return { fillRect: () => {} };
      }
    };
    teeGlobals.VideoFrame = class {
      constructor() {
        throw new DOMException('nope', 'InvalidStateError');
      }
    };
    expect(probePresentationTee()).toBe(false);
  });
});
