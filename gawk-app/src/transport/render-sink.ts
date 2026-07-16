// The viewer pipeline hands each decoded VideoFrame to a RenderSink instead of
// bouncing it across a callback boundary. On the main thread the sink draws to
// a 2D canvas; in the worker (R8 S6) it draws to a transferred OffscreenCanvas
// so decoded frames are painted *in the worker* and never postMessage-d back.
//
// R10 (docs/14): the worker composes sinks via createRenderSink() — a WebGL
// textured-quad sink (Firefox's 2D drawImage(VideoFrame) does a synchronous
// CPU conversion per frame; the 2D sink survives as fallback) wrapped in a
// scheduling sink that draws at most once per rAF tick, latest-frame-wins.
// R12 (docs/17): that scheduling sink is PacedPresentationSink — with no
// display target it IS the old R10 coalescer; with targets (the opt-in
// adaptive playout mode) it holds ≤3 decoded frames and presents each in its
// vsync slot.
//
// Contract: draw() takes ownership of the frame and MUST close() it (single
// owner — the pipeline never touches the frame again).

import { log } from '../lib/logger';
import { getInterpolationEnabled, midSlotMs } from './interpolation';

// Which context ultimately paints — surfaced in ViewerStats.renderer so
// "is your Firefox actually on the WebGL sink?" is answerable remotely.
export type RenderSinkKind = '2d' | 'webgl';

export interface RenderSink {
  readonly kind: RenderSinkKind;
  // Whether the scheduling sink paces on worker rAF or the timer fallback —
  // surfaced so degraded pacing accuracy is visible (R12 T2).
  readonly scheduleKind?: RenderScheduleKind;
  // R12 T2: an optional target display time (same clock as the sink's `now`,
  // i.e. this context's performance.now()). Absent ⇒ present ASAP (the
  // latest-frame-wins default). Ownership transfers either way.
  draw(frame: VideoFrame, targetDisplayMs?: number): void;
  // Cumulative frames drawn (R9 M6): feeds the viewer funnel's renderedFps.
  drawnFrames(): number;
  // R12 T1: presentation-cadence jitter since the last drain; implemented by
  // the scheduling sink (the paint is the phenomenon under measurement), null
  // elsewhere and before two draws.
  drainCadence?(): RenderCadence | null;
  // R12 T2: close everything held without presenting (broadcaster restart,
  // resync, stop); presentNewest paints the newest held frame first (mode
  // toggled off — don't let it wait out a schedule that no longer applies).
  flush?(presentNewest?: boolean): void;
  // R12 T4: whether this sink can synthesize interpolated frames (the
  // WebGL2 two-texture path) — gates the experimental toggle's visibility.
  readonly supportsInterpolation?: boolean;
}

// R12 T4: what the paced sink needs from an interpolation-capable inner sink.
interface InterpolatingInner {
  upload(frame: VideoFrame): void;
  present(alpha: number): void;
}

function asInterpolating(sink: RenderSink): (RenderSink & InterpolatingInner) | null {
  const s = sink as RenderSink & Partial<InterpolatingInner>;
  return typeof s.upload === 'function' && typeof s.present === 'function'
    ? (s as RenderSink & InterpolatingInner)
    : null;
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

// R12 T4 (docs/17 Decision 7): the interpolating WebGL sink — two ping-pong
// textures (previous + current frame) and a linear-blend fragment shader.
// upload() is decoupled from present(): the paced sink uploads the NEXT real
// frame early to synthesize a mid frame (present(0.5) = blend of the frame
// on screen and the one in hand), then presents the real frame from the
// already-uploaded texture (present(1)). Plain draw() = upload + present(1),
// so this sink is a drop-in WebGLRenderSink when interpolation is off.
// WebGL2-only by policy (createContextSink); T5 swaps the blend for
// motion-estimated warping behind the same two methods.
const BLEND_FRAGMENT_SHADER = `
precision mediump float;
varying vec2 uv;
uniform sampler2D prevTex;
uniform sampler2D currTex;
uniform float alphaMix;
void main() {
  gl_FragColor = mix(texture2D(prevTex, uv), texture2D(currTex, uv), alphaMix);
}`;

export class InterpolatingWebGLRenderSink implements RenderSink {
  readonly kind: RenderSinkKind = 'webgl';
  private canvas: OffscreenCanvas;
  private gl: GL;
  private drawn = 0;
  private currUnit = 0; // texture unit holding the newest uploaded frame
  private uploads = 0;
  private prevValid = false; // false until two same-sized frames are in
  private prevTexLoc: WebGLUniformLocation | null;
  private currTexLoc: WebGLUniformLocation | null;
  private alphaLoc: WebGLUniformLocation | null;

  constructor(canvas: OffscreenCanvas, gl: GL) {
    this.canvas = canvas;
    this.gl = gl;

    const program = gl.createProgram();
    if (!program) throw new Error('WebGL createProgram failed');
    gl.attachShader(program, compileShader(gl, gl.VERTEX_SHADER, VERTEX_SHADER));
    gl.attachShader(program, compileShader(gl, gl.FRAGMENT_SHADER, BLEND_FRAGMENT_SHADER));
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

    for (const unit of [0, 1]) {
      const texture = gl.createTexture();
      if (!texture) throw new Error('WebGL createTexture failed');
      gl.activeTexture(gl.TEXTURE0 + unit);
      gl.bindTexture(gl.TEXTURE_2D, texture);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
    }
    this.prevTexLoc = gl.getUniformLocation(program, 'prevTex');
    this.currTexLoc = gl.getUniformLocation(program, 'currTex');
    this.alphaLoc = gl.getUniformLocation(program, 'alphaMix');
  }

  // Takes ownership of the frame: uploads it into the "current" texture
  // (the old current becomes "previous") and closes it. Does not paint.
  upload(frame: VideoFrame): void {
    const gl = this.gl;
    try {
      if (gl.isContextLost()) return;
      const canvas = this.canvas;
      if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
        canvas.width = frame.displayWidth;
        canvas.height = frame.displayHeight;
        gl.viewport(0, 0, canvas.width, canvas.height);
        // A resolution change makes the previous texture unblendable.
        this.uploads = 0;
      }
      this.currUnit = 1 - this.currUnit;
      gl.activeTexture(gl.TEXTURE0 + this.currUnit);
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, frame);
      this.uploads++;
      this.prevValid = this.uploads >= 2;
    } finally {
      frame.close();
    }
  }

  // Paints mix(previous, current, alpha): 1 = the current (real) frame,
  // 0.5 = the synthesized mid frame. Falls back to the current frame while
  // no valid previous texture exists.
  present(alpha: number): void {
    const gl = this.gl;
    if (gl.isContextLost()) return;
    gl.uniform1i(this.currTexLoc, this.currUnit);
    gl.uniform1i(this.prevTexLoc, this.prevValid ? 1 - this.currUnit : this.currUnit);
    gl.uniform1f(this.alphaLoc, this.prevValid ? alpha : 1);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    this.drawn++;
  }

  draw(frame: VideoFrame): void {
    this.upload(frame);
    this.present(1);
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
export type RenderScheduleKind = 'raf' | 'timer';

// Worker rAF exists in Chrome and Firefox ≥ 105 (it shipped alongside
// OffscreenCanvas); the timer fallback keeps coalescing — the point — even
// where it doesn't (pacing accuracy degrades to ~today's and the overlay
// says so via scheduleKind).
const defaultScheduleKind: RenderScheduleKind =
  typeof requestAnimationFrame === 'function' ? 'raf' : 'timer';
const defaultSchedule: RenderSchedule =
  defaultScheduleKind === 'raf'
    ? (cb) => requestAnimationFrame(cb)
    : (cb) => void setTimeout(cb, 16);

// The display refresh interval, estimated as a windowed median of scheduler
// tick deltas (R12 T2) — sets the ±½-vsync lookahead for slot matching.
// Median, not mean: one long tick (GC, tab switch) must not stretch it.
const DEFAULT_DISPLAY_INTERVAL_MS = 1000 / 60;
const DISPLAY_INTERVAL_SAMPLES = 32;
const DISPLAY_INTERVAL_MAX_MS = 250; // longer deltas are pauses, not refreshes

export class DisplayIntervalEstimator {
  private lastTickMs: number | null = null;
  private deltas: number[] = [];

  recordTick(nowMs: number): void {
    if (this.lastTickMs !== null) {
      const d = nowMs - this.lastTickMs;
      if (d > 0 && d < DISPLAY_INTERVAL_MAX_MS) {
        this.deltas.push(d);
        if (this.deltas.length > DISPLAY_INTERVAL_SAMPLES) this.deltas.shift();
      }
    }
    this.lastTickMs = nowMs;
  }

  intervalMs(): number {
    if (this.deltas.length < 4) return DEFAULT_DISPLAY_INTERVAL_MS;
    const sorted = [...this.deltas].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  }
}

interface HeldFrame {
  frame: VideoFrame;
  targetDisplayMs: number;
}

// How many decoded frames pacing may hold. VRAM is trivial at 3; the real
// bound is the decoder's frame pool, which the reorder buffer's decode lead
// keeps at a steady-state hold of 1–2 (docs/17 Decision 3). Overflow closes
// the oldest — latest-frame-wins under load is structural, not a mode.
export const MAX_HELD_FRAMES = 3;

// The one scheduling sink (R12 T2, subsuming R10 P1's CoalescingRenderSink):
// at most one inner draw per scheduler tick, latest-frame-wins. A frame drawn
// without a target presents at the next tick exactly as the coalescing sink
// did; a frame with a targetDisplayMs is held (bounded) and presented in the
// tick whose time best matches its slot — the newest due frame wins and every
// older held frame closes unseen. Pacing holds only frames already in hand;
// it never waits for missing ones.
export class PacedPresentationSink implements RenderSink {
  readonly kind: RenderSinkKind;
  readonly scheduleKind: RenderScheduleKind;
  private inner: RenderSink;
  private schedule: RenderSchedule;
  private now: () => number;
  // R12 T1: cadence is recorded at the inner draw — the actual paint.
  private cadence = new CadenceRecorder();
  private display = new DisplayIntervalEstimator();
  private held: HeldFrame[] = [];
  private immediate: VideoFrame | null = null;
  private tickScheduled = false;
  private dropped = 0;
  // R12 T4: the interpolation-capable view of the inner sink (null = plain).
  private interpolating: (RenderSink & InterpolatingInner) | null;
  // The last REAL frame's display slot — the left edge of a mid slot.
  private lastPresentedTarget: number | null = null;
  // A real frame uploaded early for a mid blend; its own slot still pends.
  private uploadedNext: { targetDisplayMs: number; timestampUs: number } | null = null;

  constructor(
    inner: RenderSink,
    schedule: RenderSchedule = defaultSchedule,
    now: () => number = () => performance.now(),
    scheduleKind: RenderScheduleKind = defaultScheduleKind,
  ) {
    this.inner = inner;
    this.kind = inner.kind;
    this.schedule = schedule;
    this.now = now;
    this.scheduleKind = scheduleKind;
    this.interpolating = asInterpolating(inner);
  }

  get supportsInterpolation(): boolean {
    return this.interpolating !== null;
  }

  draw(frame: VideoFrame, targetDisplayMs?: number): void {
    if (targetDisplayMs === undefined) {
      this.immediate?.close();
      this.immediate = frame;
    } else {
      this.held.push({ frame, targetDisplayMs });
      while (this.held.length > MAX_HELD_FRAMES) {
        this.held.shift()!.frame.close();
        this.dropped++;
      }
    }
    this.ensureTick();
  }

  // Frames closed unseen by pacing (superseded in-slot, overflow, flush).
  pacingDropped(): number {
    return this.dropped;
  }

  flush(presentNewest = false): void {
    this.uploadedNext = null;
    this.lastPresentedTarget = null;
    // The newest frame overall: a pending immediate frame arrived after any
    // held paced one (mixing only happens across a mode switch).
    let newest = this.immediate;
    this.immediate = null;
    if (!newest && this.held.length > 0) newest = this.held.pop()!.frame;
    for (const h of this.held) {
      h.frame.close();
      this.dropped++;
    }
    this.held = [];
    if (!newest) return;
    if (presentNewest) {
      this.paint(newest);
    } else {
      newest.close();
      this.dropped++;
    }
  }

  drawnFrames(): number {
    return this.inner.drawnFrames();
  }

  drainCadence(): RenderCadence | null {
    return this.cadence.drain();
  }

  private ensureTick(): void {
    if (this.tickScheduled) return;
    this.tickScheduled = true;
    this.schedule(() => this.tick());
  }

  private tick(): void {
    this.tickScheduled = false;
    const now = this.now();
    this.display.recordTick(now);
    const dueBy = now + this.display.intervalMs() / 2;

    if (this.immediate) {
      // An ASAP frame paints this tick; held paced frames keep their slots
      // and are handled next tick (16 ms — mixing is a mode-switch transient).
      const frame = this.immediate;
      this.immediate = null;
      this.uploadedNext = null; // superseded by the newer ASAP frame
      this.lastPresentedTarget = null; // no slot to interpolate from
      this.paint(frame);
    } else {
      // Held frames are in decode order with monotonic targets: the due ones
      // are a prefix. The newest due frame wins; older due frames missed
      // their slot to it and close unseen.
      let dueCount = 0;
      while (dueCount < this.held.length && this.held[dueCount].targetDisplayMs <= dueBy) {
        dueCount++;
      }
      if (dueCount > 0) {
        const due = this.held.splice(0, dueCount);
        const winner = due.pop()!;
        for (const h of due) {
          h.frame.close();
          this.dropped++;
        }
        this.uploadedNext = null; // a newer real frame supersedes its slot
        this.presentReal(winner.frame, winner.targetDisplayMs);
      } else if (this.uploadedNext && this.uploadedNext.targetDisplayMs <= dueBy) {
        // The early-uploaded frame's own slot: present it from its texture.
        const { targetDisplayMs, timestampUs } = this.uploadedNext;
        this.uploadedNext = null;
        if (this.interpolating) {
          this.cadence.record(timestampUs, this.now());
          this.interpolating.present(1);
          this.lastPresentedTarget = targetDisplayMs;
        }
      } else if (this.held.length > 0 && this.uploadedNext === null) {
        // R12 T4: mid slot — synthesize a frame halfway between the one on
        // screen and the next one already in hand (opportunistic: a missing
        // next frame simply means no interpolation this interval).
        const interp = getInterpolationEnabled() ? this.interpolating : null;
        const next = this.held[0];
        const mid = interp ? midSlotMs(this.lastPresentedTarget, next.targetDisplayMs) : null;
        if (interp && mid !== null && mid <= dueBy) {
          this.held.shift();
          this.uploadedNext = {
            targetDisplayMs: next.targetDisplayMs,
            timestampUs: next.frame.timestamp,
          };
          interp.upload(next.frame); // takes ownership, closes the frame
          interp.present(0.5);
          // Synthesized presents don't move lastPresentedTarget — real
          // slots anchor the mid computation and the cadence metric.
        }
      }
    }

    if (this.held.length > 0 || this.immediate || this.uploadedNext) this.ensureTick();
  }

  // A real frame's slot: through the interpolation path when it is both
  // available and enabled (so the previous-frame texture stays warm), the
  // plain inner draw otherwise.
  private presentReal(frame: VideoFrame, targetDisplayMs: number): void {
    this.cadence.record(frame.timestamp, this.now());
    const interp = getInterpolationEnabled() ? this.interpolating : null;
    if (interp) {
      interp.upload(frame);
      interp.present(1);
    } else {
      this.inner.draw(frame);
    }
    this.lastPresentedTarget = targetDisplayMs;
  }

  private paint(frame: VideoFrame): void {
    this.cadence.record(frame.timestamp, this.now());
    this.inner.draw(frame);
  }
}

// The worker's sink: WebGL2 → WebGL → 2D, wrapped in the paced presentation
// sink (which is a pure coalescer until the pipeline passes display targets).
// Context flavors are tried in order; a canvas whose getContext() returned
// null is not bound to that flavor, so falling through is safe. (If WebGL
// context creation *succeeds* but program setup then throws, the canvas is
// stuck in WebGL mode and the 2D fallback will be blind — vanishingly rare,
// hence only logged.)
export function createRenderSink(
  canvas: OffscreenCanvas,
  schedule: RenderSchedule = defaultSchedule,
  scheduleKind: RenderScheduleKind = defaultScheduleKind,
): RenderSink {
  return new PacedPresentationSink(createContextSink(canvas), schedule, undefined, scheduleKind);
}

// Exported for R16 (docs/21): the viewer worker composes
// PacedPresentationSink(TeeRenderSink(createContextSink(canvas))) on gated
// devices; createRenderSink() stays the everything-else path.
export function createContextSink(canvas: OffscreenCanvas): RenderSink {
  const opts = { alpha: false, antialias: false, depth: false, stencil: false };
  // WebGL2 gets the interpolation-capable sink (R12 T4 — WebGL2-only by
  // policy); its plain draw() path is identical to WebGLRenderSink, so this
  // costs nothing when the experimental toggle is off.
  const gl2 = canvas.getContext('webgl2', opts) as GL | null;
  if (gl2) {
    try {
      return new InterpolatingWebGLRenderSink(canvas, gl2);
    } catch (e) {
      log.warn('WebGL2 interpolating sink init failed; trying plain WebGL:', e);
      try {
        return new WebGLRenderSink(canvas, gl2);
      } catch (e2) {
        log.warn('WebGL render sink init failed; falling back to 2D:', e2);
      }
    }
  } else {
    const gl = canvas.getContext('webgl', opts) as GL | null;
    if (gl) {
      try {
        return new WebGLRenderSink(canvas, gl);
      } catch (e) {
        log.warn('WebGL render sink init failed; falling back to 2D:', e);
      }
    }
  }
  return new OffscreenCanvasRenderSink(canvas);
}
