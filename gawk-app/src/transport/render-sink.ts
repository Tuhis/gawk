// The viewer pipeline hands each decoded VideoFrame to a RenderSink instead of
// bouncing it across a callback boundary. On the main thread the sink draws to
// a 2D canvas; in the worker (R8 S6) it draws to a transferred OffscreenCanvas
// so decoded frames are painted *in the worker* and never postMessage-d back.
//
// R10 (docs/14): the worker composes sinks via createRenderSink() — a WebGL
// textured-quad sink (Firefox's 2D drawImage(VideoFrame) does a synchronous
// CPU conversion per frame; the 2D sink survives as fallback) wrapped in a
// coalescing sink that draws at most once per rAF tick, latest-frame-wins.
//
// Contract: draw() takes ownership of the frame and MUST close() it (single
// owner — the pipeline never touches the frame again).

import { log } from '../lib/logger';

// Which context ultimately paints — surfaced in ViewerStats.renderer so
// "is your Firefox actually on the WebGL sink?" is answerable remotely.
export type RenderSinkKind = '2d' | 'webgl';

export interface RenderSink {
  readonly kind: RenderSinkKind;
  draw(frame: VideoFrame): void;
  // Cumulative frames drawn (R9 M6): feeds the viewer funnel's renderedFps.
  drawnFrames(): number;
  // R12 T1: presentation-cadence jitter since the last drain; implemented by
  // the scheduling sink (the paint is the phenomenon under measurement), null
  // elsewhere and before two draws.
  drainCadence?(): RenderCadence | null;
}

// Presentation-cadence error per stats window (R12 T1, docs/17 Decision 1).
export interface RenderCadence {
  stdDevMs: number;
  p95Ms: number; // p95 of |error|
  samples: number;
}

// How much each draw interval deviated from its frames' capture interval:
// err = (draw-wall delta) − (frame-timestamp delta). Perfect pacing reads 0
// regardless of source fps — subtracting the timestamp delta is what makes
// this a jitter metric instead of conflating fps rung changes with judder.
// Coalescing skips are fine: both deltas span the same skipped frames.
const CADENCE_MAX_INTERVAL_MS = 5000; // beyond this, timestamps jumped timelines (restart)
const CADENCE_MAX_SAMPLES = 240; // bound if nothing drains (~4 s at 60 Hz)

export class CadenceRecorder {
  private lastTsMs: number | null = null;
  private lastWallMs: number | null = null;
  private errs: number[] = [];

  record(timestampUs: number, nowMs: number): void {
    const tsMs = timestampUs / 1000;
    if (!Number.isFinite(tsMs)) return;
    if (this.lastTsMs !== null && this.lastWallMs !== null) {
      const tsDelta = tsMs - this.lastTsMs;
      // A non-positive or absurd timestamp delta is a timeline discontinuity
      // (broadcaster restart): start a fresh pairing, don't record an outlier.
      if (tsDelta > 0 && tsDelta <= CADENCE_MAX_INTERVAL_MS) {
        this.errs.push(nowMs - this.lastWallMs - tsDelta);
        if (this.errs.length > CADENCE_MAX_SAMPLES) this.errs.shift();
      }
    }
    this.lastTsMs = tsMs;
    this.lastWallMs = nowMs;
  }

  // Destructive read: stats for the window since the last drain. The last
  // draw's timestamp survives so intervals stay continuous across drains.
  drain(): RenderCadence | null {
    const errs = this.errs;
    if (errs.length === 0) return null;
    this.errs = [];
    const mean = errs.reduce((a, b) => a + b, 0) / errs.length;
    const variance = errs.reduce((a, b) => a + (b - mean) * (b - mean), 0) / errs.length;
    const sortedAbs = errs.map(Math.abs).sort((a, b) => a - b);
    const p95 = sortedAbs[Math.max(0, Math.ceil(0.95 * sortedAbs.length) - 1)];
    return { stdDevMs: Math.sqrt(variance), p95Ms: p95, samples: errs.length };
  }

  reset(): void {
    this.lastTsMs = null;
    this.lastWallMs = null;
    this.errs = [];
  }
}

// Draws to an OffscreenCanvas transferred once from the main thread. The
// backing-store size tracks the frame's display size; the on-screen <canvas>
// element's CSS governs letterboxing, exactly as the main-thread path did.
export class OffscreenCanvasRenderSink implements RenderSink {
  readonly kind: RenderSinkKind = '2d';
  private canvas: OffscreenCanvas;
  private ctx: OffscreenCanvasRenderingContext2D | null;
  private drawn = 0;

  constructor(canvas: OffscreenCanvas) {
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
  }

  draw(frame: VideoFrame): void {
    const canvas = this.canvas;
    const ctx = this.ctx;
    if (ctx) {
      if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
        canvas.width = frame.displayWidth;
        canvas.height = frame.displayHeight;
      }
      ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
      this.drawn++;
    }
    frame.close();
  }

  drawnFrames(): number {
    return this.drawn;
  }
}

// All GL calls used below live on the WebGL1 base interface.
type GL = WebGLRenderingContext | WebGL2RenderingContext;

// Fullscreen quad; UVs flip Y so the top of the VideoFrame lands at the top
// of the canvas (clip space is bottom-up, images are top-down).
const VERTEX_SHADER = `
attribute vec2 pos;
varying vec2 uv;
void main() {
  uv = vec2((pos.x + 1.0) * 0.5, (1.0 - pos.y) * 0.5);
  gl_Position = vec4(pos, 0.0, 1.0);
}`;

const FRAGMENT_SHADER = `
precision mediump float;
varying vec2 uv;
uniform sampler2D tex;
void main() { gl_FragColor = texture2D(tex, uv); }`;

// Draws each VideoFrame as a WebGL texture on a fullscreen quad.
// texImage2D(VideoFrame) is the upload path every WebGL video player uses:
// synchronous, no per-frame allocation, and kept on the GPU where the
// platform allows — unlike Firefox's software 2D drawImage path (R10) or a
// bitmaprenderer sink's per-frame createImageBitmap hop.
//
// The constructor throws if program setup fails; createRenderSink() catches
// and falls back to the 2D sink.
export class WebGLRenderSink implements RenderSink {
  readonly kind: RenderSinkKind = 'webgl';
  private canvas: OffscreenCanvas;
  private gl: GL;
  private drawn = 0;

  constructor(canvas: OffscreenCanvas, gl: GL) {
    this.canvas = canvas;
    this.gl = gl;

    const program = gl.createProgram();
    if (!program) throw new Error('WebGL createProgram failed');
    gl.attachShader(program, compileShader(gl, gl.VERTEX_SHADER, VERTEX_SHADER));
    gl.attachShader(program, compileShader(gl, gl.FRAGMENT_SHADER, FRAGMENT_SHADER));
    gl.linkProgram(program);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
      throw new Error(`WebGL program link failed: ${gl.getProgramInfoLog(program) ?? ''}`);
    }
    gl.useProgram(program);

    const quad = gl.createBuffer();
    if (!quad) throw new Error('WebGL createBuffer failed');
    gl.bindBuffer(gl.ARRAY_BUFFER, quad);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);
    const pos = gl.getAttribLocation(program, 'pos');
    gl.enableVertexAttribArray(pos);
    gl.vertexAttribPointer(pos, 2, gl.FLOAT, false, 0, 0);

    const texture = gl.createTexture();
    if (!texture) throw new Error('WebGL createTexture failed');
    gl.bindTexture(gl.TEXTURE_2D, texture);
    // Video frames are never power-of-two: clamp + linear, no mips.
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
  }

  draw(frame: VideoFrame): void {
    const gl = this.gl;
    try {
      // A lost context makes every call a silent no-op; skip the work but
      // keep honoring the close contract. Recovery is a page concern.
      if (gl.isContextLost()) return;
      const canvas = this.canvas;
      if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
        canvas.width = frame.displayWidth;
        canvas.height = frame.displayHeight;
        gl.viewport(0, 0, canvas.width, canvas.height);
      }
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, frame);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
      this.drawn++;
    } finally {
      frame.close();
    }
  }

  drawnFrames(): number {
    return this.drawn;
  }
}

function compileShader(gl: GL, type: number, source: string): WebGLShader {
  const shader = gl.createShader(type);
  if (!shader) throw new Error('WebGL createShader failed');
  gl.shaderSource(shader, source);
  gl.compileShader(shader);
  if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
    throw new Error(`WebGL shader compile failed: ${gl.getShaderInfoLog(shader) ?? ''}`);
  }
  return shader;
}

export type RenderSchedule = (cb: () => void) => void;

// Worker rAF exists in Chrome and Firefox ≥ 105 (it shipped alongside
// OffscreenCanvas); the timer fallback keeps coalescing — the point — even
// where it doesn't.
const defaultSchedule: RenderSchedule =
  typeof requestAnimationFrame === 'function'
    ? (cb) => requestAnimationFrame(cb)
    : (cb) => void setTimeout(cb, 16);

// Latest-frame-wins pacing (R10 P1): at most one inner draw per scheduler
// tick; a frame still pending when a newer one decodes is closed unseen.
// Painting faster than the display refreshes is pure waste, and on Firefox
// each skipped 2D draw is worker-thread time handed back to the datagram
// reader and the decoder.
export class CoalescingRenderSink implements RenderSink {
  readonly kind: RenderSinkKind;
  private inner: RenderSink;
  private schedule: RenderSchedule;
  private now: () => number;
  private pending: VideoFrame | null = null;
  private flushScheduled = false;
  // R12 T1: cadence is recorded at the inner draw — the actual paint.
  private cadence = new CadenceRecorder();

  constructor(
    inner: RenderSink,
    schedule: RenderSchedule = defaultSchedule,
    now: () => number = () => performance.now(),
  ) {
    this.inner = inner;
    this.kind = inner.kind;
    this.schedule = schedule;
    this.now = now;
  }

  draw(frame: VideoFrame): void {
    this.pending?.close();
    this.pending = frame;
    if (!this.flushScheduled) {
      this.flushScheduled = true;
      this.schedule(() => this.flush());
    }
  }

  private flush(): void {
    this.flushScheduled = false;
    const frame = this.pending;
    this.pending = null;
    if (frame) {
      this.cadence.record(frame.timestamp, this.now());
      this.inner.draw(frame);
    }
  }

  drawnFrames(): number {
    return this.inner.drawnFrames();
  }

  drainCadence(): RenderCadence | null {
    return this.cadence.drain();
  }
}

// The worker's sink: WebGL2 → WebGL → 2D, coalescing-wrapped. Context flavors
// are tried in order; a canvas whose getContext() returned null is not bound
// to that flavor, so falling through is safe. (If WebGL context creation
// *succeeds* but program setup then throws, the canvas is stuck in WebGL mode
// and the 2D fallback will be blind — vanishingly rare, hence only logged.)
export function createRenderSink(
  canvas: OffscreenCanvas,
  schedule: RenderSchedule = defaultSchedule,
): RenderSink {
  return new CoalescingRenderSink(createContextSink(canvas), schedule);
}

function createContextSink(canvas: OffscreenCanvas): RenderSink {
  const opts = { alpha: false, antialias: false, depth: false, stencil: false };
  const gl = (canvas.getContext('webgl2', opts) ??
    canvas.getContext('webgl', opts)) as GL | null;
  if (gl) {
    try {
      return new WebGLRenderSink(canvas, gl);
    } catch (e) {
      log.warn('WebGL render sink init failed; falling back to 2D:', e);
    }
  }
  return new OffscreenCanvasRenderSink(canvas);
}
