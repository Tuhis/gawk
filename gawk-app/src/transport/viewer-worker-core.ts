// R8 S6: host-agnostic core of the worker-offloaded viewer. `viewer.worker.ts`
// is a thin `onmessage` shell around this; the core owns the S1–S5 pipeline
// *and* the reconnect state machine (it reuses ViewerSession unchanged, so
// backoff / code-4000-terminal / fatal-codec behave identically to the
// main-thread path). It is deliberately DOM-free: it talks to the outside
// world only through an injected `WorkerHost` (a postMessage-like event sink
// plus a render sink wrapping the OffscreenCanvas), which is what lets the
// whole thing be unit-tested synchronously with a fake host — no real Worker,
// no OffscreenCanvas, no DOM.

import type { ConnectOptions } from './connection';
import type { PlayoutMode } from './playout';
import type { RenderSink } from './render-sink';
import { ViewerPipeline } from './viewer';
import type { ViewerStats } from './viewer';
import { ViewerSession, type ViewerErrorKind, type ViewerSessionCallbacks } from './viewer-session';
import { LocalViewerTransport, type ViewerTransportFactory } from './viewer-transport';

// Main thread → worker.
export type ViewerWorkerCommand =
  // R16: `presentationTee` is sent ONLY on gated (element-fullscreen-less)
  // devices — non-gated devices' init messages are byte-identical to before
  // (docs/21 Decision 1). It makes the worker probe the tee capability and,
  // if supported, build the sink chain with the (idle) TeeRenderSink inside.
  | { type: 'init'; canvas: OffscreenCanvas; presentationTee?: boolean }
  | { type: 'start'; serverUrl: string; broadcastId: string; connectOpts: ConnectOptions }
  | { type: 'stop' }
  // R5 Q3 + R12 T2: set the playout mode in the worker's context (the reorder
  // buffer and pipeline read it live, so this works mid-session and across
  // reconnects).
  | { type: 'playout'; mode: PlayoutMode }
  // R12 T4: the experimental frame-interpolation toggle, same crossing.
  | { type: 'interpolation'; enabled: boolean }
  // R19 (docs/24 Decision 9): resilient mode for this worker's context —
  // wider reorder/playout profile. Sent before 'start' (the delivery
  // negotiation itself rides connectOpts.deliveryMode into the subscribe
  // URL); a mode change is a deliberate reconnect, not a live flip.
  | { type: 'resilient'; enabled: boolean }
  // R16 Decision 4: activate the tee — create the VideoTrackGenerator in the
  // worker, post its track back (transferred), start capturing presented
  // frames. Idempotent; a no-op when init carried no tee or the probe failed.
  | { type: 'arm' };

// Worker → main thread. Small control/telemetry messages only — decoded frames
// are drawn in the worker and never appear here.
export type ViewerWorkerEvent =
  | { type: 'connected' }
  | { type: 'reconnecting'; attempt: number; reason: string; closeCode?: number | null }
  | { type: 'codec'; codec: string }
  | { type: 'stats'; stats: ViewerStats }
  // `message` is console-only detail; the screen renders copy keyed on `kind`.
  | { type: 'error'; message: string; fatal: boolean; kind: ViewerErrorKind }
  | { type: 'ended' }
  // R16 (gated devices only, in response to init.presentationTee): whether
  // VideoTrackGenerator + VideoFrame-from-canvas work in this worker. False ⇒
  // the screen never arms and fullscreen stays tier 3 (pseudo).
  | { type: 'presentationProbe'; supported: boolean }
  // R16: the generator's track, transferred once on arm. Host-level in the
  // worker — it survives pipeline attempts/reconnects (docs/21 Decision 4).
  | { type: 'presentationTrack'; track: MediaStreamTrack }
  // R15 (docs/20 Decision 7): decoded planar PCM for the main-thread
  // AudioWorklet sink — the first deliberate decoded-media crossing (an
  // AudioContext cannot exist in a dedicated worker). The channel buffers
  // are transferred, so nothing is structured-cloned.
  | {
      type: 'audioChunk';
      timestampUs: number;
      sampleRate: number;
      channels: Float32Array[];
      frameCount: number;
    }
  // R15 (docs/20 Decision 8): restart/reconnect — the sink must flush and
  // re-anchor before the new timeline's packets arrive.
  | { type: 'audioReset' };

// Shell-level boot handshake (posted by viewer.worker.ts on load, before any
// command): reports whether this worker's global scope actually has the codecs
// + transport the pipeline needs. The main thread waits for `supported: true`
// before transferring the OffscreenCanvas, and falls back to the main-thread
// pipeline otherwise (Firefox worker WebCodecs is the risk R8 flags).
export type ViewerWorkerBoot = { type: 'boot'; supported: boolean };
export type ViewerWorkerOutbound = ViewerWorkerEvent | ViewerWorkerBoot;

export interface WorkerHost {
  // R15: the optional transfer list carries decoded audio buffers across
  // without a structured clone.
  post(event: ViewerWorkerEvent, transfer?: Transferable[]): void;
  renderSink: RenderSink;
  // R10 P3: how each pipeline attempt gets its transport. The shell supplies
  // a nested-transport-worker factory where nested workers exist; omitted, the
  // pipeline runs its transport in-process (this worker) as before.
  transportFactory?: ViewerTransportFactory;
}

// Injectable for tests; the default wires a real ViewerSession whose pipeline
// draws to the host's render sink instead of forwarding frames.
export type ViewerSessionLike = { start(): Promise<void>; stop(): Promise<void> };
export type ViewerSessionFactory = (
  serverUrl: string,
  broadcastId: string,
  connectOpts: ConnectOptions,
  callbacks: ViewerSessionCallbacks,
  renderSink: RenderSink,
) => ViewerSessionLike;

const defaultSessionFactory = (host: WorkerHost): ViewerSessionFactory => {
  const transportFactory =
    host.transportFactory ?? ((url, opts) => new LocalViewerTransport(url, opts));
  return (url, id, opts, cbs, sink) =>
    new ViewerSession(
      url,
      id,
      opts,
      cbs,
      (u, i, o, c) => new ViewerPipeline(u, i, o, c, sink, transportFactory),
    );
};

export class ViewerWorkerCore {
  private host: WorkerHost;
  private createSession: ViewerSessionFactory;
  private session: ViewerSessionLike | null = null;
  // Bumped on every start/stop so callbacks from a superseded session (whose
  // async teardown outlives it — e.g. a broadcast-id change or StrictMode
  // remount) are ignored instead of clobbering the live session's state.
  private generation = 0;

  constructor(host: WorkerHost, createSession?: ViewerSessionFactory) {
    this.host = host;
    this.createSession = createSession ?? defaultSessionFactory(host);
  }

  start(params: { serverUrl: string; broadcastId: string; connectOpts: ConnectOptions }): void {
    // A previous session may still be tearing down; supersede it.
    const prev = this.session;
    const gen = ++this.generation;
    if (prev) void prev.stop();

    const current = (): boolean => gen === this.generation;

    const cbs: ViewerSessionCallbacks = {
      // Frames are rendered inside ViewerPipeline via the injected sink; this
      // never fires on the worker path, but must satisfy the interface.
      onDecodedFrame: () => {},
      onConfig: (config) => {
        if (current()) this.host.post({ type: 'codec', codec: config.codec });
      },
      onStats: (stats) => {
        if (current()) this.host.post({ type: 'stats', stats });
      },
      onConnected: () => {
        if (current()) this.host.post({ type: 'connected' });
      },
      onReconnecting: ({ attempt, reason, closeCode }) => {
        if (current()) this.host.post({ type: 'reconnecting', attempt, reason, closeCode });
      },
      onError: (err) => {
        if (!current()) return;
        // ViewerSession fires onError only for a fatal pipeline verdict or an
        // exhausted reconnect budget — fatal is what distinguishes the two.
        const fatal = Boolean((err as { fatal?: boolean }).fatal);
        this.host.post({ type: 'error', message: err.message, fatal, kind: fatal ? 'unplayable' : 'lost' });
        // Mirrors the main-thread ViewerScreen: ensure teardown/onEnded runs
        // after a terminal error (e.g. reconnect budget exhausted, which does
        // not fire onEnded on its own).
        void session.stop();
      },
      onEnded: () => {
        if (!current()) return;
        this.session = null;
        this.host.post({ type: 'ended' });
      },
      // R15 (docs/20 Decision 7): decoded PCM crosses to the main thread,
      // channel buffers transferred.
      onAudioChunk: (chunk) => {
        if (!current()) return;
        this.host.post(
          {
            type: 'audioChunk',
            timestampUs: chunk.timestampUs,
            sampleRate: chunk.sampleRate,
            channels: chunk.channels,
            frameCount: chunk.frameCount,
          },
          chunk.channels.map((c) => c.buffer as ArrayBuffer),
        );
      },
      onAudioReset: () => {
        if (current()) this.host.post({ type: 'audioReset' });
      },
    };

    const session = this.createSession(
      params.serverUrl,
      params.broadcastId,
      params.connectOpts,
      cbs,
      this.host.renderSink,
    );
    this.session = session;

    session.start().catch((e) => {
      // First-connect failure is fatal by ViewerSession policy and fires no
      // callbacks — surface it here (matches ViewerScreen's start().catch).
      if (!current()) return;
      this.session = null;
      const err = e instanceof Error ? e : new Error(String(e));
      this.host.post({
        type: 'error',
        message: err.message,
        fatal: Boolean((err as { fatal?: boolean }).fatal),
        kind: 'unreachable',
      });
    });
  }

  async stop(): Promise<void> {
    this.generation++; // invalidate the outgoing session's callbacks
    const s = this.session;
    this.session = null;
    if (s) await s.stop();
  }
}
