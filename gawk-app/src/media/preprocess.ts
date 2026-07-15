// Pre-encode frame preprocessing for the R3 ladder: framerate gating
// (timestamp-based dropping) and resolution scaling (OffscreenCanvas).
// Runs between capture and encoder; capture always stays at native.
// See docs/08-resolution-framerate-picker.md.

import { computeTargetSize, type FramerateRung, type ResolutionRung } from './ladder';

// Timestamp-based frame gate. Maintains a virtual schedule (nextDueUs)
// advanced by the target interval; frames arriving before their slot are
// dropped. A gap larger than one interval (capture stall, target change)
// re-anchors the schedule — we never burst to "catch up". Pure logic, no
// WebCodecs/DOM, unit-tested in preprocess.test.ts.
export class FpsGate {
  private intervalUs: number | null = null;
  private nextDueUs: number | null = null;
  private dropped = 0;

  setTargetFps(fps: number | null): void {
    const intervalUs = fps === null ? null : Math.round(1_000_000 / fps);
    if (intervalUs !== this.intervalUs) {
      this.intervalUs = intervalUs;
      this.nextDueUs = null; // re-anchor on the next frame
    }
  }

  get droppedCount(): number {
    return this.dropped;
  }

  accept(timestampUs: number): boolean {
    if (this.intervalUs === null) return true;
    if (this.nextDueUs === null || timestampUs - this.nextDueUs > this.intervalUs) {
      this.nextDueUs = timestampUs + this.intervalUs;
      return true;
    }
    if (timestampUs < this.nextDueUs) {
      this.dropped++;
      return false;
    }
    this.nextDueUs += this.intervalUs;
    return true;
  }
}

export interface PreprocessStats {
  gateDropped: number;
  scaledFrames: number;
}

// Gate + scale stage. process() consumes the input frame (closes it) unless
// it is returned unchanged (passthrough — the common native/native case must
// stay zero-copy). drawImage of a VideoFrame is GPU-backed in Chromium; the
// canvas and context are reused across frames.
export class FramePreprocessor {
  private gate = new FpsGate();
  private resolutionRung: ResolutionRung = 'native';
  private framerateRung: FramerateRung = 'native';
  private canvas: OffscreenCanvas | null = null;
  private ctx: OffscreenCanvasRenderingContext2D | null = null;
  private scaledFrames = 0;

  setTarget(resolution: ResolutionRung, framerate: FramerateRung): void {
    this.resolutionRung = resolution;
    this.framerateRung = framerate;
  }

  getStats(): PreprocessStats {
    return { gateDropped: this.gate.droppedCount, scaledFrames: this.scaledFrames };
  }

  // Returns the frame to encode (the input itself, or a scaled replacement
  // with the same timestamp), or null when the fps gate drops it. The input
  // frame is closed unless returned as-is.
  process(frame: VideoFrame, nativeFps: number | null): VideoFrame | null {
    let targetFps: number | null = null;
    if (this.framerateRung !== 'native') {
      targetFps = this.framerateRung;
    } else if (nativeFps !== null) {
      targetFps = nativeFps;
    }

    this.gate.setTargetFps(targetFps);

    if (!this.gate.accept(frame.timestamp)) {
      frame.close();
      return null;
    }
    const target = computeTargetSize(frame.displayWidth, frame.displayHeight, this.resolutionRung);
    if (target === null) return frame;

    if (!this.canvas || this.canvas.width !== target.width || this.canvas.height !== target.height) {
      this.canvas = new OffscreenCanvas(target.width, target.height);
      this.ctx = this.canvas.getContext('2d', { alpha: false });
      if (!this.ctx) {
        // No 2D context (should not happen in practice): send the frame
        // unscaled rather than killing the broadcast.
        this.canvas = null;
        return frame;
      }
    }
    this.ctx!.drawImage(frame, 0, 0, target.width, target.height);
    const scaled = new VideoFrame(this.canvas, {
      timestamp: frame.timestamp,
      alpha: 'discard',
    });
    frame.close();
    this.scaledFrames++;
    return scaled;
  }
}
