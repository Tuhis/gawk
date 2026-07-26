// R8 S6: host-agnostic core of the worker-offloaded viewer. `viewer.worker.ts`
// is a thin `onmessage` shell around this; the core owns the S1–S5 pipeline
// *and* the reconnect state machine (it reuses ViewerSession unchanged, so
// backoff / code-4000-terminal / fatal-codec behave identically to the
// main-thread path). It is deliberately DOM-free: it talks to the outside
// world only through an injected `WorkerHost` (a postMessage-like event sink
// plus a render sink wrapping the OffscreenCanvas), which is what lets the
// whole thing be unit-tested synchronously with a fake host — no real Worker,
// no OffscreenCanvas, no DOM.

import type { ViewerDeliveryMode } from './resilient';
import type { ConnectOptions } from './connection';
import type { DecodedAudioChunk } from './audio-decode';
import type { AudioMuxCodec, AudioTapEvent, Fmp4Track } from './fmp4-muxer';
import type { PlayoutMode } from './playout';
import type { RenderSink } from './render-sink';
import type { ReleasedFrame } from './reorder-buffer';
import { ViewerPipeline } from './viewer';
import type { ViewerStats } from './viewer';
import { ViewerSession, type ViewerErrorKind, type ViewerSessionCallbacks } from './viewer-session';
import { LocalViewerTransport, type ViewerTransportFactory } from './viewer-transport';

// Main thread → worker.
export type ViewerWorkerCommand =
  // R22 (docs/27, keeping R16's Decision-1 constraint): `presentationMux` is
  // sent ONLY on gated (element-fullscreen-less) devices — non-gated devices'
  // init messages are byte-identical to before. It routes the reorder
  // buffer's released frames through the host frame tap so the (arm-created)
  // fMP4 muxer can feed the iPhone native-fullscreen video.
  | { type: 'init'; canvas: OffscreenCanvas; presentationMux?: boolean }
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
  | { type: 'resilient'; mode: ViewerDeliveryMode }
  // R22 (docs/27 Decision 5): start muxing — create the Fmp4Muxer at the
  // worker-shell level (it survives pipeline attempts/reconnects) and begin
  // posting segments. Idempotent; a no-op when init carried no mux flag.
  // `audio` (R22 audio, docs/27 findings 2 + 4) is the main thread's negotiated
  // encapsulation: 'opus' muxes the R15 lane verbatim (what Chrome takes),
  // 'aac' transcodes the decoded PCM because iOS refuses Opus in MP4. Absent
  // keeps the presentation video-only, exactly as before audio muxing existed.
  | { type: 'arm'; audio?: AudioMuxCodec }
  // R15 N5 (docs/20 Decision 10): the audio sink's ~4 Hz playhead report,
  // travelling the reverse direction of the stats flow. The AudioContext is
  // main-thread-only, so this is how the worker's pipeline gets an audio
  // clock to derive video display targets from. atEpochMs is absolute
  // (timeOrigin + now) because the two contexts have different timeOrigins.
  | { type: 'audioPlayhead'; playheadUs: number | null; atEpochMs: number };

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
  // R22 (gated devices only, after 'arm'): one fMP4 segment from the worker
  // muxer, buffer transferred. Init segments carry the authoritative mime
  // (codec derived from the bitstream); media segments carry the keyframe
  // flag so the main-thread queue can drop to a decodable restart point.
  | MuxSegmentEvent
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

// R22: the muxer's output events (see ViewerWorkerEvent above). `track` routes
// the segment to its own SourceBuffer on the main thread — video and audio are
// separate MSE streams (docs/27 finding 2).
export type MuxSegmentEvent =
  | { type: 'muxSegment'; kind: 'init'; track: Fmp4Track; mime: string; data: ArrayBuffer }
  | {
      type: 'muxSegment';
      kind: 'media';
      track: Fmp4Track;
      keyframe: boolean;
      data: ArrayBuffer;
    };

// R22 audio: what the pipeline forks to the shell's muxer (defined beside the
// muxer; re-exported here because the shell's host wiring is the only consumer).
export type { AudioTapEvent } from './fmp4-muxer';

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
  // R22 (docs/27 Decision 3): the encoded-frame fork. Present only when init
  // carried `presentationMux` (gated devices) — the pipeline then hands every
  // reorder-released frame here, upstream of the decoder, and the shell's
  // muxer (idle until 'arm') turns them into fMP4. Host-level so it survives
  // pipeline attempts/reconnects.
  frameTap?: (frame: ReleasedFrame) => void;
  // R22 audio: the encoded-audio fork, present under the same gate as frameTap.
  audioTap?: (ev: AudioTapEvent) => void;
  // R22 audio, iOS path (docs/27 finding 4): the DECODED-audio fork, for the
  // AAC transcode iOS needs because it refuses Opus in MP4. Fired before the
  // decoded buffers are transferred to the main thread.
  audioPcmTap?: (chunk: DecodedAudioChunk) => void;
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
        // R22 audio, iOS path: transcode BEFORE the post — the channel buffers
        // are transferred below, which detaches them.
        this.host.audioPcmTap?.(chunk);
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
      // R22: the encoded-frame fork for the shell's muxer. Generation-guarded
      // so a superseded session's late releases can't interleave a stale
      // timeline into the (session-long) mux stream.
      ...(this.host.frameTap
        ? {
            onReleasedFrame: (frame: ReleasedFrame) => {
              if (current()) this.host.frameTap!(frame);
            },
          }
        : {}),
      // R22 audio: same generation guard — a superseded session's audio must
      // not interleave into the (session-long) mux timeline.
      ...(this.host.audioTap
        ? {
            onAudioMux: (ev: AudioTapEvent) => {
              if (current()) this.host.audioTap!(ev);
            },
          }
        : {}),
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
