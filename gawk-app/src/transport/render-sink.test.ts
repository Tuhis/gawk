// R8 S6: OffscreenCanvasRenderSink resizes the backing store to the frame and
// always closes the frame (single-owner contract). No real OffscreenCanvas —
// a fake canvas/context is enough to pin the behavior.
// R10 (docs/14) + R12 (docs/17): PacedPresentationSink schedules at most one
// draw per tick (latest-frame-wins; display-slot pacing when targets are
// given), WebGLRenderSink uploads via texImage2D, and createRenderSink
// composes them with a 2D fallback.

import { describe, expect, it, vi } from 'vitest';
import {
  CadenceRecorder,
  DisplayIntervalEstimator,
  MAX_HELD_FRAMES,
  OffscreenCanvasRenderSink,
  PacedPresentationSink,
  WebGLRenderSink,
  createRenderSink,
  type RenderSink,
} from './render-sink';

// Fake OffscreenCanvas whose width/height are accessors so we can count writes
// (a plain data property can't distinguish "assigned same value" from "skipped").
function fakeCanvas(getContext: (type: string) => unknown = () => ({ drawImage: vi.fn() })) {
  const ctx = { drawImage: vi.fn() };
  let w = 0;
  let h = 0;
  let writes = 0;
  const canvas = {
    get width() {
      return w;
    },
    set width(v: number) {
      writes++;
      w = v;
    },
    get height() {
      return h;
    },
    set height(v: number) {
      writes++;
      h = v;
    },
    getContext: vi.fn((type: string) => (type === '2d' ? ctx : getContext(type))),
  };
  return { canvas: canvas as unknown as OffscreenCanvas, ctx, writes: () => writes };
}

function fakeFrame(w: number, h: number, timestampUs = 0) {
  return { displayWidth: w, displayHeight: h, timestamp: timestampUs, close: vi.fn() } as unknown as VideoFrame;
}

// Minimal WebGL fake: constants the sink touches, truthy handles, passing
// compile/link status. Only what the sink calls is modeled.
function fakeGL({ contextLost = false } = {}) {
  const gl = {
    VERTEX_SHADER: 1,
    FRAGMENT_SHADER: 2,
    LINK_STATUS: 3,
    COMPILE_STATUS: 4,
    ARRAY_BUFFER: 5,
    STATIC_DRAW: 6,
    FLOAT: 7,
    TEXTURE_2D: 8,
    TEXTURE_WRAP_S: 9,
    TEXTURE_WRAP_T: 10,
    TEXTURE_MIN_FILTER: 11,
    TEXTURE_MAG_FILTER: 12,
    CLAMP_TO_EDGE: 13,
    LINEAR: 14,
    RGBA: 15,
    UNSIGNED_BYTE: 16,
    TRIANGLE_STRIP: 17,
    createProgram: vi.fn(() => ({})),
    createShader: vi.fn(() => ({})),
    shaderSource: vi.fn(),
    compileShader: vi.fn(),
    getShaderParameter: vi.fn(() => true),
    getShaderInfoLog: vi.fn(() => ''),
    attachShader: vi.fn(),
    linkProgram: vi.fn(),
    getProgramParameter: vi.fn(() => true),
    getProgramInfoLog: vi.fn(() => ''),
    useProgram: vi.fn(),
    createBuffer: vi.fn(() => ({})),
    bindBuffer: vi.fn(),
    bufferData: vi.fn(),
    getAttribLocation: vi.fn(() => 0),
    enableVertexAttribArray: vi.fn(),
    vertexAttribPointer: vi.fn(),
    createTexture: vi.fn(() => ({})),
    bindTexture: vi.fn(),
    texParameteri: vi.fn(),
    isContextLost: vi.fn(() => contextLost),
    viewport: vi.fn(),
    texImage2D: vi.fn(),
    drawArrays: vi.fn(),
  };
  return gl as unknown as WebGLRenderingContext & typeof gl;
}

describe('OffscreenCanvasRenderSink', () => {
  it('sizes the canvas to the frame, draws it, and closes it', () => {
    const { canvas, ctx } = fakeCanvas();
    const sink = new OffscreenCanvasRenderSink(canvas);
    const frame = fakeFrame(1280, 720);

    sink.draw(frame);

    expect(canvas.width).toBe(1280);
    expect(canvas.height).toBe(720);
    expect(ctx.drawImage).toHaveBeenCalledWith(frame, 0, 0, 1280, 720);
    expect(frame.close).toHaveBeenCalledTimes(1);
  });

  it('only resizes when the frame dimensions change', () => {
    const { canvas, writes } = fakeCanvas();
    const sink = new OffscreenCanvasRenderSink(canvas);
    sink.draw(fakeFrame(640, 480));
    expect(writes()).toBe(2); // width + height set once
    sink.draw(fakeFrame(640, 480));
    expect(writes()).toBe(2); // unchanged: no further writes
  });

  it('still closes the frame when no 2D context is available', () => {
    const canvas = { width: 0, height: 0, getContext: () => null } as unknown as OffscreenCanvas;
    const sink = new OffscreenCanvasRenderSink(canvas);
    const frame = fakeFrame(320, 240);
    sink.draw(frame);
    expect(frame.close).toHaveBeenCalledTimes(1);
  });

  it('counts drawn frames for the viewer funnel (R9 M6)', () => {
    const { canvas } = fakeCanvas();
    const sink = new OffscreenCanvasRenderSink(canvas);
    expect(sink.drawnFrames()).toBe(0);
    sink.draw(fakeFrame(640, 480));
    sink.draw(fakeFrame(640, 480));
    expect(sink.drawnFrames()).toBe(2);

    // A context-less draw closes the frame but renders nothing — not drawn.
    const noCtx = { width: 0, height: 0, getContext: () => null } as unknown as OffscreenCanvas;
    const blindSink = new OffscreenCanvasRenderSink(noCtx);
    blindSink.draw(fakeFrame(320, 240));
    expect(blindSink.drawnFrames()).toBe(0);
  });

  it('reports kind 2d', () => {
    expect(new OffscreenCanvasRenderSink(fakeCanvas().canvas).kind).toBe('2d');
  });
});

function innerSink() {
  const drawn: VideoFrame[] = [];
  const sink: RenderSink = {
    kind: '2d',
    draw: (f) => drawn.push(f),
    drawnFrames: () => drawn.length,
  };
  return { sink, drawn };
}

// Manual scheduler: collects callbacks; fire() runs one tick.
function manualSchedule() {
  const pending: (() => void)[] = [];
  return {
    schedule: (cb: () => void) => pending.push(cb),
    fire: () => pending.splice(0).forEach((cb) => cb()),
    scheduled: () => pending.length,
  };
}

function pacedHarness() {
  const { sink, drawn } = innerSink();
  const sched = manualSchedule();
  const clock = { t: 1000 };
  const paced = new PacedPresentationSink(sink, sched.schedule, () => clock.t);
  return { paced, drawn, sched, clock };
}

// R12 T2: with no target (the default path), the paced sink IS the old
// coalescing sink — hold ≤1, newest wins, ≤1 inner draw per tick. These are
// the R10 P1 tests, ported.
describe('PacedPresentationSink no-target mode (R10 P1 semantics)', () => {
  it('draws only the newest frame per tick and closes superseded ones unseen', () => {
    const { paced, drawn, sched } = pacedHarness();

    const a = fakeFrame(640, 480);
    const b = fakeFrame(640, 480);
    const c = fakeFrame(640, 480);
    paced.draw(a);
    paced.draw(b);
    paced.draw(c);

    expect(drawn).toHaveLength(0); // nothing painted before the tick
    expect(a.close).toHaveBeenCalledTimes(1);
    expect(b.close).toHaveBeenCalledTimes(1);
    expect(c.close).not.toHaveBeenCalled(); // ownership passed to inner on flush

    sched.fire();
    expect(drawn).toEqual([c]);
  });

  it('schedules at most one flush per tick', () => {
    const { paced, sched } = pacedHarness();
    paced.draw(fakeFrame(640, 480));
    paced.draw(fakeFrame(640, 480));
    expect(sched.scheduled()).toBe(1);
  });

  it('a frame arriving after a flush schedules a new tick', () => {
    const { paced, drawn, sched } = pacedHarness();

    paced.draw(fakeFrame(640, 480));
    sched.fire();
    expect(drawn).toHaveLength(1);

    paced.draw(fakeFrame(640, 480));
    expect(sched.scheduled()).toBe(1);
    sched.fire();
    expect(drawn).toHaveLength(2);
  });

  it('delegates drawnFrames and kind to the inner sink', () => {
    const { paced, drawn, sched } = pacedHarness();
    expect(paced.kind).toBe('2d');
    paced.draw(fakeFrame(640, 480));
    expect(paced.drawnFrames()).toBe(0); // pending, not painted
    sched.fire();
    expect(paced.drawnFrames()).toBe(drawn.length);
  });
});

// R12 T2 (docs/17 Decision 3): with a target display time, frames are held
// (≤ MAX_HELD_FRAMES) and presented in their vsync slot — the newest due
// frame wins, older ones close unseen, and pacing never queue-grows.
describe('PacedPresentationSink paced mode (R12 T2)', () => {
  it('holds an early frame and presents it once its slot is due', () => {
    const { paced, drawn, sched, clock } = pacedHarness();
    const f = fakeFrame(640, 480, 0);
    paced.draw(f, 1100);

    sched.fire(); // t=1000: slot 1100 is beyond now + half-vsync — hold
    expect(drawn).toHaveLength(0);
    expect(f.close).not.toHaveBeenCalled();
    expect(sched.scheduled()).toBe(1); // keeps ticking while frames are held

    clock.t = 1100;
    sched.fire();
    expect(drawn).toEqual([f]);
    expect(sched.scheduled()).toBe(0); // nothing held — stops ticking
  });

  it('a slot due within half a display interval presents now (no extra vsync of delay)', () => {
    const { paced, drawn, sched, clock } = pacedHarness();
    clock.t = 1000;
    paced.draw(fakeFrame(640, 480, 0), 1006); // within 16.7/2 ms of now
    sched.fire();
    expect(drawn).toHaveLength(1);
  });

  it('presents the newest due frame and closes older due frames unseen', () => {
    const { paced, drawn, sched, clock } = pacedHarness();
    const a = fakeFrame(640, 480, 0);
    const b = fakeFrame(640, 480, 33_333);
    paced.draw(a, 1000);
    paced.draw(b, 1033);
    clock.t = 1040; // both slots passed
    sched.fire();
    expect(drawn).toEqual([b]);
    expect(a.close).toHaveBeenCalledTimes(1);
    expect(paced.pacingDropped()).toBe(1);
  });

  it('never holds more than MAX_HELD_FRAMES — the oldest closes', () => {
    const { paced, sched, clock } = pacedHarness();
    const frames = Array.from({ length: MAX_HELD_FRAMES + 1 }, (_, i) => fakeFrame(640, 480, i));
    frames.forEach((f, i) => paced.draw(f, 2000 + i * 33));
    expect(frames[0].close).toHaveBeenCalledTimes(1); // overflow: oldest out
    frames.slice(1).forEach((f) => expect(f.close).not.toHaveBeenCalled());

    clock.t = 3000;
    sched.fire(); // all due: newest paints, the rest close
    expect(paced.pacingDropped()).toBe(MAX_HELD_FRAMES); // 1 overflow + (MAX−1) superseded
  });

  it('flush() closes everything held and presents nothing', () => {
    const { paced, drawn, clock } = pacedHarness();
    clock.t = 1000;
    const a = fakeFrame(640, 480, 0);
    const b = fakeFrame(640, 480, 33_333);
    paced.draw(a, 1100);
    paced.draw(b, 1133);
    paced.flush();
    expect(a.close).toHaveBeenCalledTimes(1);
    expect(b.close).toHaveBeenCalledTimes(1);
    expect(drawn).toHaveLength(0);
  });

  it('flush(true) presents the newest held frame immediately (mode toggled off)', () => {
    const { paced, drawn, clock } = pacedHarness();
    clock.t = 1000;
    const a = fakeFrame(640, 480, 0);
    const b = fakeFrame(640, 480, 33_333);
    paced.draw(a, 1100);
    paced.draw(b, 1133);
    paced.flush(true);
    expect(drawn).toEqual([b]);
    expect(a.close).toHaveBeenCalledTimes(1);
  });

  it('a no-target frame after paced frames presents next tick without waiting', () => {
    const { paced, drawn, sched, clock } = pacedHarness();
    clock.t = 1000;
    paced.draw(fakeFrame(640, 480, 0), 1150); // held, not due for 150 ms
    paced.draw(fakeFrame(640, 480, 33_333)); // toggle raced: immediate frame
    sched.fire();
    expect(drawn).toHaveLength(1); // the immediate frame — no 150 ms stall
  });
});

describe('DisplayIntervalEstimator (R12 T2)', () => {
  it('defaults to ~60 Hz before enough ticks arrive', () => {
    const e = new DisplayIntervalEstimator();
    expect(e.intervalMs()).toBeCloseTo(1000 / 60, 1);
  });

  it('reads the median tick delta', () => {
    const e = new DisplayIntervalEstimator();
    let t = 0;
    for (let i = 0; i < 10; i++) {
      e.recordTick(t);
      t += 8.333; // 120 Hz display
    }
    expect(e.intervalMs()).toBeCloseTo(8.333, 2);
  });

  it('ignores pauses (huge deltas) instead of skewing the median', () => {
    const e = new DisplayIntervalEstimator();
    let t = 0;
    for (let i = 0; i < 8; i++) {
      e.recordTick(t);
      t += 16.7;
    }
    e.recordTick(t + 5000); // tab hidden for 5 s — not a display interval
    for (let i = 0; i < 3; i++) {
      t += 16.7;
      e.recordTick(t + 5000);
    }
    expect(e.intervalMs()).toBeCloseTo(16.7, 1);
  });
});

describe('CadenceRecorder (R12 T1)', () => {
  it('reads zero error for perfectly paced draws', () => {
    const r = new CadenceRecorder();
    // 30 fps source presented exactly on its capture cadence.
    for (let i = 0; i < 10; i++) r.record(i * 33_333, 1000 + i * 33.333);
    const s = r.drain();
    expect(s).not.toBeNull();
    expect(s!.samples).toBe(9);
    expect(s!.stdDevMs).toBeCloseTo(0, 3);
    expect(s!.p95Ms).toBeCloseTo(0, 3);
  });

  it('measures jitter as the excess of draw-interval over timestamp-interval', () => {
    const r = new CadenceRecorder();
    // Constant 33.333 ms source cadence, but draws alternate early/late by
    // ±16.667 ms (the classic 1-vsync/3-vsync beat on a 60 Hz display).
    let wall = 1000;
    for (let i = 0; i < 12; i++) {
      r.record(i * 33_333, wall);
      wall += i % 2 === 0 ? 16.667 : 50;
    }
    const s = r.drain()!;
    expect(s.stdDevMs).toBeGreaterThan(15);
    expect(s.p95Ms).toBeGreaterThan(15);
  });

  it('does not conflate a source cadence change with jitter', () => {
    const r = new CadenceRecorder();
    // An fps rung switch: 60 fps → 30 fps, every frame drawn exactly on time.
    let wall = 1000;
    let ts = 0;
    for (let i = 0; i < 6; i++) {
      r.record(ts, wall);
      ts += 16_667;
      wall += 16.667;
    }
    for (let i = 0; i < 6; i++) {
      r.record(ts, wall);
      ts += 33_333;
      wall += 33.333;
    }
    expect(r.drain()!.stdDevMs).toBeCloseTo(0, 2);
  });

  it('drains destructively and is null with fewer than two draws', () => {
    const r = new CadenceRecorder();
    expect(r.drain()).toBeNull();
    r.record(0, 1000);
    expect(r.drain()).toBeNull(); // one draw = no interval yet
    r.record(33_333, 1040);
    expect(r.drain()).not.toBeNull();
    expect(r.drain()).toBeNull(); // drained
  });

  it('treats a timestamp discontinuity (broadcaster restart) as a fresh pairing', () => {
    const r = new CadenceRecorder();
    r.record(1_000_000, 1000);
    r.record(0, 1033); // timestamps jumped backwards: new timeline, not a sample
    r.record(33_333, 1066);
    const s = r.drain()!;
    expect(s.samples).toBe(1); // only the post-restart pair
    expect(Math.abs(s.stdDevMs)).toBeLessThan(1);
  });
});

describe('PacedPresentationSink cadence (R12 T1)', () => {
  it('records presentation cadence at the inner draw and drains it', () => {
    const { paced, sched, clock } = pacedHarness();

    paced.draw(fakeFrame(640, 480, 0));
    sched.fire();
    clock.t += 40; // 33.333 ms of content presented 40 ms apart → ~6.7 ms error
    paced.draw(fakeFrame(640, 480, 33_333));
    sched.fire();

    const s = paced.drainCadence();
    expect(s).not.toBeNull();
    expect(s!.samples).toBe(1);
    expect(s!.p95Ms).toBeCloseTo(6.667, 2);
    expect(paced.drainCadence()).toBeNull();
  });
});

describe('WebGLRenderSink (R10 P2)', () => {
  it('uploads the frame via texImage2D, draws the quad, and closes the frame', () => {
    const { canvas } = fakeCanvas();
    const gl = fakeGL();
    const sink = new WebGLRenderSink(canvas, gl);
    const frame = fakeFrame(1280, 720);

    sink.draw(frame);

    expect(gl.texImage2D).toHaveBeenCalledWith(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, frame);
    expect(gl.drawArrays).toHaveBeenCalledWith(gl.TRIANGLE_STRIP, 0, 4);
    expect(frame.close).toHaveBeenCalledTimes(1);
    expect(sink.drawnFrames()).toBe(1);
    expect(sink.kind).toBe('webgl');
  });

  it('resizes canvas + viewport only when frame dimensions change', () => {
    const { canvas, writes } = fakeCanvas();
    const gl = fakeGL();
    const sink = new WebGLRenderSink(canvas, gl);

    sink.draw(fakeFrame(640, 480));
    expect(writes()).toBe(2);
    expect(gl.viewport).toHaveBeenCalledWith(0, 0, 640, 480);
    sink.draw(fakeFrame(640, 480));
    expect(writes()).toBe(2);
    expect(gl.viewport).toHaveBeenCalledTimes(1);
  });

  it('closes the frame without drawing on a lost context', () => {
    const { canvas } = fakeCanvas();
    const gl = fakeGL({ contextLost: true });
    const sink = new WebGLRenderSink(canvas, gl);
    const frame = fakeFrame(640, 480);

    sink.draw(frame);

    expect(gl.texImage2D).not.toHaveBeenCalled();
    expect(frame.close).toHaveBeenCalledTimes(1);
    expect(sink.drawnFrames()).toBe(0);
  });

  it('closes the frame even when a GL call throws (single-owner contract)', () => {
    const { canvas } = fakeCanvas();
    const gl = fakeGL();
    gl.texImage2D.mockImplementation(() => {
      throw new Error('upload failed');
    });
    const sink = new WebGLRenderSink(canvas, gl);
    const frame = fakeFrame(640, 480);

    expect(() => sink.draw(frame)).toThrow('upload failed');
    expect(frame.close).toHaveBeenCalledTimes(1);
  });

  it('throws from the constructor when program setup fails', () => {
    const { canvas } = fakeCanvas();
    const gl = fakeGL();
    gl.getProgramParameter.mockReturnValue(false);
    expect(() => new WebGLRenderSink(canvas, gl)).toThrow(/link failed/);
  });
});

describe('createRenderSink (R10 P2)', () => {
  it('prefers WebGL and wraps it in the coalescing sink', () => {
    const gl = fakeGL();
    const { canvas } = fakeCanvas((type) => (type === 'webgl2' ? gl : null));
    const sched: (() => void)[] = [];
    const sink = createRenderSink(canvas, (cb) => sched.push(cb));

    expect(sink).toBeInstanceOf(PacedPresentationSink);
    expect(sink.kind).toBe('webgl');

    // Draws coalesce and land on the GL context.
    sink.draw(fakeFrame(640, 480));
    expect(gl.texImage2D).not.toHaveBeenCalled();
    sched.splice(0).forEach((cb) => cb());
    expect(gl.texImage2D).toHaveBeenCalledTimes(1);
  });

  it('falls back to the 2D sink when no WebGL context is available', () => {
    const { canvas } = fakeCanvas(() => null); // webgl2/webgl → null, 2d works
    const sink = createRenderSink(canvas, (cb) => cb());
    expect(sink.kind).toBe('2d');
    const frame = fakeFrame(320, 240);
    sink.draw(frame);
    expect(frame.close).toHaveBeenCalledTimes(1);
    expect(sink.drawnFrames()).toBe(1);
  });

  it('falls back to the 2D sink when WebGL setup throws', () => {
    const gl = fakeGL();
    gl.getProgramParameter.mockReturnValue(false); // link failure → constructor throws
    const { canvas } = fakeCanvas((type) => (type === 'webgl2' ? gl : null));
    const sink = createRenderSink(canvas, (cb) => cb());
    expect(sink.kind).toBe('2d');
  });
});
