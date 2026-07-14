// R8 S6: the Web Worker entry point — a thin shell around ViewerWorkerCore.
// All the pipeline/reconnect logic lives in the (DOM-free, unit-tested) core;
// this file only bridges `postMessage` to it and owns the two things that are
// genuinely worker-scoped: the capability handshake and the OffscreenCanvas.
//
// Vite bundles this via `new Worker(new URL('./viewer.worker.ts', ...))`.

import { setSmoothedPlayout } from './playout';
import { createRenderSink } from './render-sink';
import {
  ViewerWorkerCore,
  type ViewerWorkerCommand,
  type ViewerWorkerOutbound,
} from './viewer-worker-core';
import type { ViewerTransportFactory } from './viewer-transport';
import { WorkerViewerTransport } from './worker-viewer-transport';

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

// R10 P3: run the WebTransport read loops in a *nested* transport worker so
// decode/render work here can never starve the incoming-datagram queue. One
// nested worker per pipeline attempt (spawned on connect, reaped on close).
// Where nested workers don't exist, the pipeline keeps its in-process
// transport — same behavior as before the split.
const transportFactory: ViewerTransportFactory | undefined =
  typeof Worker === 'function'
    ? (url, opts) =>
        new WorkerViewerTransport(
          () => new Worker(new URL('./transport.worker.ts', import.meta.url), { type: 'module' }),
          url,
          opts,
        )
    : undefined;

let core: ViewerWorkerCore | null = null;

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as ViewerWorkerCommand;
  switch (cmd.type) {
    case 'init': {
      // WebGL (2D fallback) wrapped in rAF coalescing — R10, docs/14.
      const sink = createRenderSink(cmd.canvas);
      core = new ViewerWorkerCore({
        post: (ev) => ctx.postMessage(ev),
        renderSink: sink,
        transportFactory,
      });
      break;
    }
    case 'start':
      core?.start(cmd);
      break;
    case 'stop':
      void core?.stop();
      break;
    case 'playout':
      // Worker-context module state; the live pipeline's reorder buffer reads
      // it on every advance (R5 Q3). Valid before/after init and start alike.
      setSmoothedPlayout(cmd.smoothed);
      break;
  }
};
