// R8 S6: imperative glue between React and the viewer Web Worker, kept out of
// the component so the effect code stays legible. Owns the one-shot
// OffscreenCanvas transfer and the boot handshake; the pipeline/reconnect logic
// lives in the worker's ViewerWorkerCore.
//
// Lifecycle contract: construct once per <canvas> (the transfer is one-shot and
// cannot be repeated or reversed), drive with start()/stop() across broadcasts,
// dispose() on teardown.

import type { ConnectOptions } from '../../transport/connection';
import type {
  ViewerWorkerCommand,
  ViewerWorkerEvent,
  ViewerWorkerOutbound,
} from '../../transport/viewer-worker-core';

export interface StartParams {
  serverUrl: string;
  broadcastId: string;
  connectOpts: ConnectOptions;
}

export interface WorkerViewerCallbacks {
  onEvent: (ev: ViewerWorkerEvent) => void;
  // The worker reported it lacks the codecs/transport it needs (before any
  // canvas transfer) — caller should fall back to the main-thread pipeline.
  onUnsupported: () => void;
}

export class WorkerViewerController {
  private worker: Worker;
  private canvas: HTMLCanvasElement;
  private cb: WorkerViewerCallbacks;

  private booted = false;
  private supported = false;
  private canvasTransferred = false;
  private disposed = false;
  private pendingStart: StartParams | null = null;

  constructor(canvas: HTMLCanvasElement, cb: WorkerViewerCallbacks) {
    this.canvas = canvas;
    this.cb = cb;
    this.worker = new Worker(new URL('../../transport/viewer.worker.ts', import.meta.url), {
      type: 'module',
    });
    this.worker.onmessage = (e: MessageEvent) => this.onMessage(e.data as ViewerWorkerOutbound);
  }

  private onMessage(msg: ViewerWorkerOutbound): void {
    if (msg.type === 'boot') {
      this.booted = true;
      this.supported = msg.supported;
      if (!msg.supported) {
        this.cb.onUnsupported();
        return;
      }
      if (this.pendingStart) this.flushStart();
      return;
    }
    if (this.disposed) return;
    this.cb.onEvent(msg);
  }

  private post(cmd: ViewerWorkerCommand, transfer?: Transferable[]): void {
    this.worker.postMessage(cmd, transfer ?? []);
  }

  private flushStart(): void {
    const params = this.pendingStart;
    if (!params || this.disposed) return;
    if (!this.canvasTransferred) {
      const offscreen = this.canvas.transferControlToOffscreen();
      this.canvasTransferred = true;
      this.post({ type: 'init', canvas: offscreen }, [offscreen]);
    }
    this.post({ type: 'start', ...params });
  }

  // (Re)start a session. Buffered until the boot handshake completes; a repeat
  // call (broadcast-id change) supersedes inside the worker core.
  start(params: StartParams): void {
    if (this.disposed) return;
    this.pendingStart = params;
    if (this.booted && this.supported) this.flushStart();
  }

  stop(): void {
    this.pendingStart = null;
    if (this.booted && this.supported && this.canvasTransferred) this.post({ type: 'stop' });
  }

  // R5 Q3: apply the smoothed-playout setting inside the worker context.
  // Safe at any lifecycle point — worker messages queue until the shell runs,
  // and the setting is module state there, independent of start/stop.
  setSmoothedPlayout(smoothed: boolean): void {
    if (this.disposed) return;
    this.post({ type: 'playout', smoothed });
  }

  dispose(): void {
    this.disposed = true;
    this.worker.terminate();
  }
}
