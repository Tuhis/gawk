// R8 S6: the Web Worker entry point — a thin shell around ViewerWorkerCore.
// All the pipeline/reconnect logic lives in the (DOM-free, unit-tested) core;
// this file only bridges `postMessage` to it and owns the two things that are
// genuinely worker-scoped: the capability handshake and the OffscreenCanvas.
//
// Vite bundles this via `new Worker(new URL('./viewer.worker.ts', ...))`.

import { createRenderSink } from './render-sink';
import {
  ViewerWorkerCore,
  type ViewerWorkerCommand,
  type ViewerWorkerOutbound,
} from './viewer-worker-core';

// The DedicatedWorkerGlobalScope, typed minimally so we don't have to pull the
// "WebWorker" lib in alongside "DOM" (they clash on globals like `self`).
interface WorkerScope {
  postMessage(message: ViewerWorkerOutbound, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent) => void) | null;
}
const ctx = self as unknown as WorkerScope;

// Boot handshake: report capability *before* the main thread transfers the
// canvas, so an unsupported worker can be discarded without detaching the
// canvas from the main-thread fallback path. Buffered until main attaches its
// handler.
const supported = typeof VideoDecoder !== 'undefined' && typeof WebTransport !== 'undefined';
ctx.postMessage({ type: 'boot', supported });

let core: ViewerWorkerCore | null = null;

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as ViewerWorkerCommand;
  switch (cmd.type) {
    case 'init': {
      // WebGL (2D fallback) wrapped in rAF coalescing — R10, docs/14.
      const sink = createRenderSink(cmd.canvas);
      core = new ViewerWorkerCore({ post: (ev) => ctx.postMessage(ev), renderSink: sink });
      break;
    }
    case 'start':
      core?.start(cmd);
      break;
    case 'stop':
      void core?.stop();
      break;
  }
};
