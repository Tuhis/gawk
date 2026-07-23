// R8 S6: imperative glue between React and the viewer Web Worker, kept out of
// the component so the effect code stays legible. Owns the one-shot
// OffscreenCanvas transfer and the boot handshake; the pipeline/reconnect logic
// lives in the worker's ViewerWorkerCore.
//
// Lifecycle contract: construct once per <canvas> (the transfer is one-shot and
// cannot be repeated or reversed), drive with start()/stop() across broadcasts,
// dispose() on teardown.

import type { ViewerDeliveryMode } from '../../transport/resilient';
import type { ConnectOptions } from '../../transport/connection';
import type { PlayoutMode } from '../../transport/playout';
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

export interface WorkerViewerOptions {
  // R16 (docs/21): request the presentation tee at init. Set only on gated
  // (element-fullscreen-less) devices — when false, the init message is
  // byte-identical to before R16 and no tee/probe code runs in the worker.
  presentationTee?: boolean;
}

export class WorkerViewerController {
  private worker: Worker;
  private canvas: HTMLCanvasElement;
  private cb: WorkerViewerCallbacks;
  private presentationTee: boolean;

  private booted = false;
  private supported = false;
  private canvasTransferred = false;
  private disposed = false;
  private pendingStart: StartParams | null = null;
  private armRequested = false;
  private armSent = false;

  constructor(canvas: HTMLCanvasElement, cb: WorkerViewerCallbacks, opts: WorkerViewerOptions = {}) {
    this.canvas = canvas;
    this.cb = cb;
    this.presentationTee = opts.presentationTee ?? false;
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
      // The tee flag is spread in only when set, keeping non-gated init
      // messages byte-identical to pre-R16 (docs/21 Decision 1).
      this.post(
        { type: 'init', canvas: offscreen, ...(this.presentationTee ? { presentationTee: true } : {}) },
        [offscreen],
      );
    }
    this.post({ type: 'start', ...params });
    if (this.armRequested && !this.armSent) {
      this.post({ type: 'arm' });
      this.armSent = true;
    }
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

  // R5 Q3 + R12 T2: apply the playout mode inside the worker context.
  // Safe at any lifecycle point — worker messages queue until the shell runs,
  // and the setting is module state there, independent of start/stop.
  setPlayoutMode(mode: PlayoutMode): void {
    if (this.disposed) return;
    this.post({ type: 'playout', mode });
  }

  // R12 T4: the experimental interpolation toggle, same crossing semantics.
  setInterpolation(enabled: boolean): void {
    if (this.disposed) return;
    this.post({ type: 'interpolation', enabled });
  }

  // R15 N5: the audio sink's ~4 Hz playhead report (docs/20 Decision 10).
  // Fire-and-forget: a dropped report just means the worker keeps the
  // previous mapping, and a stale one falls back to the arrival baseline.
  sendAudioPlayhead(playheadUs: number | null, atEpochMs: number): void {
    if (this.disposed) return;
    this.post({ type: 'audioPlayhead', playheadUs, atEpochMs });
  }

  // R19: resilient mode for the worker context. Callers send it before
  // start() (worker messages process in order), so the wider profile is live
  // before the session's first frame.
  setViewerDeliveryMode(mode: ViewerDeliveryMode): void {
    if (this.disposed) return;
    this.post({ type: 'resilient', mode });
  }

  // R16: activate the presentation tee (gated devices, at `watching`). Sent
  // at most once — the worker's generator/track are session-long and survive
  // reconnects (docs/21 Decision 4). Buffered until the canvas/init exist.
  armPresentation(): void {
    if (this.disposed || this.armRequested) return;
    this.armRequested = true;
    if (this.canvasTransferred && !this.armSent) {
      this.post({ type: 'arm' });
      this.armSent = true;
    }
  }

  dispose(): void {
    this.disposed = true;
    this.worker.terminate();
  }
}
