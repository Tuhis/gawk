// R8 S6: OffscreenCanvasRenderSink resizes the backing store to the frame and
// always closes the frame (single-owner contract). No real OffscreenCanvas —
// a fake canvas/context is enough to pin the behavior.

import { describe, expect, it, vi } from 'vitest';
import { OffscreenCanvasRenderSink } from './render-sink';

// Fake OffscreenCanvas whose width/height are accessors so we can count writes
// (a plain data property can't distinguish "assigned same value" from "skipped").
function fakeCanvas() {
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
    getContext: vi.fn(() => ctx),
  };
  return { canvas: canvas as unknown as OffscreenCanvas, ctx, writes: () => writes };
}

function fakeFrame(w: number, h: number) {
  return { displayWidth: w, displayHeight: h, close: vi.fn() } as unknown as VideoFrame;
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
});
