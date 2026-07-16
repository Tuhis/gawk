// R16 (docs/21 Decision 3/4): the presentation tee — a RenderSink decorator
// around the context sink, inside the paced sink:
// PacedPresentationSink(TeeRenderSink(contextSink)). The canvas is painted
// exactly as today; after every paint that reaches the screen (plain draw(),
// present(1), AND present(0.5) synthesized mid-frames) the armed tee wraps
// the canvas in a VideoFrame and writes it to a VideoTrackGenerator, whose
// track feeds the hidden iPhone presentation <video>. Only *presented* frames
// cross — coalesced/superseded frames were never painted — so the track
// carries the exact smoothed output, interpolated frames included.
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
  private canvas: OffscreenCanvas;
  private writer: TeeFrameWriter | null = null;
  private teed = 0;
  private errors = 0;
  // The last presented REAL frame's timestamp and the last uploaded frame's —
  // a synthesized present(0.5) sits between them (the paced sink uploads the
  // next real frame just before presenting the mid blend).
  private prevRealTsUs: number | null = null;
  private uploadedTsUs: number | null = null;

  constructor(inner: RenderSink, canvas: OffscreenCanvas) {
    this.inner = inner;
    this.canvas = canvas;
    this.kind = inner.kind;

    const interp = inner as RenderSink & Partial<InterpolatingInner>;
    if (typeof interp.upload === 'function' && typeof interp.present === 'function') {
      this.upload = (frame: VideoFrame) => {
        // Record before delegating — the inner sink closes the frame.
        this.uploadedTsUs = frame.timestamp;
        interp.upload!(frame);
      };
      this.present = (alpha: number) => {
        interp.present!(alpha);
        if (alpha >= 1) {
          // A real frame's own slot, presented from its uploaded texture.
          const ts = this.uploadedTsUs;
          if (ts !== null) {
            this.prevRealTsUs = ts;
            this.capture(ts);
          }
        } else {
          // Synthesized mid frame: midpoint of the two real timestamps around
          // it. The element renders live srcObject frames on arrival, so this
          // only needs to be sane and monotonic, not load-bearing.
          const ts = this.midTimestampUs();
          if (ts !== null) this.capture(ts);
        }
      };
    }
  }

  draw(frame: VideoFrame, targetDisplayMs?: number): void {
    const ts = frame.timestamp;
    this.inner.draw(frame, targetDisplayMs);
    // draw() ≡ upload + present(1): this frame is now both the newest real
    // frame on screen and the newest uploaded one.
    this.prevRealTsUs = ts;
    this.uploadedTsUs = ts;
    this.capture(ts);
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

  private midTimestampUs(): number | null {
    if (this.uploadedTsUs === null) return null;
    if (this.prevRealTsUs === null) return this.uploadedTsUs;
    return Math.round((this.prevRealTsUs + this.uploadedTsUs) / 2);
  }

  // Capture the just-painted canvas into the generator. Failures count
  // teeErrors and never throw into the paint path — the inline canvas must
  // keep working even if the tee dies.
  private capture(timestampUs: number): void {
    const writer = this.writer;
    if (!writer) return;
    let frame: VideoFrame | null = null;
    try {
      frame = new VideoFrame(this.canvas as unknown as CanvasImageSource, {
        timestamp: timestampUs,
      });
      const f = frame;
      writer.write(f).catch(() => {
        this.errors++;
        try {
          f.close();
        } catch {
          // already consumed by the sink
        }
      });
      this.teed++;
    } catch {
      this.errors++;
      try {
        frame?.close();
      } catch {
        // never constructed / already closed
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

// R16 Decision 7 (U1): the worker capability probe, run when init carries the
// presentationTee flag — VideoTrackGenerator present AND a trial
// VideoFrame-from-OffscreenCanvas (the tee's one unverified load-bearing
// operation on iOS WebKit). Failure ⇒ tier 3 and no arm command is ever sent.
export function probePresentationTee(): boolean {
  try {
    if (!getVideoTrackGenerator()) return false;
    const canvas = new OffscreenCanvas(2, 2);
    // Paint something so the canvas has a real drawing buffer to wrap.
    canvas.getContext('2d')?.fillRect(0, 0, 2, 2);
    const frame = new VideoFrame(canvas as unknown as CanvasImageSource, { timestamp: 0 });
    frame.close();
    return true;
  } catch {
    return false;
  }
}
