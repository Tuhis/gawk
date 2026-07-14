// R10 P3: the dedicated transport worker's entry point — a thin shell around
// TransportWorkerCore. Spawned as a *nested* worker by viewer.worker.ts (one
// per ViewerPipeline attempt, so reconnects get a fresh session and teardown
// is just worker death); the WebTransport read loops run here so no decode or
// render pressure in the viewer worker can starve the incoming-datagram queue.
//
// Vite bundles this via `new Worker(new URL('./transport.worker.ts', ...))`.

import {
  TransportWorkerCore,
  type TransportWorkerCommand,
  type TransportWorkerEvent,
} from './transport-worker-core';

// The DedicatedWorkerGlobalScope, typed minimally so we don't have to pull the
// "WebWorker" lib in alongside "DOM" (they clash on globals like `self`).
interface WorkerScope {
  postMessage(message: TransportWorkerEvent, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent) => void) | null;
  close(): void;
}
const ctx = self as unknown as WorkerScope;

const core = new TransportWorkerCore({ post: (ev, transfer) => ctx.postMessage(ev, transfer) });

ctx.onmessage = (e: MessageEvent) => {
  const cmd = e.data as TransportWorkerCommand;
  switch (cmd.type) {
    case 'connect':
      core.connect(cmd.url, cmd.connectOpts);
      break;
    case 'close':
      core.close();
      // Give the graceful wt.close() a beat to flush its close frame, then
      // die (the proxy terminate()s shortly after as a backstop).
      setTimeout(() => ctx.close(), 100);
      break;
  }
};
