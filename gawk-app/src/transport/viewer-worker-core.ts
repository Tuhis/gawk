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
import type { RenderSink } from './render-sink';
import { ViewerPipeline } from './viewer';
import type { ViewerStats } from './viewer';
import { ViewerSession, type ViewerSessionCallbacks } from './viewer-session';
import { LocalViewerTransport, type ViewerTransportFactory } from './viewer-transport';

// Main thread → worker.
export type ViewerWorkerCommand =
  | { type: 'init'; canvas: OffscreenCanvas }
  | { type: 'start'; serverUrl: string; broadcastId: string; connectOpts: ConnectOptions }
  | { type: 'stop' }
  // R5 Q3: set the smoothed-playout mode in the worker's context (the reorder
  // buffer reads it live, so this works mid-session and across reconnects).
  | { type: 'playout'; smoothed: boolean };

// Worker → main thread. Small control/telemetry messages only — decoded frames
// are drawn in the worker and never appear here.
export type ViewerWorkerEvent =
  | { type: 'connected' }
  | { type: 'reconnecting'; attempt: number; reason: string }
  | { type: 'codec'; codec: string }
  | { type: 'stats'; stats: ViewerStats }
  | { type: 'error'; message: string; fatal: boolean }
  | { type: 'ended' };

// Shell-level boot handshake (posted by viewer.worker.ts on load, before any
// command): reports whether this worker's global scope actually has the codecs
// + transport the pipeline needs. The main thread waits for `supported: true`
// before transferring the OffscreenCanvas, and falls back to the main-thread
// pipeline otherwise (Firefox worker WebCodecs is the risk R8 flags).
export type ViewerWorkerBoot = { type: 'boot'; supported: boolean };
export type ViewerWorkerOutbound = ViewerWorkerEvent | ViewerWorkerBoot;

export interface WorkerHost {
  post(event: ViewerWorkerEvent): void;
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
      onReconnecting: ({ attempt, reason }) => {
        if (current()) this.host.post({ type: 'reconnecting', attempt, reason });
      },
      onError: (err) => {
        if (!current()) return;
        this.host.post({ type: 'error', message: err.message, fatal: Boolean((err as { fatal?: boolean }).fatal) });
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
      this.host.post({ type: 'error', message: err.message, fatal: Boolean((err as { fatal?: boolean }).fatal) });
    });
  }

  async stop(): Promise<void> {
    this.generation++; // invalidate the outgoing session's callbacks
    const s = this.session;
    this.session = null;
    if (s) await s.stop();
  }
}
