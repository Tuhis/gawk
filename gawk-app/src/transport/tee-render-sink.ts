// R16 (docs/21 Decision 3/4, reworked per the U4 findings): the presentation
// tee — a RenderSink decorator around the context sink, inside the paced
// sink: PacedPresentationSink(TeeRenderSink(contextSink)). The canvas is
// painted exactly as today; the armed tee writes a *clone of each decoded
// frame it presents* (via new VideoFrame(frame, { timestamp })) to a
// VideoTrackGenerator, whose track feeds the hidden iPhone presentation
// <video>. Only presented frames cross — coalesced/superseded frames were
// never painted — so the track carries the paced real frames.
//
// U4 second on-device pass (2026-07-16): the original design wrapped the
// *canvas* in a VideoFrame after each paint, carrying interpolated mid
// frames too — but VideoFrame-from-WebGL-canvas content is **black** on iOS
// WebKit (frames flowed end-to-end, element presented them, screen stayed
// black; preserveDrawingBuffer didn't cure it). Cloning the decoded frame is
// the canonical WebCodecs→MediaStream path (no canvas readback anywhere,
// cheaper per frame); the cost is that synthesized present(0.5) blends exist
// only on the canvas and don't cross — fullscreen shows real frames at the
// paced cadence.
//
// Idle until armed: constructed only when the worker init carries
// `presentationTee: true` (gated devices), and even then pass-through-only
// (zero per-frame work beyond delegation) until arm() supplies a writer.
// Once armed it stays armed for the session (pausing would reopen the
// video's readyState question under the user's next tap).

import type { RenderSink, RenderSinkKind } from './render-sink';

// The writer side of VideoTrackGenerator.writable (or a test fake). Writing
// transfers frame ownership to the sink; on rejection we close defensively.
export interface TeeFrameWriter {
  write(frame: VideoFrame): Promise<void>;
}

export interface TeeStats {
  armed: boolean;
  teedFrames: number;
  teeErrors: number;
}

// What the paced sink probes for via its interpolation seam.
interface InterpolatingInner {
  upload(frame: VideoFrame): void;
  present(alpha: number): void;
}

export class TeeRenderSink implements RenderSink {
  readonly kind: RenderSinkKind;
  // Present only when the inner sink interpolates — assigned conditionally so
  // PacedPresentationSink's upload/present probe sees exactly the inner
  // sink's capability through the tee.
  upload?: (frame: VideoFrame) => void;
  present?: (alpha: number) => void;

  private inner: RenderSink;
  private writer: TeeFrameWriter | null = null;
  private teed = 0;
  private errors = 0;
  // A clone taken at upload(), written when its frame's own slot presents
  // (present(1)); a superseding upload closes it unseen — mirrors the paced
  // sink's supersession, where an uploaded frame's slot can be overtaken by
  // a newer real frame before it ever presents.
  private heldClone: VideoFrame | null = null;
  // U4 black-screen finding: the generator stream is stamped from this
  // clock — zero-based at the first clone, strictly monotonic — never from
  // source-frame timestamps, which sit on the *broadcaster's* clock (huge
  // foreign values, backwards jumps on restarts). WebKit schedules a locally
  // generated track's samples by PTS; foreign PTS ⇒ frames that never
  // present ⇒ a black native-fullscreen player.
  private now: () => number;
  private baseMs: number | null = null;
  private lastTsUs = -1;

  constructor(inner: RenderSink, now: () => number = () => performance.now()) {
    this.inner = inner;
    this.kind = inner.kind;
    this.now = now;

    const interp = inner as RenderSink & Partial<InterpolatingInner>;
    if (typeof interp.upload === 'function' && typeof interp.present === 'function') {
      this.upload = (frame: VideoFrame) => {
        // Clone before delegating — the inner sink closes the frame.
        if (this.writer) {
          this.heldClone?.close();
          this.heldClone = this.cloneForTee(frame);
        }
        interp.upload!(frame);
      };
      this.present = (alpha: number) => {
        interp.present!(alpha);
        // A real frame's own slot: its held clone crosses now. Synthesized
        // mid blends (alpha < 1) exist only on the canvas — nothing to tee
        // without a canvas readback, which is exactly what U4 ruled out.
        if (alpha >= 1) {
          const clone = this.heldClone;
          this.heldClone = null;
          if (clone) this.writeTeed(clone);
        }
      };
    }
  }

  draw(frame: VideoFrame, targetDisplayMs?: number): void {
    // draw() ≡ upload + present(1): clone before the inner sink closes the
    // frame, write after the paint so presented order is preserved.
    const clone = this.writer ? this.cloneForTee(frame) : null;
    this.inner.draw(frame, targetDisplayMs);
    if (clone) this.writeTeed(clone);
  }

  drawnFrames(): number {
    return this.inner.drawnFrames();
  }

  arm(writer: TeeFrameWriter): void {
    this.writer = writer;
  }

  teeStats(): TeeStats {
    return { armed: this.writer !== null, teedFrames: this.teed, teeErrors: this.errors };
  }

  // Zero-based local capture time in µs; the +1 tie-break keeps stamps
  // strictly increasing even when two clones land in the same clock tick.
  private nextTimestampUs(): number {
    const nowMs = this.now();
    this.baseMs ??= nowMs;
    const ts = Math.max(Math.round((nowMs - this.baseMs) * 1000), this.lastTsUs + 1);
    this.lastTsUs = ts;
    return ts;
  }

  // Clone a decoded frame for the generator (shares the media resource — no
  // pixel copy). Failures count teeErrors and never throw into the paint
  // path — the inline canvas must keep working even if the tee dies.
  private cloneForTee(frame: VideoFrame): VideoFrame | null {
    try {
      return new VideoFrame(frame as unknown as CanvasImageSource, {
        timestamp: this.nextTimestampUs(),
      });
    } catch {
      this.errors++;
      return null;
    }
  }

  private writeTeed(clone: VideoFrame): void {
    const writer = this.writer;
    if (!writer) {
      clone.close();
      return;
    }
    try {
      writer.write(clone).catch(() => {
        this.errors++;
        try {
          clone.close();
        } catch {
          // already consumed by the sink
        }
      });
      this.teed++;
    } catch {
      this.errors++;
      try {
        clone.close();
      } catch {
        // already consumed
      }
    }
  }
}

// VideoTrackGenerator is worker-only by spec (Safari 18+; Chromium ships the
// older MediaStreamTrackGenerator instead, which is exactly why the probe
// checks for this shape). Not in TS lib.dom yet — accessed via globalThis.
export interface VideoTrackGeneratorLike {
  readonly track: MediaStreamTrack;
  readonly writable: WritableStream<VideoFrame>;
}
export type VideoTrackGeneratorCtor = new () => VideoTrackGeneratorLike;

export function getVideoTrackGenerator(): VideoTrackGeneratorCtor | undefined {
  return (globalThis as { VideoTrackGenerator?: VideoTrackGeneratorCtor }).VideoTrackGenerator;
}

// R16 Decision 7 (U1, updated in U4): the worker capability probe, run when
// init carries the presentationTee flag — VideoTrackGenerator present AND a
// trial clone-with-timestamp from a VideoFrame (the reworked tee's one
// load-bearing operation; the canvas is only scaffolding to mint the base
// frame). Failure ⇒ tier 3 and no arm command is ever sent.
export function probePresentationTee(): boolean {
  try {
    if (!getVideoTrackGenerator()) return false;
    const canvas = new OffscreenCanvas(2, 2);
    // Paint something so the canvas has a real drawing buffer to wrap.
    canvas.getContext('2d')?.fillRect(0, 0, 2, 2);
    const base = new VideoFrame(canvas as unknown as CanvasImageSource, { timestamp: 0 });
    try {
      const clone = new VideoFrame(base as unknown as CanvasImageSource, { timestamp: 1 });
      clone.close();
    } finally {
      base.close();
    }
    return true;
  } catch {
    return false;
  }
}
