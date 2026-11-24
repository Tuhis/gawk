// Imperative glue between the broadcaster UI and the broadcast Web Worker,
// kept out of the component so the screen stays path-agnostic.
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

import { acquireDisplayStream, type DisplayStreamGrant } from '../../media/capture';
import type { CaptureConfig } from '../../media/types';
import type { FramerateSelection, ResolutionSelection } from '../../media/ladder';
import {
  BroadcastPipeline,
  BroadcastStartError,
  captureMediaSourceFrom,
  DEFAULT_ENCODER_SETTINGS,
  type BroadcastCallbacks,
  type BroadcastStopOptions,
  type BroadcastSessionLike,
  type EncoderSettings,
} from '../../transport/broadcaster';
import type {
  BroadcastWorkerCommand,
  BroadcastWorkerOutbound,
} from '../../transport/broadcast-worker-core';
import { DEFAULT_DELIVERY, type Delivery } from '../../transport/carrier-sender';
import type { ConnectOptions } from '../../transport/connection';
import { log } from '../../lib/logger';

// How long to wait for the worker's boot handshake before falling back.
const BOOT_TIMEOUT_MS = 2000;
// How long stop() waits for the worker's 'ended' before force-terminating.
const STOP_TIMEOUT_MS = 3000;

// Worker liveness. A killed worker (renderer OOM, hard crash) fires no event
// at all, so the session watches the pipeline's stats heartbeat instead: once
// started, it posts stats every 500 ms until teardown — through transport
// auto-resume too. WORKER_SILENCE_MS without one (ten missed beats) means the
// worker is gone.
export const WORKER_SILENCE_MS = 5000;
const LIVENESS_CHECK_MS = 1000;
// A watchdog tick this late means the main thread itself was stalled (system
// sleep, a frozen page, a debugger pause) — the worker's stats may be queued
// behind it, so the silence window restarts instead of judging.
const LIVENESS_STALL_MS = 3 * LIVENESS_CHECK_MS;
export const WORKER_SILENCE_MESSAGE =
  'The broadcast stopped responding (its background worker was lost), so viewers no longer get video. Start again to continue.';

export interface WorkerLivenessOptions {
  // Whether the page is hidden. While it is, no verdict: a hidden tab may
  // throttle the worker's timers (and freezes on some browsers), and nobody
  // is looking at the LIVE screen then anyway. The check resumes with a
  // fresh window once the page is visible, so a dead worker in a background
  // tab is caught within WORKER_SILENCE_MS of the broadcaster coming back.
  isHidden?: () => boolean;
}

function documentHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}

// The slice of Worker the session touches, injectable for tests.
export interface WorkerLike {
  postMessage(message: unknown, transfer?: Transferable[]): void;
  terminate(): void;
  onmessage: ((e: MessageEvent) => void) | null;
  onerror: ((e: ErrorEvent) => void) | null;
}

export type AcquireDisplayStream = (
  config: CaptureConfig,
) => Promise<{ stream: MediaStream; track: MediaStreamTrack; audioUnavailable?: boolean }>;

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
  private delivery: Delivery = DEFAULT_DELIVERY;

  private localStream: MediaStream | null = null;
  private started = false;
  private ended = false;
  private disposed = false;
  private startResolve: (() => void) | null = null;
  private startReject: ((e: Error) => void) | null = null;
  private stopWaiter: (() => void) | null = null;
  private stopping = false;
  private isHidden: () => boolean;
  private livenessTimer: ReturnType<typeof setInterval> | null = null;
  // Last heartbeat (or window restart) and last watchdog tick, Date.now().
  private lastHeardAt = 0;
  private lastCheckAt = 0;

  constructor(
    worker: WorkerLike,
    config: CaptureConfig,
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
    acquire: AcquireDisplayStream = acquireDisplayStream,
    liveness: WorkerLivenessOptions = {},
  ) {
    this.worker = worker;
    this.config = config;
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.broadcastId = broadcastId;
    this.acquire = acquire;
    this.isHidden = liveness.isHidden ?? documentHidden;
    worker.onmessage = (e: MessageEvent) => this.onMessage(e.data as BroadcastWorkerOutbound);
    // Replaces waitForBoot's handler, whose post-boot resolve is a no-op.
    worker.onerror = (e: ErrorEvent) => this.onWorkerError(e);
  }

  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void {
    this.ladder = { selection, framerate };
    if (this.started) this.post({ type: 'setLadder', selection, framerate });
  }

  setEncoderSettings(settings: EncoderSettings): void {
    this.encoderSettings = settings;
    if (this.started) this.post({ type: 'setEncoderSettings', settings });
  }

  setDelivery(delivery: Delivery): void {
    this.delivery = delivery;
    if (this.started) this.post({ type: 'setDelivery', delivery });
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
      delivery: this.delivery,
    });
    return new Promise((resolve, reject) => {
      this.startResolve = resolve;
      this.startReject = reject;
    });
  }

  async stop(opts?: BroadcastStopOptions): Promise<void> {
    if (this.disposed || this.ended) return;
    // The worker goes quiet while it tears down; stop()'s own timeout covers
    // a worker that never answers, so the liveness check stands down.
    this.stopping = true;
    this.disarmLiveness();
    // Kill the capture indicator immediately; the worker stops its clone in
    // pipeline teardown.
    this.releaseLocalMedia();
    if (!this.started) {
      this.handleEnded();
      return;
    }
    // The worker's pipeline closes the session (with 4014 for an end stop)
    // before it answers 'ended'. The timeout below is for a hung worker,
    // whose session dies with it and falls back to the relay's grace.
    this.post(opts?.end ? { type: 'stop', end: true } : { type: 'stop' });
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
        if (!this.stopping) this.armLiveness();
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
      case 'localRelay':
        this.cb.onLocalRelay?.(msg.ev);
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
        this.lastHeardAt = Date.now();
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

  // An uncaught exception inside the worker. The worker may keep running,
  // but its pipeline is in an unknown state, so the broadcast ends here —
  // like 'startError' during start, like 'error' + 'ended' after it.
  private onWorkerError(e: ErrorEvent): void {
    if (this.disposed || this.ended) return;
    e.preventDefault?.();
    const message = e.message || 'The broadcast worker failed';
    log.error('Broadcast worker error:', message);
    const reject = this.startReject;
    if (reject) {
      this.startResolve = this.startReject = null;
      this.releaseLocalMedia();
      this.ended = true;
      this.dispose();
      reject(new Error(message));
      return;
    }
    this.cb.onError(new Error(message));
    this.handleEnded();
  }

  private armLiveness(): void {
    if (this.livenessTimer !== null) return;
    this.lastHeardAt = this.lastCheckAt = Date.now();
    this.livenessTimer = setInterval(() => this.checkLiveness(), LIVENESS_CHECK_MS);
  }

  private disarmLiveness(): void {
    if (this.livenessTimer === null) return;
    clearInterval(this.livenessTimer);
    this.livenessTimer = null;
  }

  private checkLiveness(): void {
    const now = Date.now();
    const stalled = now - this.lastCheckAt > LIVENESS_STALL_MS;
    this.lastCheckAt = now;
    if (this.disposed || this.ended || this.stopping) {
      this.disarmLiveness();
      return;
    }
    if (stalled || this.isHidden()) {
      // No verdict; the window restarts from here (see WorkerLivenessOptions
      // and LIVENESS_STALL_MS).
      this.lastHeardAt = now;
      return;
    }
    if (now - this.lastHeardAt < WORKER_SILENCE_MS) return;
    log.error(`Broadcast worker silent for ${now - this.lastHeardAt} ms; treating it as dead.`);
    this.cb.onError(new Error(WORKER_SILENCE_MESSAGE));
    this.handleEnded();
  }

  // The worker connected and wants the screen share. Acquisition failures
  // (picker cancelled, permission denied) go back as 'captureFailed', which
  // the worker-side media factory turns into a phase-'capture' start error —
  // identical semantics to the main-thread path.
  private async provideCapture(): Promise<void> {
    let acquired: Awaited<ReturnType<AcquireDisplayStream>>;
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
    // The system-audio clone transfers beside the video clone (the
    // original audio track stays in the local stream, whose teardown stops
    // it). An audio clone failure degrades to video-only — audio must never
    // fail a start the video path would have survived.
    let audioClone: MediaStreamTrack | null = null;
    if (this.config.audio) {
      const audioTrack = acquired.stream.getAudioTracks?.()[0] ?? null;
      try {
        audioClone = audioTrack?.clone() ?? null;
      } catch (e) {
        log.warn('Audio track clone failed; broadcasting video-only:', e);
        audioClone = null;
      }
    }
    let clone: MediaStreamTrack;
    try {
      clone = acquired.track.clone();
      this.post(
        {
          type: 'capture',
          track: clone,
          nativeFps,
          audioTrack: audioClone,
          // The pipeline reports 'audio unavailable here' rather than the
          // indistinguishable 'no audio shared'.
          audioUnavailable: acquired.audioUnavailable ?? false,
        },
        audioClone
          ? [clone as unknown as Transferable, audioClone as unknown as Transferable]
          : [clone as unknown as Transferable],
      );
    } catch (e) {
      // Transfer failed despite the probe — release everything and fail the
      // start; the pipeline (phase 'capture') tears the relay session down.
      log.error('MediaStreamTrack transfer failed after a successful probe:', e);
      audioClone?.stop();
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
    this.disarmLiveness();
    this.worker.terminate();
  }

  private post(cmd: BroadcastWorkerCommand, transfer?: Transferable[]): void {
    this.worker.postMessage(cmd, transfer);
  }
}

// Whether the worker path can even be attempted from this main thread (the
// worker's own capabilities are checked by its boot handshake): null when it
// can, otherwise the missing capability, for the fallback reason.
function mainThreadGateFailure(): string | null {
  if (typeof Worker !== 'function') return 'Web Workers unavailable';
  if (
    typeof document === 'undefined' ||
    typeof HTMLCanvasElement === 'undefined' ||
    typeof HTMLCanvasElement.prototype.captureStream !== 'function'
  ) {
    // Needed to mint the dummy track the transfer probe sends.
    return 'canvas.captureStream unavailable';
  }
  return null;
}

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// A DOMException's name (DataCloneError, NotSupportedError) is the useful
// part; it is not an Error subclass in every realm, so read it duck-typed.
function errorName(e: unknown): string {
  const name = (e as { name?: unknown } | null)?.name;
  return typeof name === 'string' && name ? name : errorText(e);
}

// Waits for the worker's boot handshake: null when it reports itself
// capable, otherwise why not.
function waitForBoot(worker: WorkerLike, timeoutMs: number): Promise<string | null> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => resolve(`broadcast worker boot timed out after ${timeoutMs} ms`), timeoutMs);
    worker.onmessage = (e: MessageEvent) => {
      const msg = e.data as BroadcastWorkerOutbound;
      if (msg.type !== 'boot') return;
      clearTimeout(timer);
      if (msg.supported) {
        resolve(null);
      } else if (msg.missing && msg.missing.length > 0) {
        resolve(`${msg.missing.join(', ')} unavailable in worker scope`);
      } else {
        resolve('broadcast worker reported itself unsupported');
      }
    };
    worker.onerror = (e: ErrorEvent) => {
      clearTimeout(timer);
      // A script that fails to load (404, MIME, CSP worker-src) arrives here
      // as a plain message-less Event, not as a throw from new Worker.
      resolve(e?.message ? `broadcast worker failed to start: ${e.message}` : 'broadcast worker failed to load or start');
    };
  });
}

// Proves MediaStreamTrack transfer works before committing to the worker
// path: postMessage throws DataCloneError *synchronously on the sender* for
// non-transferable types, so a dummy canvas.captureStream track answers the
// question without a roundtrip (getDisplayMedia tracks can't be minted
// without a user prompt). Null on success, otherwise why it failed.
function probeTrackTransfer(worker: WorkerLike): string | null {
  let track: MediaStreamTrack | undefined;
  try {
    const canvas = document.createElement('canvas');
    canvas.width = 2;
    canvas.height = 2;
    track = canvas.captureStream(0).getVideoTracks()[0];
  } catch (e) {
    // Its own gate: a captureStream failure says nothing about transfer.
    return `canvas.captureStream failed (${errorName(e)})`;
  }
  if (!track) return 'canvas.captureStream produced no track';
  try {
    worker.postMessage({ type: 'probeTrack', track }, [track as unknown as Transferable]);
    return null;
  } catch (e) {
    return `MediaStreamTrack not transferable (${errorName(e)})`;
  }
}

// Builds the broadcast session for BroadcasterScreen: the worker-offloaded
// pipeline where the environment supports it, the main-thread
// BroadcastPipeline otherwise (e.g. Firefox, jsdom). Both implement
// BroadcastSessionLike, so the caller never branches.
//
// `grant` is the display stream the caller requested in its start click:
// Safari honours getDisplayMedia only from the user-gesture handler itself,
// and the worker boot + relay connect that precede capture outlast it. Both
// paths consume the grant instead of prompting.
export async function createBroadcastSession(
  config: CaptureConfig,
  serverUrl: string,
  connectOpts: ConnectOptions,
  callbacks: BroadcastCallbacks,
  broadcastId?: string,
  grant?: Promise<DisplayStreamGrant>,
): Promise<BroadcastSessionLike> {
  const { worker, reason } = await tryCreateBroadcastWorker();
  if (!worker) {
    const pipeline = new BroadcastPipeline(
      config,
      serverUrl,
      connectOpts,
      callbacks,
      broadcastId,
      undefined,
      captureMediaSourceFrom(grant),
    );
    pipeline.setPipelineFallbackReason(reason);
    return pipeline;
  }
  return new WorkerBroadcastSession(
    worker,
    config,
    serverUrl,
    connectOpts,
    callbacks,
    broadcastId,
    grant ? () => grant : acquireDisplayStream,
  );
}

// The outcome of trying the worker path: the booted worker, or the gate that
// rejected it, as a human-readable reason for the stats overlay and Copy
// diagnostics (docs/16 Decision 1).
export type BroadcastWorkerProbe<W extends WorkerLike = Worker> =
  | { worker: W; reason: null }
  | { worker: null; reason: string };

// Every gate is injectable so each rejection is testable without a real
// Worker; production uses the defaults.
export interface BroadcastWorkerProbeDeps<W extends WorkerLike> {
  mainThreadGate?: () => string | null;
  spawn?: () => W;
  probeTransfer?: (worker: W) => string | null;
  bootTimeoutMs?: number;
}

function spawnBroadcastWorker(): Worker {
  return new Worker(new URL('../../transport/broadcaster.worker.ts', import.meta.url), {
    type: 'module',
  });
}

// Four independent gates, in order: the main thread can host a worker at
// all, the worker script loads, its boot handshake reports the APIs it needs,
// and a MediaStreamTrack transfers into it.
export async function tryCreateBroadcastWorker<W extends WorkerLike = Worker>(
  deps: BroadcastWorkerProbeDeps<W> = {},
): Promise<BroadcastWorkerProbe<W>> {
  const fallback = (reason: string): BroadcastWorkerProbe<W> => {
    log.info(`Broadcast worker unavailable (${reason}); using main-thread pipeline.`);
    return { worker: null, reason };
  };
  const gate = (deps.mainThreadGate ?? mainThreadGateFailure)();
  if (gate) return fallback(gate);
  let worker: W;
  try {
    worker = (deps.spawn ?? (spawnBroadcastWorker as unknown as () => W))();
  } catch (e) {
    return fallback(`broadcast worker failed to load: ${errorText(e)}`);
  }
  const bootFailure = await waitForBoot(worker, deps.bootTimeoutMs ?? BOOT_TIMEOUT_MS);
  const failure = bootFailure ?? (deps.probeTransfer ?? probeTrackTransfer)(worker);
  if (failure) {
    worker.terminate();
    return fallback(failure);
  }
  return { worker, reason: null };
}
