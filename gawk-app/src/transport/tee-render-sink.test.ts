// R16 (docs/21 U2, reworked in U4): the presentation tee. An idle tee
// delegates byte-identically (the paced sink behaves exactly as with a bare
// context sink); an armed tee writes clones of the *decoded frames it
// presents* to the generator writer — never a canvas readback (U4 second
// on-device pass: VideoFrame-from-WebGL-canvas content is black on iOS
// WebKit) — stamped from the tee's own zero-based clock (never the
// broadcaster's foreign PTS); coalesced/superseded frames never cross;
// write/clone failures count teeErrors and never throw into the paint path.

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

// Node has no VideoFrame; the tee constructs clones via
// new VideoFrame(sourceFrame, { timestamp }).
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
    delete teeGlobals.VideoFrame; // any clone attempt would throw → teeErrors
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner);

    expect(tee.kind).toBe('webgl');
    const f = frame(1000);
    tee.draw(f, 42);
    expect(calls).toEqual([{ frame: f, target: 42 }]);
    expect(tee.drawnFrames()).toBe(1);
    expect(tee.teeStats()).toEqual({ armed: false, teedFrames: 0, teeErrors: 0 });
  });

  it('does not expose upload/present over a non-interpolating inner sink', () => {
    const tee = new TeeRenderSink(plainInner().inner);
    expect(tee.upload).toBeUndefined();
    expect(tee.present).toBeUndefined();
    expect(new PacedPresentationSink(tee).supportsInterpolation).toBe(false);
  });

  it('passes the interpolation surface through so the paced sink sees it', () => {
    const tee = new TeeRenderSink(interpolatingInner().inner);
    expect(typeof tee.upload).toBe('function');
    expect(typeof tee.present).toBe('function');
    expect(new PacedPresentationSink(tee).supportsInterpolation).toBe(true);
  });

  it('behaves identically under the paced sink: coalescing skips unseen frames', () => {
    // Latest-frame-wins through the tee: two ASAP frames before the tick ⇒
    // the first closes unseen, the second paints — exactly the bare sink.
    const ticks: (() => void)[] = [];
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner);
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
  it('writes a clone of each drawn frame, stamped from its own zero-based clock', () => {
    // U4 findings: clones of the decoded frame itself (never a canvas
    // readback), with tee-local timestamps — source timestamps are
    // broadcaster-clock µs (foreign, backwards-jumping on restarts).
    const { inner, calls } = plainInner();
    let nowMs = 5_000;
    const tee = new TeeRenderSink(inner, () => nowMs);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    const a = frame(1_000_000);
    tee.draw(a);
    nowMs = 5_033;
    const b = frame(900_000); // source timestamps jumped backwards (restart)
    tee.draw(b);

    expect(written.map((w) => w.timestamp)).toEqual([0, 33_000]);
    expect(written.map((w) => w.source)).toEqual([a, b]);
    // The original frames still went through the paint path (which closes them).
    expect(calls.map((c) => c.frame)).toEqual([a, b]);
    expect(tee.teeStats()).toEqual({ armed: true, teedFrames: 2, teeErrors: 0 });
  });

  it('holds the clone from upload and writes it only at present(1); mids write nothing', () => {
    const { inner, uploads } = interpolatingInner();
    let nowMs = 100;
    const tee = new TeeRenderSink(inner, () => nowMs);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    // Real frame A: upload + present(1).
    const a = frame(1_000_000);
    tee.upload!(a);
    tee.present!(1);
    expect(written.map((w) => w.source)).toEqual([a]);
    // The paced sink uploads the NEXT real frame early, presents the mid…
    nowMs = 116;
    const b = frame(1_033_000);
    tee.upload!(b);
    tee.present!(0.5); // synthesized blend exists only on the canvas — no tee
    expect(written).toHaveLength(1);
    // …then presents the real frame from its already-uploaded texture.
    tee.present!(1);

    expect(uploads).toEqual([1_000_000, 1_033_000]);
    expect(written.map((w) => w.source)).toEqual([a, b]);
    expect(written.map((w) => w.timestamp)).toEqual([0, 16_000]);
    expect(tee.teeStats().teedFrames).toBe(2);
  });

  it('a superseding upload closes the held clone unseen', () => {
    // Paced-sink supersession: a newer real frame wins before the uploaded
    // one's slot — the paced sink uploads the winner without ever presenting
    // the loser. The loser's clone must close, not cross.
    const tee = new TeeRenderSink(interpolatingInner().inner);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    const a = frame(1_000_000);
    const b = frame(1_033_000);
    tee.upload!(a);
    tee.upload!(b); // supersedes A before any present
    tee.present!(1);

    expect(written.map((w) => w.source)).toEqual([b]);
    const aClone = written.find((w) => w.source === a);
    expect(aClone).toBeUndefined();
    expect(tee.teeStats().teedFrames).toBe(1);
  });

  it('only presented frames cross: paced-sink supersession never reaches the writer', () => {
    const ticks: (() => void)[] = [];
    const tee = new TeeRenderSink(plainInner().inner);
    const paced = new PacedPresentationSink(tee, (cb) => ticks.push(cb), () => 0);
    const { writer, written } = fakeWriter();
    tee.arm(writer);

    paced.draw(frame(0));
    paced.draw(frame(33_000)); // supersedes the first before any tick
    ticks.shift()!();

    expect(written).toHaveLength(1); // only the surviving frame's clone crossed
  });

  it('a rejected write counts teeErrors, closes the clone, and never throws', async () => {
    const tee = new TeeRenderSink(plainInner().inner);
    const { writer, written } = fakeWriter({ reject: true });
    tee.arm(writer);

    expect(() => tee.draw(frame(5_000))).not.toThrow();
    await Promise.resolve(); // let the rejection settle
    expect(tee.teeStats().teeErrors).toBe(1);
    expect(written[0].closed).toBe(true);
  });

  it('a clone-construction failure counts teeErrors and keeps painting', () => {
    teeGlobals.VideoFrame = class {
      constructor() {
        throw new DOMException('nope', 'InvalidStateError');
      }
    };
    const { inner, calls } = plainInner();
    const tee = new TeeRenderSink(inner);
    tee.arm(fakeWriter().writer);

    expect(() => tee.draw(frame(5_000))).not.toThrow();
    expect(calls).toHaveLength(1); // the inline paint still happened
    expect(tee.teeStats()).toEqual({ armed: true, teedFrames: 0, teeErrors: 1 });
  });
});

describe('probePresentationTee', () => {
  function installProbeCanvas() {
    teeGlobals.OffscreenCanvas = class {
      getContext() {
        return { fillRect: () => {} };
      }
    };
  }

  it('fails without VideoTrackGenerator', () => {
    installProbeCanvas();
    expect(getVideoTrackGenerator()).toBeUndefined();
    expect(probePresentationTee()).toBe(false);
  });

  it('passes when the generator exists and a trial frame + clone-with-timestamp work', () => {
    teeGlobals.VideoTrackGenerator = class {};
    installProbeCanvas();
    expect(probePresentationTee()).toBe(true);
  });

  it('fails when the trial VideoFrame throws (the pre-registered iOS fallback)', () => {
    teeGlobals.VideoTrackGenerator = class {};
    installProbeCanvas();
    teeGlobals.VideoFrame = class {
      constructor() {
        throw new DOMException('nope', 'InvalidStateError');
      }
    };
    expect(probePresentationTee()).toBe(false);
  });

  it('fails when clone-from-VideoFrame throws (the U4 load-bearing operation)', () => {
    teeGlobals.VideoTrackGenerator = class {};
    installProbeCanvas();
    // First construction (from the canvas) succeeds; the second — the
    // clone-with-timestamp from that frame, the operation the reworked tee
    // actually relies on — throws.
    let ctorCalls = 0;
    teeGlobals.VideoFrame = class {
      constructor() {
        if (++ctorCalls === 2) throw new DOMException('no frame-from-frame', 'TypeError');
      }
      close() {}
    };
    expect(probePresentationTee()).toBe(false);
  });
});
