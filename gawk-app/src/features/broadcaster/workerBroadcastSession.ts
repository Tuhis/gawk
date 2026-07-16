// R11 (docs/16): imperative glue between the broadcaster UI and the broadcast
// Web Worker, kept out of the component so the screen stays path-agnostic.
// WorkerBroadcastSession presents the exact BroadcastPipeline surface
// (start/stop/setLadder + BroadcastCallbacks) while running the pipeline in a
// worker; createBroadcastSession() picks worker vs. main-thread per
// capability, so BroadcasterScreen's reclaim/mint/error logic never changes.
//
// Division of labor: the worker owns connect → encode → send (and tears its
// own session down on failure — no zombie publisher). This side owns what is
// genuinely main-thread-scoped: getDisplayMedia (window + user gesture), the
// preview stream, and transferring a track *clone* into the worker (transfer
// detaches a track from this realm — the original must stay for the preview).

import { acquireDisplayStream } from '../../media/capture';
import type { CaptureConfig } from '../../media/types';
import type { FramerateSelection, ResolutionSelection } from '../../media/ladder';
import {
  BroadcastPipeline,
  BroadcastStartError,
  DEFAULT_ENCODER_SETTINGS,
  type BroadcastCallbacks,
  type BroadcastSessionLike,
  type EncoderSettings,
} from '../../transport/broadcaster';
import type {
  BroadcastWorkerCommand,
  BroadcastWorkerOutbound,
} from '../../transport/broadcast-worker-core';
import type { ConnectOptions } from '../../transport/connection';
import { log } from '../../lib/logger';

// How long to wait for the worker's boot handshake before falling back.
const BOOT_TIMEOUT_MS = 2000;
// How long stop() waits for the worker's 'ended' before force-terminating.
const STOP_TIMEOUT_MS = 3000;

// The slice of Worker the session touches, injectable for tests.
export interface WorkerLike {
  postMessage(message: unknown, transfer?: Transferable[]): void;
  terminate(): void;
  onmessage: ((e: MessageEvent) => void) | null;
}

export type AcquireDisplayStream = (
  config: CaptureConfig,
) => Promise<{ stream: MediaStream; track: MediaStreamTrack }>;

export class WorkerBroadcastSession implements BroadcastSessionLike {
  private worker: WorkerLike;
  private config: CaptureConfig;
  private serverUrl: string;
  private connectOpts: ConnectOptions;
  private cb: BroadcastCallbacks;
  private broadcastId?: string;
  private acquire: AcquireDisplayStream;

  // Matches BroadcastPipeline's constructor defaults; BroadcasterScreen
  // always calls setLadder before start anyway.
  private ladder: { selection: ResolutionSelection; framerate: FramerateSelection } = {
    selection: 'auto',
    framerate: 'native',
  };
  private encoderSettings: EncoderSettings = DEFAULT_ENCODER_SETTINGS;

  private localStream: MediaStream | null = null;
  private started = false;
  private ended = false;
  private disposed = false;
  private startResolve: (() => void) | null = null;
  private startReject: ((e: Error) => void) | null = null;
  private stopWaiter: (() => void) | null = null;

  constructor(
    worker: WorkerLike,
    config: CaptureConfig,
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
    acquire: AcquireDisplayStream = acquireDisplayStream,
  ) {
    this.worker = worker;
    this.config = config;
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.broadcastId = broadcastId;
    this.acquire = acquire;
    worker.onmessage = (e: MessageEvent) => this.onMessage(e.data as BroadcastWorkerOutbound);
  }

  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void {
    this.ladder = { selection, framerate };
    if (this.started) this.post({ type: 'setLadder', selection, framerate });
  }

  setEncoderSettings(settings: EncoderSettings): void {
    this.encoderSettings = settings;
    if (this.started) this.post({ type: 'setEncoderSettings', settings });
  }

  start(): Promise<void> {
    if (this.started) return Promise.reject(new Error('session already started'));
    this.started = true;
    this.post({
      type: 'start',
      config: this.config,
      serverUrl: this.serverUrl,
      connectOpts: this.connectOpts,
      broadcastId: this.broadcastId,
      selection: this.ladder.selection,
      framerate: this.ladder.framerate,
      encoderSettings: this.encoderSettings,
    });
    return new Promise((resolve, reject) => {
      this.startResolve = resolve;
      this.startReject = reject;
    });
  }

  async stop(): Promise<void> {
    if (this.disposed || this.ended) return;
    // Kill the capture indicator immediately; the worker stops its clone in
    // pipeline teardown.
    this.releaseLocalMedia();
    if (!this.started) {
      this.handleEnded();
      return;
    }
    this.post({ type: 'stop' });
    let timer: ReturnType<typeof setTimeout> | null = null;
    await new Promise<void>((resolve) => {
      this.stopWaiter = resolve;
      timer = setTimeout(resolve, STOP_TIMEOUT_MS);
    });
    if (timer) clearTimeout(timer);
    // Timeout path: the worker hung — force local teardown.
    if (!this.ended) this.handleEnded();
  }

  private onMessage(msg: BroadcastWorkerOutbound): void {
    if (this.disposed) return;
    switch (msg.type) {
      case 'boot':
        return; // consumed by createBroadcastSession; a duplicate is noise
      case 'awaitingCapture':
        void this.provideCapture();
        return;
      case 'started': {
        const resolve = this.startResolve;
        this.startResolve = this.startReject = null;
        resolve?.();
        return;
      }
      case 'startError': {
        // Mirrors BroadcastPipeline.start() rejection: the pipeline has torn
        // its session down (no zombie publisher) and onEnded must NOT fire —
        // the start() rejection is the caller's error surface.
        this.releaseLocalMedia();
        const reject = this.startReject;
        this.startResolve = this.startReject = null;
        this.ended = true;
        this.dispose();
        const cause = new Error(msg.message);
        reject?.(msg.phase ? new BroadcastStartError(msg.phase, cause) : cause);
        return;
      }
      case 'capturePath':
        this.cb.onCapturePathChosen(msg.path);
        return;
      case 'encoderConfigured':
        this.cb.onEncoderConfigured(msg.info);
        return;
      case 'broadcastId':
        this.cb.onBroadcastId?.(msg.id);
        return;
      case 'resumeToken':
        this.cb.onResumeToken?.(msg.token);
        return;
      case 'reconnecting':
        this.cb.onReconnecting?.({
          attempt: msg.attempt,
          delayMs: msg.delayMs,
          reason: msg.reason,
          closeCode: msg.closeCode,
        });
        return;
      case 'resumed':
        this.cb.onResumed?.();
        return;
      case 'stats':
        this.cb.onStats(msg.stats);
        return;
      case 'error':
        this.cb.onError(new Error(msg.message));
        return;
      case 'ended':
        this.handleEnded();
        return;
    }
  }

  // The worker connected and wants the screen share. Acquisition failures
  // (picker cancelled, permission denied) go back as 'captureFailed', which
  // the worker-side media factory turns into a phase-'capture' start error —
  // identical semantics to the main-thread path.
  private async provideCapture(): Promise<void> {
    let acquired: { stream: MediaStream; track: MediaStreamTrack };
    try {
      acquired = await this.acquire(this.config);
    } catch (e) {
      this.post({ type: 'captureFailed', message: e instanceof Error ? e.message : String(e) });
      return;
    }
    if (this.disposed || this.ended) {
      for (const t of acquired.stream.getTracks()) t.stop();
      return;
    }

    const nativeFps = acquired.track.getSettings().frameRate ?? null;
    let clone: MediaStreamTrack;
    try {
      clone = acquired.track.clone();
      this.post({ type: 'capture', track: clone, nativeFps }, [
        clone as unknown as Transferable,
      ]);
    } catch (e) {
      // Transfer failed despite the probe — release everything and fail the
      // start; the pipeline (phase 'capture') tears the relay session down.
      log.error('MediaStreamTrack transfer failed after a successful probe:', e);
      for (const t of acquired.stream.getTracks()) t.stop();
      this.post({ type: 'captureFailed', message: 'MediaStreamTrack transfer failed' });
      return;
    }

    this.localStream = acquired.stream;
    // Belt and braces: "Stop sharing" ends the whole source, so the worker's
    // clone fires 'ended' there too — both stop paths are idempotent.
    acquired.track.addEventListener('ended', () => {
      if (!this.ended) void this.stop();
    });
    this.cb.onSourceStream(acquired.stream);
  }

  private handleEnded(): void {
    if (this.ended && this.disposed) return;
    this.ended = true;
    this.releaseLocalMedia();
    this.dispose();
    const waiter = this.stopWaiter;
    this.stopWaiter = null;
    this.cb.onEnded();
    waiter?.();
  }

  private releaseLocalMedia(): void {
    if (!this.localStream) return;
    for (const t of this.localStream.getTracks()) t.stop();
    this.localStream = null;
  }

  private dispose(): void {
    this.disposed = true;
    this.worker.terminate();
  }

  private post(cmd: BroadcastWorkerCommand, transfer?: Transferable[]): void {
    this.worker.postMessage(cmd, transfer);
  }
}

// Whether the worker path can even be attempted from this main thread; the
// worker's own capabilities are checked by its boot handshake.
function canAttemptWorker(): boolean {
  return (
    typeof Worker === 'function' &&
    typeof document !== 'undefined' &&
    typeof HTMLCanvasElement !== 'undefined' &&
    typeof HTMLCanvasElement.prototype.captureStream === 'function'
  );
}

function waitForBoot(worker: Worker, timeoutMs: number): Promise<boolean> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => resolve(false), timeoutMs);
    worker.onmessage = (e: MessageEvent) => {
      const msg = e.data as BroadcastWorkerOutbound;
      if (msg.type !== 'boot') return;
      clearTimeout(timer);
      resolve(msg.supported);
    };
    worker.onerror = () => {
      clearTimeout(timer);
      resolve(false);
    };
  });
}

// Proves MediaStreamTrack transfer works before committing to the worker
// path: postMessage throws DataCloneError *synchronously on the sender* for
// non-transferable types, so a dummy canvas.captureStream track answers the
// question without a roundtrip (getDisplayMedia tracks can't be minted
// without a user prompt).
function probeTrackTransfer(worker: Worker): boolean {
  try {
    const canvas = document.createElement('canvas');
    canvas.width = 2;
    canvas.height = 2;
    const track = canvas.captureStream(0).getVideoTracks()[0];
    if (!track) return false;
    worker.postMessage({ type: 'probeTrack', track }, [track as unknown as Transferable]);
    return true;
  } catch (e) {
    log.info('MediaStreamTrack transfer unsupported; using main-thread pipeline:', e);
    return false;
  }
}

// Builds the broadcast session for BroadcasterScreen: the worker-offloaded
// pipeline where the environment supports it (Chromium), the unchanged
// main-thread BroadcastPipeline otherwise (Firefox, jsdom). Both implement
// BroadcastSessionLike, so the caller never branches.
export async function createBroadcastSession(
  config: CaptureConfig,
  serverUrl: string,
  connectOpts: ConnectOptions,
  callbacks: BroadcastCallbacks,
  broadcastId?: string,
): Promise<BroadcastSessionLike> {
  const worker = await tryCreateBroadcastWorker();
  if (!worker) {
    return new BroadcastPipeline(config, serverUrl, connectOpts, callbacks, broadcastId);
  }
  return new WorkerBroadcastSession(worker, config, serverUrl, connectOpts, callbacks, broadcastId);
}

async function tryCreateBroadcastWorker(): Promise<Worker | null> {
  if (!canAttemptWorker()) return null;
  let worker: Worker;
  try {
    worker = new Worker(new URL('../../transport/broadcaster.worker.ts', import.meta.url), {
      type: 'module',
    });
  } catch {
    return null;
  }
  const supported = await waitForBoot(worker, BOOT_TIMEOUT_MS);
  if (!supported || !probeTrackTransfer(worker)) {
    worker.terminate();
    log.info('Broadcast worker unavailable; using main-thread pipeline.');
    return null;
  }
  return worker;
}
