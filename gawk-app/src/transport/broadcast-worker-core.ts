// R11 (docs/16): host-agnostic core of the worker-offloaded broadcaster.
// `broadcaster.worker.ts` is a thin `onmessage` shell around this; the core
// owns the pipeline lifecycle and reuses BroadcastPipeline unchanged (with a
// media source that waits for the main thread to transfer the capture track).
// Deliberately DOM-free: it talks to the outside world only through an
// injected `BroadcastWorkerHost`, which is what lets it be unit-tested
// synchronously with a fake host and a fake pipeline factory — no real
// Worker, no MSTP, no WebTransport.

import { trackMediaSource, type BroadcastMediaSourceFactory } from '../media/capture';
import type { EncoderConfigured } from '../media/encoder';
import type { FramerateSelection, ResolutionSelection } from '../media/ladder';
import type { CaptureConfig } from '../media/types';
import {
  BroadcastPipeline,
  BroadcastStartError,
  type BroadcastCallbacks,
  type BroadcastStartPhase,
  type BroadcastStats,
  type EncoderSettings,
} from './broadcaster';
import type { ConnectOptions } from './connection';

// Main thread → worker.
export type BroadcastWorkerCommand =
  | {
      type: 'start';
      config: CaptureConfig;
      serverUrl: string;
      connectOpts: ConnectOptions;
      broadcastId?: string;
      selection: ResolutionSelection;
      framerate: FramerateSelection;
      // R12: the advanced encoder settings ride the start command (and the
      // dedicated command below for live changes).
      encoderSettings?: EncoderSettings;
    }
  // The capture track (transferred), in response to 'awaitingCapture'.
  | { type: 'capture'; track: MediaStreamTrack; nativeFps: number | null }
  | { type: 'captureFailed'; message: string }
  | { type: 'setLadder'; selection: ResolutionSelection; framerate: FramerateSelection }
  | { type: 'setEncoderSettings'; settings: EncoderSettings }
  | { type: 'stop' };

// Worker → main thread. Small control/telemetry messages only — VideoFrames
// are born, encoded and closed inside the worker and never appear here.
export type BroadcastWorkerEvent =
  // Connect phase succeeded; main must now run the share picker and answer
  // with 'capture' or 'captureFailed'. Preserves the connect-before-picker
  // ordering (a connect failure never shows the picker).
  | { type: 'awaitingCapture' }
  | { type: 'started' }
  // phase is null when the failure wasn't a BroadcastStartError (shouldn't
  // happen; the session then surfaces a plain error and never mint-retries).
  | { type: 'startError'; phase: BroadcastStartPhase | null; message: string }
  | { type: 'capturePath'; path: string }
  | { type: 'encoderConfigured'; info: EncoderConfigured }
  | { type: 'broadcastId'; id: string }
  | { type: 'stats'; stats: BroadcastStats }
  | { type: 'error'; message: string }
  | { type: 'ended' };

// Shell-level boot handshake (posted by broadcaster.worker.ts on load, before
// any command): whether this worker's global scope has the codecs, transport
// and MSTP the pipeline needs. The main thread falls back to the main-thread
// pipeline otherwise.
export type BroadcastWorkerBoot = { type: 'boot'; supported: boolean };
export type BroadcastWorkerOutbound = BroadcastWorkerEvent | BroadcastWorkerBoot;

export interface BroadcastWorkerHost {
  post(event: BroadcastWorkerEvent): void;
}

// Injectable for tests; the default wires a real BroadcastPipeline.
export type BroadcastPipelineLike = {
  start(): Promise<void>;
  stop(): Promise<void>;
  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void;
  setEncoderSettings(settings: EncoderSettings): void;
};
export type BroadcastPipelineFactory = (
  config: CaptureConfig,
  serverUrl: string,
  connectOpts: ConnectOptions,
  callbacks: BroadcastCallbacks,
  broadcastId: string | undefined,
  mediaSource: BroadcastMediaSourceFactory,
) => BroadcastPipelineLike;

const defaultPipelineFactory: BroadcastPipelineFactory = (config, url, opts, cbs, id, mediaSource) =>
  new BroadcastPipeline(config, url, opts, cbs, id, undefined, mediaSource);

interface PendingCapture {
  resolve: (m: { track: MediaStreamTrack; nativeFps: number | null }) => void;
  reject: (e: Error) => void;
}

export class BroadcastWorkerCore {
  private host: BroadcastWorkerHost;
  private createPipeline: BroadcastPipelineFactory;
  private pipeline: BroadcastPipelineLike | null = null;
  private pendingCapture: PendingCapture | null = null;
  // Bumped on every start/stop so callbacks from a superseded pipeline (whose
  // async teardown outlives it) are ignored instead of clobbering the live
  // one's state (mirrors ViewerWorkerCore).
  private generation = 0;

  constructor(host: BroadcastWorkerHost, createPipeline?: BroadcastPipelineFactory) {
    this.host = host;
    this.createPipeline = createPipeline ?? defaultPipelineFactory;
  }

  start(params: {
    config: CaptureConfig;
    serverUrl: string;
    connectOpts: ConnectOptions;
    broadcastId?: string;
    selection: ResolutionSelection;
    framerate: FramerateSelection;
    encoderSettings?: EncoderSettings;
  }): void {
    const prev = this.pipeline;
    const gen = ++this.generation;
    if (prev) void prev.stop();
    this.rejectPendingCapture(new Error('superseded by a new start'));

    const current = (): boolean => gen === this.generation;

    // The pipeline calls this after its transport connect succeeds; the
    // promise resolves when the main thread transfers the capture track.
    const mediaSource: BroadcastMediaSourceFactory = () =>
      new Promise((resolve, reject) => {
        if (!current()) {
          reject(new Error('superseded by a new start'));
          return;
        }
        this.pendingCapture = {
          resolve: ({ track, nativeFps }) => resolve(trackMediaSource(track, nativeFps)),
          reject,
        };
        this.host.post({ type: 'awaitingCapture' });
      });

    const cbs: BroadcastCallbacks = {
      // The worker-side source has no stream; the preview stream lives on the
      // main thread (WorkerBroadcastSession fires the UI's onSourceStream).
      onSourceStream: () => {},
      onCapturePathChosen: (path) => {
        if (current()) this.host.post({ type: 'capturePath', path });
      },
      onEncoderConfigured: (info) => {
        if (current()) this.host.post({ type: 'encoderConfigured', info });
      },
      onStats: (stats) => {
        if (current()) this.host.post({ type: 'stats', stats });
      },
      onBroadcastId: (id) => {
        if (current()) this.host.post({ type: 'broadcastId', id });
      },
      onError: (err) => {
        if (current()) this.host.post({ type: 'error', message: err.message });
      },
      onEnded: () => {
        if (!current()) return;
        this.pipeline = null;
        this.host.post({ type: 'ended' });
      },
    };

    const pipeline = this.createPipeline(
      params.config,
      params.serverUrl,
      params.connectOpts,
      cbs,
      params.broadcastId,
      mediaSource,
    );
    pipeline.setLadder(params.selection, params.framerate);
    if (params.encoderSettings) pipeline.setEncoderSettings(params.encoderSettings);
    this.pipeline = pipeline;

    pipeline.start().then(
      () => {
        if (current()) this.host.post({ type: 'started' });
      },
      (e: unknown) => {
        if (!current()) return;
        this.pipeline = null;
        const message = e instanceof Error ? e.message : String(e);
        const phase = e instanceof BroadcastStartError ? e.phase : null;
        this.host.post({ type: 'startError', phase, message });
      },
    );
  }

  capture(track: MediaStreamTrack, nativeFps: number | null): void {
    const pending = this.pendingCapture;
    this.pendingCapture = null;
    if (!pending) {
      // Stray track (e.g. raced a stop): don't leave the capture indicator on.
      track.stop();
      return;
    }
    pending.resolve({ track, nativeFps });
  }

  captureFailed(message: string): void {
    this.rejectPendingCapture(new Error(message));
  }

  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void {
    this.pipeline?.setLadder(selection, framerate);
  }

  setEncoderSettings(settings: EncoderSettings): void {
    this.pipeline?.setEncoderSettings(settings);
  }

  // Stops the live pipeline and always answers with 'ended' — the pipeline's
  // own onEnded is generation-filtered here, and the main-side session awaits
  // this event to resolve its stop().
  async stop(): Promise<void> {
    this.generation++;
    this.rejectPendingCapture(new Error('broadcast stopped'));
    const p = this.pipeline;
    this.pipeline = null;
    if (p) await p.stop();
    this.host.post({ type: 'ended' });
  }

  private rejectPendingCapture(err: Error): void {
    const pending = this.pendingCapture;
    this.pendingCapture = null;
    pending?.reject(err);
  }
}
