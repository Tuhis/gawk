// R8 S6: OffscreenCanvasRenderSink resizes the backing store to the frame and
// always closes the frame (single-owner contract). No real OffscreenCanvas —
// a fake canvas/context is enough to pin the behavior.
// R10 (docs/14): CoalescingRenderSink paces draws to one per scheduler tick
// (latest-frame-wins), WebGLRenderSink uploads via texImage2D, and
// createRenderSink composes them with a 2D fallback.

import { describe, expect, it, vi } from 'vitest';
import {
  CoalescingRenderSink,
  OffscreenCanvasRenderSink,
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

function fakeFrame(w: number, h: number) {
  return { displayWidth: w, displayHeight: h, close: vi.fn() } as unknown as VideoFrame;
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

describe('CoalescingRenderSink (R10 P1)', () => {
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

  it('draws only the newest frame per tick and closes superseded ones unseen', () => {
    const { sink, drawn } = innerSink();
    const sched = manualSchedule();
    const coalescing = new CoalescingRenderSink(sink, sched.schedule);

    const a = fakeFrame(640, 480);
    const b = fakeFrame(640, 480);
    const c = fakeFrame(640, 480);
    coalescing.draw(a);
    coalescing.draw(b);
    coalescing.draw(c);

    expect(drawn).toHaveLength(0); // nothing painted before the tick
    expect(a.close).toHaveBeenCalledTimes(1);
    expect(b.close).toHaveBeenCalledTimes(1);
    expect(c.close).not.toHaveBeenCalled(); // ownership passed to inner on flush

    sched.fire();
    expect(drawn).toEqual([c]);
  });

  it('schedules at most one flush per tick', () => {
    const { sink } = innerSink();
    const sched = manualSchedule();
    const coalescing = new CoalescingRenderSink(sink, sched.schedule);

    coalescing.draw(fakeFrame(640, 480));
    coalescing.draw(fakeFrame(640, 480));
    expect(sched.scheduled()).toBe(1);
  });

  it('a frame arriving after a flush schedules a new tick', () => {
    const { sink, drawn } = innerSink();
    const sched = manualSchedule();
    const coalescing = new CoalescingRenderSink(sink, sched.schedule);

    coalescing.draw(fakeFrame(640, 480));
    sched.fire();
    expect(drawn).toHaveLength(1);

    coalescing.draw(fakeFrame(640, 480));
    expect(sched.scheduled()).toBe(1);
    sched.fire();
    expect(drawn).toHaveLength(2);
  });

  it('delegates drawnFrames and kind to the inner sink', () => {
    const { sink, drawn } = innerSink();
    const sched = manualSchedule();
    const coalescing = new CoalescingRenderSink(sink, sched.schedule);
    expect(coalescing.kind).toBe('2d');
    coalescing.draw(fakeFrame(640, 480));
    expect(coalescing.drawnFrames()).toBe(0); // pending, not painted
    sched.fire();
    expect(coalescing.drawnFrames()).toBe(drawn.length);
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

    expect(sink).toBeInstanceOf(CoalescingRenderSink);
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
