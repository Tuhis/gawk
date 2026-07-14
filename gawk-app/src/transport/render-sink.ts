// The viewer pipeline hands each decoded VideoFrame to a RenderSink instead of
// bouncing it across a callback boundary. On the main thread the sink draws to
// a 2D canvas; in the worker (R8 S6) it draws to a transferred OffscreenCanvas
// so decoded frames are painted *in the worker* and never postMessage-d back.
//
// Contract: draw() takes ownership of the frame and MUST close() it (single
// owner — the pipeline never touches the frame again).

export interface RenderSink {
  draw(frame: VideoFrame): void;
}

// Draws to an OffscreenCanvas transferred once from the main thread. The
// backing-store size tracks the frame's display size; the on-screen <canvas>
// element's CSS governs letterboxing, exactly as the main-thread path did.
export class OffscreenCanvasRenderSink implements RenderSink {
  private canvas: OffscreenCanvas;
  private ctx: OffscreenCanvasRenderingContext2D | null;

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
    }
    frame.close();
  }
}
