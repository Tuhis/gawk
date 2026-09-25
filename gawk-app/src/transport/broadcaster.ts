// Broadcaster pipeline: capture → encode → packetize → /publish datagrams.
// The capture/encode half mirrors media/loopback.ts (first-frame encoder
// negotiation and all); the decode half is replaced by the network.

import { log } from '../lib/logger';
import {
  audioLaneSupported,
  startAudioLane,
  type RunningAudioLane,
} from '../media/audio-lane';
import {
  startCapture,
  stopCapture,
  type BroadcastMediaSource,
  type BroadcastMediaSourceFactory,
  type DisplayStreamGrant,
} from '../media/capture';
import { Encoder, type EncodedFrame, type EncoderConfigured } from '../media/encoder';
import {
  applyCeiling,
  autoLadder,
  clampBitrateOverride,
  computeBitrate,
  hardwareCeiling,
  resolveAutoFps,
  rungCapWidth,
  type FramerateRung,
  type FramerateSelection,
  type ResolutionRung,
  type ResolutionSelection,
} from '../media/ladder';
import {
  EncoderSupportProber,
  probeSupportMatrix,
  probeSupported,
  type HwPreference,
  type SourceDims,
  type SupportMatrix,
} from '../media/probe';
import { FallbackController } from '../media/fallback';
import { FramePreprocessor } from '../media/preprocess';
import type { CaptureConfig } from '../media/types';
import {
  bytesToHex,
  connectWebTransport,
  DatagramSender,
  readDatagrams,
  type ConnectOptions,
} from './connection';
import { ConnectionStatsSampler, type TransportConnectionStats } from './net-stats';
import {
  packetizeDecoderConfig,
  packetizeFrameWithParity,
  packetizeStreamKeyframe,
} from './packetizer';
import { CAP_PARITY_CHUNKS, parseRelayCapabilities } from './parity';
import {
  RECONNECT_MAX_ATTEMPTS,
  isTerminalPublisherClose,
  reconnectDelayMs,
  type ReconnectInfo,
} from './reconnect';
import { CLOCK_MAPPING_INTERVAL_MS, TimeSyncClient } from './time-sync';
import {
  CLOSE_CODE_PUBLISHER_SUPERSEDED,
  CLOSE_CODE_TERMINATED_BY_OPERATOR,
  encodeClockMapping,
  nextFrameId,
  parseBroadcastAnnounce,
  parseResumeToken,
  parseSessionClosing,
  parseTelemetryHello,
  parseViewerCount,
  peekType,
  TYPE_BROADCAST_ANNOUNCE,
  TYPE_RESUME_TOKEN,
  TYPE_RELAY_CAPABILITIES,
  TYPE_SESSION_CLOSING,
  TYPE_TELEMETRY_HELLO,
  TYPE_TELEMETRY_ENDPOINT,
  parseTelemetryEndpoint,
  TYPE_VIEWER_COUNT,
  VIEWER_COUNT_SIZE,
  type TelemetryHelloMessage,
} from './wire';

export interface BroadcastStats {
  encodedFrames: number;
  keyframes: number;
  droppedFrames: number;
  fpsGateDropped: number;
  datagramsSent: number;
  bytesSent: number;
  configsSent: number;
  // Keyframes travel over reliable uni streams, deltas over datagrams.
  keyframeStreamsSent: number;
  keyframeStreamsFailed: number;
  keyframeBytesSent: number;
  encoderQueueDepth: number;
  encoderFps: number;
  lastEncodeLatencyMs: number;
  // Funnel rates: capture → post-gate → encoded → sent.
  // A rate gap between two adjacent stages localizes the bottleneck to that
  // stage. captureFps counts frames delivered by the capture path *before*
  // the fps gate; sentFps counts frames whose bytes were actually handed to
  // the transport without error (the "actually sent framerate").
  captureFps: number;
  sentFps: number;
  // Connection health for this leg (broadcaster→relay); null when the
  // browser doesn't implement WebTransport.getStats().
  connection: TransportConnectionStats | null;
  // Self-owned broadcaster↔relay RTT from the TimeSync exchange — works
  // where getStats() doesn't. Null until the first ping/pong completes.
  timeSyncRttMs: number | null;
  // Automatic-fallback observability. autoRung is the currently
  // applied ladder rung in auto mode, null in explicit mode. encoderPressure
  // is the explicit-mode passive warning: the encoder can't keep up but the
  // rung is held because the broadcaster chose it.
  autoRung: ResolutionRung | null;
  autoAtFloor: boolean;
  autoStepDowns: number;
  autoStepUps: number;
  encoderPressure: boolean;
  // The HW-aware auto ceiling (null in explicit resolution
  // mode) and the resolved 'auto' framerate (null when the fps selection is
  // an explicit rung).
  autoCeiling: ResolutionRung | null;
  autoFps: number | null;
  // Where the pipeline runs, detected via `window` absence — 'worker' on the
  // offloaded path.
  pipelineContext: 'worker' | 'main-thread';
  // The live "N watching" number the relay pushes
  // to this publisher session (~1 s cadence, change-driven). Null until the
  // first push lands; survives transport resumes (the new session re-pushes
  // within a tick).
  viewerCount: number | null;
  // The audio lane. 'off' = audio not requested (zero audio code paths
  // ran); 'no-track' = requested but the grant had no audio track (Firefox,
  // unchecked picker box) — a state, not an error; 'unavailable' = the
  // browser refused to start an audio source at all and capture fell back to
  // a video-only grant (Chromium on Linux/macOS screen shares);
  // 'unsupported' = track present but this scope lacks AudioEncoder/MSTP;
  // 'error' = the lane died mid-broadcast (video continues).
  audioState: 'off' | 'no-track' | 'unavailable' | 'unsupported' | 'active' | 'error';
  audioEncodedPackets: number;
  audioPacketsSent: number;
  audioBytesSent: number;
  audioConfigsSent: number;
  audioEncodedPerSec: number;
  audioSentPerSec: number;
  audioSampleRate: number | null;
  audioChannels: number | null;
  audioCodec: string | null;
  audioBitrateBps: number | null;
  // Forward parity on the delta path. parityLevel is what this producer is
  // EMITTING — the fleet level the relay advertised via RelayCapabilities, 0
  // when the relay advertises none or is configured off. It is not a
  // request: a producer cannot choose to protect a stream the fleet has not
  // asked it to protect, because the relay is what filters the symbols per
  // subscriber.
  parityLevel: number;
  parityChunksSent: number;
  parityBytesSent: number;
  // How long the encoder took to hand back the packet for a captured frame.
  // Audio timestamps are pinned at capture arrival (the stage video is
  // stamped at), so this delay is measured rather than written into them.
  audioEncodeLagMs: number | null;
  // Times the audio media clock drifted far enough to be re-pinned. Each one
  // steps the audio timeline against video.
  audioAnchorReanchors: number;
  // What this broadcast was ASKED to be, as opposed to what it turned out to
  // be. Everything else on this object is an outcome; without the target, no
  // consumer can compute the difference — "30 fps" reads identically whether
  // 30 or 60 was requested, and a session that delivered half its intended
  // rate has a perfect funnel and no dips.
  //
  // Deliberately NOT read back from the settings store or `getSettings()`:
  // these are the values the encoder actually committed to
  // (`EncoderConfigured`), which is the same "trust the real thing, not the
  // metadata" rule the capture path follows. Null until the encoder configures
  // — an unconfigured broadcast has no target, and a zero would claim one.
  targetWidth: number | null;
  targetHeight: number | null;
  targetFps: number | null;
  targetBitrateBps: number | null;
  // The codec string the encoder settled on, and whether it got hardware.
  // Both are negotiated outcomes of a *request*, which is why they live here
  // beside the targets rather than in the funnel.
  codec: string | null;
  acceleration: string | null;
  // Whether the tab is hidden, and for how long in total. The browser
  // throttles a hidden tab, so capture and encode rates fall for a reason
  // outside the pipeline. `documentHiddenMs` is CUMULATIVE so a reader can
  // delta two samples and get the hidden share of the window a rate was
  // measured over. Attached on the main thread by BroadcasterScreen: the
  // pipeline may run in a worker, where `document` does not exist.
  documentHidden?: boolean;
  documentHiddenMs?: number;
}

const EMPTY_BROADCAST_STATS: BroadcastStats = {
  encodedFrames: 0,
  keyframes: 0,
  droppedFrames: 0,
  fpsGateDropped: 0,
  datagramsSent: 0,
  bytesSent: 0,
  configsSent: 0,
  keyframeStreamsSent: 0,
  keyframeStreamsFailed: 0,
  keyframeBytesSent: 0,
  encoderQueueDepth: 0,
  encoderFps: 0,
  lastEncodeLatencyMs: 0,
  captureFps: 0,
  sentFps: 0,
  connection: null,
  timeSyncRttMs: null,
  autoRung: null,
  autoAtFloor: false,
  autoStepDowns: 0,
  autoStepUps: 0,
  encoderPressure: false,
  autoCeiling: null,
  autoFps: null,
  pipelineContext: 'main-thread',
  viewerCount: null,
  audioState: 'off',
  audioEncodedPackets: 0,
  audioPacketsSent: 0,
  audioBytesSent: 0,
  audioConfigsSent: 0,
  audioEncodedPerSec: 0,
  audioSentPerSec: 0,
  audioSampleRate: null,
  audioChannels: null,
  audioCodec: null,
  audioBitrateBps: null,
  audioEncodeLagMs: null,
  audioAnchorReanchors: 0,
  parityLevel: 0,
  parityChunksSent: 0,
  parityBytesSent: 0,
  targetWidth: null,
  targetHeight: null,
  targetFps: null,
  targetBitrateBps: null,
  codec: null,
  acceleration: null,
};

export interface BroadcastCallbacks {
  onSourceStream: (stream: MediaStream) => void;
  onEncoderConfigured: (info: EncoderConfigured) => void;
  onCapturePathChosen: (path: string) => void;
  onStats: (stats: BroadcastStats) => void;
  onError: (err: Error) => void;
  onEnded: () => void;
  onBroadcastId?: (id: string) => void;
  // The hex resume token minted by the relay (wire 0x09). The UI
  // keeps it next to the broadcast ID so a manual restart can reclaim.
  onResumeToken?: (token: string) => void;
  // Auto-resume: the session died mid-broadcast and a transport-only
  // reconnect is scheduled (capture + encoder stay alive, frames drop).
  onReconnecting?: (info: ReconnectInfo) => void;
  // The transport reconnected; the pipeline forced a fresh keyframe.
  onResumed?: () => void;
  // This session's telemetry identity, straight off wire 0x0D. Optional on
  // purpose — an older relay, or one with telemetry off, never sends it, and
  // the collector's correct behaviour then is to collect nothing. Fires once
  // per transport session, so an auto-resume delivers a fresh identity for the
  // new session.
  onTelemetryHello?: (hello: TelemetryHelloMessage) => void;
  onTelemetryEndpoint?: (url: string) => void;
}

function roundDownToEven(n: number): number {
  return n - (n % 2);
}

export type BroadcastStartPhase = 'connect' | 'capture';

// The advanced encoder settings. hwPreference selects the variant cascade;
// bitrateOverride (bps, clamped by
// clampBitrateOverride) replaces the ladder math while set; codecOverride
// pins the preference list to one codec. All three take effect via encoder
// recreate on the next frame — never a stream restart.
export interface EncoderSettings {
  hwPreference: HwPreference;
  bitrateOverride: number | null;
  codecOverride: string | null;
}

export const DEFAULT_ENCODER_SETTINGS: EncoderSettings = {
  hwPreference: 'auto',
  bitrateOverride: null,
  codecOverride: null,
};

// The surface BroadcasterScreen drives. Implemented by
// BroadcastPipeline (main thread) and WorkerBroadcastSession (worker path),
// so the screen's reclaim/mint/error logic is path-agnostic.
export interface BroadcastSessionLike {
  start(): Promise<void>;
  stop(): Promise<void>;
  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void;
  setEncoderSettings(settings: EncoderSettings): void;
}

// Default media source: the main-thread capture path.
// Lives here (not capture.ts) so tests that mock '../media/capture' keep
// stubbing startCapture/stopCapture without also faking the adapter.
// `grant` is the display stream BroadcasterScreen requested in the start
// click (Safari's user-gesture rule); without one, capture prompts itself.
export const captureMediaSourceFrom = (
  grant?: Promise<DisplayStreamGrant>,
): BroadcastMediaSourceFactory => async (config) => {
  const handle = await startCapture(config, grant);
  return {
    capturePath: handle.capturePath,
    stream: handle.stream,
    // Framerate is the one setting still taken from getSettings(): frames
    // don't carry a rate, and it only seeds the encoder's rate-control hint
    // when the framerate rung is 'native'.
    nativeFps: handle.track.getSettings().frameRate ?? null,
    // The system-audio track when audio was requested and the grant
    // delivered one; stopCapture stops every stream track, audio included.
    // Never inspected when audio wasn't requested (also: fakes without audio
    // APIs).
    audioTrack: config.audio ? (handle.stream.getAudioTracks?.()[0] ?? null) : null,
    audioUnavailable: handle.audioUnavailable,
    onEnded: (cb) => handle.track.addEventListener('ended', cb),
    startFrames: (onFrame) => handle.startFrames(onFrame),
    stop: () => stopCapture(handle),
    ...(typeof handle.track.applyConstraints === 'function'
      ? { applyConstraints: (c: MediaTrackConstraints) => handle.track.applyConstraints(c) }
      : {}),
  };
};

const captureMediaSource = captureMediaSourceFrom();

// Thrown by BroadcastPipeline.start(). The phase tells the caller whether a
// relay session was ever established: 'connect' failures never had one (safe
// to retry, e.g. mint after a failed reclaim), while 'capture' failures had a
// live publisher session which the pipeline has already torn down — falling
// back to a different broadcast ID would be wrong there.
export class BroadcastStartError extends Error {
  readonly phase: BroadcastStartPhase;

  constructor(phase: BroadcastStartPhase, cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause));
    this.name = 'BroadcastStartError';
    this.phase = phase;
    this.cause = cause;
  }
}

// The sentence a broadcaster sees when the relay ends its publisher session
// for good. Deliberately parallel to the natives' closeCodeError (Go) and
// close_code_message (Rust): the same close code must read the same on all
// three broadcasters, because it is one product and one support conversation.
function terminalPublisherMessage(code: number): string {
  switch (code) {
    case CLOSE_CODE_PUBLISHER_SUPERSEDED:
      return 'Another broadcaster took over this code — this session has been superseded.';
    case CLOSE_CODE_TERMINATED_BY_OPERATOR:
      return 'This broadcast was terminated by the server operator.';
    default:
      return 'The relay ended this broadcast.';
  }
}

export class BroadcastPipeline {
  private config: CaptureConfig;
  private serverUrl: string;
  private connectOpts: ConnectOptions;
  private cb: BroadcastCallbacks;
  private broadcastId?: string;

  private wt: WebTransport | null = null;
  private sender: DatagramSender | null = null;
  private media: BroadcastMediaSource | null = null;
  private mediaSource: BroadcastMediaSourceFactory;
  private encoder: Encoder | null = null;
  private stopping = false;

  // Ladder: gate + scale before encode. The encoder is recreated — not
  // reconfigured — whenever the preprocessed frames stop matching its
  // configured size, or a ladder change is flagged.
  private preprocessor = new FramePreprocessor();
  private ladderFps: FramerateSelection = 'native';
  private pendingEncoderReset = false;
  private encoderIniting = false;
  private encoderDims: { width: number; height: number } | null = null;
  private nativeFps: number | null = null;

  // Probe matrix state. The matrix is probed once at start (pre-capture 4K
  // upper bound) and refined from real frame dims — but only
  // upward (monotonic max): our own applyConstraints shrinks the frames, and
  // re-probing at constrained dims would feed the ceiling its own output
  // (constrain → smaller frames → "source is smaller" → different ceiling →
  // constrain…). A null prober (no WebCodecs in scope) keeps optimistic
  // defaults: ceiling native, auto fps 30.
  private prober: EncoderSupportProber | null;
  private matrix: SupportMatrix | null = null;
  private matrixGen = 0;
  private resolvedAutoFps: 60 | 30 = 30;
  private autoCeiling: ResolutionRung = 'native';
  private probedSourceDims: SourceDims | null = null;
  private lastConstraintsKey: string | null = null;

  // Automatic fallback. The controller is pure and timer-free; the pipeline
  // resolves its direction decisions against a per-source effective ladder.
  // Auto state is runtime-only — reset to the ceiling on every start and on
  // any resolution-selection change.
  private resolutionSelection: ResolutionSelection = 'auto';
  private encoderSettings: EncoderSettings = DEFAULT_ENCODER_SETTINGS;
  private controller = new FallbackController();
  private autoRungs: ResolutionRung[] | null = null;
  private autoIndex = 0;
  private autoSrcLongerDim: number | null = null;
  private now: () => number;

  private nextFrameId = 0;
  // Latest DecoderConfig datagram; re-sent immediately before every
  // keyframe so a viewer that missed it can always recover at the next
  // keyframe (the relay additionally caches and re-emits it).
  private configDatagram: Uint8Array<ArrayBuffer> | null = null;

  // Auto-resume state. resumeToken is minted by the relay (0x09) and
  // presented on every /publish/{id} claim; connGeneration invalidates
  // callbacks from a torn-down transport so a stale wt.closed can't disturb
  // its successor; the frameId counter deliberately survives resumes —
  // frameID continuity IS the viewer's resume-vs-restart signal.
  private resumeToken: string | null = null;
  // The fleet parity level, learned from RelayCapabilities. 0 until the
  // relay says otherwise, so an older relay (which sends nothing) and a fleet
  // configured off are the same code path — no parity is ever emitted.
  private parityLevel = 0;
  private connGeneration = 0;
  // The close code the relay said, in-band, it is about to
  // close session `gen` with. Chrome never reads the close code itself, so
  // handleSessionGone falls back to this when `closed` carries none.
  private closingNotice: { gen: number; code: number } | null = null;
  private reconnectAttempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  private stats: BroadcastStats = { ...EMPTY_BROADCAST_STATS };
  private lastStatsAt = 0;
  private encodedSinceStats = 0;
  private capturedSinceStats = 0;
  private sentSinceStats = 0;
  // Audio lane: strictly subordinate — its errors annotate, never fail the
  // broadcast.
  private audioLane: RunningAudioLane | null = null;
  private lastAudioEncoded = 0;
  private lastAudioSent = 0;
  private connSampler: ConnectionStatsSampler | null = null;
  private statsTimer: ReturnType<typeof setInterval> | null = null;

  // Relay clock sync + the ClockMapping publication. Frame
  // timestamps are already on this machine's performance.now() timeline
  // (capture.ts re-stamps at capture), so the TimeSync offset IS the mapping.
  private timeSync: TimeSyncClient | null = null;
  private clockMappingTimer: ReturnType<typeof setInterval> | null = null;
  private lastMappingSentAt: number | null = null;

  constructor(
    config: CaptureConfig,
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
    // Injectable clock (defaults to performance.now); the fallback
    // controller's decisions are time-based, so tests drive it
    // deterministically.
    now: () => number = () => performance.now(),
    // Where frames come from. Default is the main-thread
    // getDisplayMedia capture; the broadcast worker injects a source built
    // around a transferred track.
    mediaSource: BroadcastMediaSourceFactory = captureMediaSource,
    // Injectable for tests; null (no WebCodecs in scope)
    // disables the matrix and keeps optimistic defaults.
    prober?: EncoderSupportProber,
  ) {
    this.config = config;
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.broadcastId = broadcastId;
    this.now = now;
    this.mediaSource = mediaSource;
    this.prober = prober ?? (probeSupported() ? new EncoderSupportProber() : null);
    this.stats.pipelineContext = typeof window === 'undefined' ? 'worker' : 'main-thread';
  }

  async start(): Promise<void> {
    // Connect before consuming the screen capture: an unreachable server
    // fails the start before any frame is captured. The picker itself may
    // already be open — BroadcasterScreen requests the display stream inside
    // the start click, since Safari honours getDisplayMedia only from the
    // user-gesture handler, and that grant arrives here via mediaSource.
    this.resumeToken = this.connectOpts.resumeToken ?? null;
    try {
      await this.connectTransport();
    } catch (e) {
      // After stop(), a failed dial is not a start failure: the screen would
      // fall back from reclaim to minting a broadcast nobody owns.
      if (this.stopping) return;
      throw new BroadcastStartError('connect', e);
    }
    if (this.stopping) {
      // stop() raced the first dial, the same race tryResume guards:
      // teardown() ran with no session yet, so this one would be a zombie
      // publisher holding the broadcast ID, and it would go on to consume the
      // screen grant after the page gave up on it. stop() already fired
      // onEnded; resolve quietly, because a connect-phase rejection would make
      // the screen fall back from reclaim to a mint.
      this.teardownTransport();
      return;
    }
    // The mapping check runs on a 1s timer so the first mapping goes out
    // promptly after the first pong, then refreshes on the cadence. One
    // timer for the pipeline's life — it survives transport resumes.
    this.clockMappingTimer = setInterval(() => this.maybeSendClockMapping(), 1000);

    // Probe the support matrix before media starts, so the first encoder
    // init already resolves the auto ceiling / auto fps. Never throws; a
    // probe-less scope keeps the defaults.
    await this.refreshMatrix();
    // stop() during the probe: teardown() already closed the session, and
    // capturing now would own a stream nothing ever stops.
    if (this.stopping) return;

    try {
      await this.startMedia();
    } catch (e) {
      // The session is live; a leaked WebTransport here would be a zombie
      // publisher holding the broadcast ID until the tab closes.
      this.stopping = true;
      await this.teardown();
      throw new BroadcastStartError('capture', e);
    }
  }

  // The /publish dial URL. Auth crosses as query params — the WebTransport
  // JS API can't set headers: the publish secret, and for any
  // /publish/{id} claim the resume token (required by the relay).
  private publishUrl(): string {
    const path = this.broadcastId ? `/publish/${this.broadcastId}` : '/publish';
    const urlObj = new URL(path, this.serverUrl);
    if (this.connectOpts.publishSecret) {
      urlObj.searchParams.set('secret', this.connectOpts.publishSecret);
    }
    if (this.broadcastId && this.resumeToken) {
      urlObj.searchParams.set('resume', this.resumeToken);
    }
    return urlObj.toString();
  }

  // Establishes one WebTransport session and everything scoped to it: the
  // datagram sender, connection sampler, TimeSync client, closed-handler and
  // server-message reader. Called by start() and by every resume attempt.
  private async connectTransport(): Promise<void> {
    const wt = await connectWebTransport(this.publishUrl(), this.connectOpts);
    const gen = ++this.connGeneration;
    this.wt = wt;
    this.sender = new DatagramSender(wt);
    this.connSampler = new ConnectionStatsSampler(wt);
    void wt.closed
      .then((info) => {
        const closeInfo = info as { closeCode?: number } | undefined;
        this.handleSessionGone(gen, null, closeInfo?.closeCode ?? null);
      })
      .catch((e) => {
        const err = e instanceof Error ? e : new Error(String(e));
        this.handleSessionGone(gen, err, (e as { closeCode?: number })?.closeCode ?? null);
      });

    // The server-message read is detached: media flow must never wait on the
    // announce — only the UI code display and the resume token consume it.
    void this.readServerMessages(wt, gen);

    // Relay clock sync. Pings ride the ordinary datagram sender; the
    // read loop exists solely to catch replies (the relay sends the publisher
    // nothing else as datagrams). Failures are the session's problem, not the
    // ping loop's. Fresh estimator per session — the new pod has a new clock.
    const sender = this.sender;
    this.timeSync = new TimeSyncClient((d) => void sender.send([d]).catch(() => {}));
    this.timeSync.start();
    void readDatagrams(wt, (dgram) => {
      // The relay pushes the live viewer count on this session; everything
      // else stays TimeSync's.
      if (dgram.length === VIEWER_COUNT_SIZE && dgram[1] === TYPE_VIEWER_COUNT) {
        try {
          this.stats.viewerCount = parseViewerCount(dgram);
        } catch {
          // Malformed — drop it and keep the previous count.
        }
        return;
      }
      this.timeSync?.handleDatagram(dgram);
    }).catch(() => {});
  }

  // Reads server-initiated unidirectional streams for the session's life,
  // dispatching each message by wire type (BroadcastAnnounce 0x03,
  // ResumeToken 0x09, telemetry, relay capabilities) — arrival order is not
  // guaranteed.
  // Failures are logged, never fatal: the broadcast runs fine without the
  // code being displayed.
  private async readServerMessages(wt: WebTransport, gen: number): Promise<void> {
    try {
      const reader = wt.incomingUnidirectionalStreams.getReader();
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) return;
          if (value) void this.readServerMessage(value, gen);
        }
      } finally {
        reader.releaseLock();
      }
    } catch (e) {
      if (!this.stopping) log.warn('Server message stream ended:', e);
    }
  }

  private async readServerMessage(stream: ReadableStream<Uint8Array>, gen: number): Promise<void> {
    try {
      const chunks: Uint8Array[] = [];
      const streamReader = stream.getReader();
      try {
        while (true) {
          const { value, done } = await streamReader.read();
          if (done) break;
          if (value) chunks.push(value);
        }
      } finally {
        streamReader.releaseLock();
      }
      let totalLen = 0;
      for (const c of chunks) totalLen += c.length;
      const data = new Uint8Array(totalLen);
      let offset = 0;
      for (const c of chunks) {
        data.set(c, offset);
        offset += c.length;
      }
      const { msgType } = peekType(data);
      switch (msgType) {
        case TYPE_BROADCAST_ANNOUNCE: {
          const id = parseBroadcastAnnounce(data);
          // The pipeline keeps its own ID so a resume can target it even if
          // no UI ever consumed the announce.
          this.broadcastId = id;
          if (!this.stopping) this.cb.onBroadcastId?.(id);
          break;
        }
        case TYPE_RESUME_TOKEN: {
          this.resumeToken = bytesToHex(parseResumeToken(data));
          if (!this.stopping) this.cb.onResumeToken?.(this.resumeToken);
          break;
        }
        // This session's telemetry identity. Parsed here and handed straight
        // out — the pipeline never collects or sends anything itself
        // (collection lives on the main thread). An older relay sends no such
        // stream, which is why the callback is optional on both sides rather
        // than a required handshake step.
        case TYPE_TELEMETRY_HELLO: {
          const hello = parseTelemetryHello(data);
          if (!this.stopping) this.cb.onTelemetryHello?.(hello);
          break;
        }
        // Where this session's telemetry should go.
        // Same route as the hello (main-thread pipeline; the worker shell
        // forwards neither — collection begins only where a hello lands).
        case TYPE_TELEMETRY_ENDPOINT: {
          const url = parseTelemetryEndpoint(data);
          if (!this.stopping) this.cb.onTelemetryEndpoint?.(url);
          break;
        }
        // The fleet's parity level. An older relay sends no such stream, so
        // parityLevel stays 0 and this producer emits no parity chunks. A
        // relay configured off sends level 0 for the same effect, so the
        // fleet can be turned down from one chart value.
        case TYPE_RELAY_CAPABILITIES: {
          const caps = parseRelayCapabilities(data);
          this.parityLevel = caps.flags & CAP_PARITY_CHUNKS ? caps.parityLevel : 0;
          this.stats.parityLevel = this.parityLevel;
          break;
        }
        // The relay is about to close this session with this
        // code. Kept for handleSessionGone — the close itself arrives in
        // Chrome as a bare "Connection lost.".
        case TYPE_SESSION_CLOSING: {
          this.closingNotice = { gen, code: parseSessionClosing(data) };
          break;
        }
        default:
          log.warn(`Unexpected server uni-stream message type 0x${msgType.toString(16)}`);
      }
    } catch (e) {
      if (!this.stopping) log.warn('Server uni-stream message read failed:', e);
    }
  }

  // Live ladder change: takes effect on the next captured frame. Safe to
  // call any time (before start(), while running). Resolution changes are
  // caught by the dimension check; framerate-only changes need the explicit
  // reset flag because the frame size doesn't move (the encoder's
  // framerate/bitrate config must still follow the rung).
  //
  // The resolution axis is a ResolutionSelection. 'auto' walks the ladder on
  // its own; an explicit rung is honored unconditionally (never
  // auto-stepped). A resolution-selection change resets auto state to the
  // ceiling; a framerate-only change keeps the current auto rung.
  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void {
    const selectionChanged = selection !== this.resolutionSelection;
    this.resolutionSelection = selection;
    this.ladderFps = framerate;
    // Both 'auto' resolutions are matrix lookups — sync, no re-probe
    // (the matrix covers every fps rung).
    this.resolveFromMatrix();
    const fps = this.effectiveFpsRung();

    if (selection === 'auto') {
      if (selectionChanged) {
        // Switched into auto: restart optimistically at the ceiling.
        this.autoRungs = null;
        this.autoSrcLongerDim = null;
        this.autoIndex = 0;
        this.stats.autoAtFloor = false;
        this.stats.encoderPressure = false;
      }
      // The HW-aware ceiling (pre-frames), or the rung the auto ladder had
      // already settled on — a framerate-only change keeps it.
      this.preprocessor.setTarget(this.currentAutoRung() ?? this.autoCeiling, fps);
    } else {
      // Explicit rung: drives the preprocessor directly, forever.
      this.autoRungs = null;
      this.autoSrcLongerDim = null;
      this.autoIndex = 0;
      this.stats.autoAtFloor = false;
      this.stats.encoderPressure = false;
      this.preprocessor.setTarget(selection, fps);
    }

    this.pendingEncoderReset = true;
    // A live encoder means this call causes a real recreate; discard the
    // renegotiation churn and give the new config a fair evaluation window.
    // Before start() there is no encoder and nothing to reset — and a
    // startup cooldown would needlessly blind the controller to the first
    // seconds of the stream.
    if (this.encoder) this.controller.noteReset(this.now());
    this.syncCaptureConstraints();
  }

  // Advanced encoder settings — acceleration tri-state,
  // bitrate override, codec pin. Like setLadder, safe any time; takes
  // effect via encoder recreate on the next captured frame. A change resets
  // the fallback controller for the same reason a ladder change does: the
  // new encoder config deserves a fresh evaluation window.
  setEncoderSettings(settings: EncoderSettings): void {
    if (
      settings.hwPreference === this.encoderSettings.hwPreference &&
      settings.bitrateOverride === this.encoderSettings.bitrateOverride &&
      settings.codecOverride === this.encoderSettings.codecOverride
    ) {
      return;
    }
    const probeAxesChanged =
      settings.hwPreference !== this.encoderSettings.hwPreference ||
      settings.codecOverride !== this.encoderSettings.codecOverride;
    this.encoderSettings = settings;
    this.pendingEncoderReset = true;
    if (this.encoder) this.controller.noteReset(this.now());
    // Acceleration mode / codec pin shift what the matrix means — re-probe
    // and recompute the ceiling when it lands (bitrate never affects it).
    if (probeAxesChanged) void this.refreshMatrix(this.probedSourceDims ?? this.matrix?.source);
  }

  private currentAutoRung(): ResolutionRung | null {
    if (this.resolutionSelection !== 'auto') return null;
    if (this.autoRungs === null) return this.autoCeiling; // before the first frame
    return this.autoRungs[this.autoIndex];
  }

  // The effective framerate rung — 'auto' resolves to the matrix's
  // framerate-first answer (conservative 30 until probed).
  private effectiveFpsRung(): FramerateRung {
    return this.ladderFps === 'auto' ? this.resolvedAutoFps : this.ladderFps;
  }

  // The fps the ceiling is evaluated at. 'native' has no matrix column —
  // probe at 60, the upper bound (a rung must do HW at 60 to be the ceiling
  // for a native-fps stream).
  private ceilingProbeFps(): number {
    const fps = this.effectiveFpsRung();
    return fps === 'native' ? 60 : fps;
  }

  private resolveFromMatrix(): void {
    this.resolvedAutoFps = this.matrix ? resolveAutoFps(this.matrix.get) : 30;
    this.autoCeiling = this.matrix
      ? hardwareCeiling(this.matrix.get, this.ceilingProbeFps())
      : 'native';
  }

  // Probes (or re-probes) the matrix and recomputes the auto targets when it
  // lands. Awaited once in start() so the first encoder init sees it; later
  // refreshes are fire-and-forget with a generation guard. Never rejects.
  private async refreshMatrix(source?: SourceDims): Promise<void> {
    if (!this.prober || this.stopping) return;
    const gen = ++this.matrixGen;
    try {
      const matrix = await probeSupportMatrix(this.prober, {
        codecs: this.encoderSettings.codecOverride
          ? [this.encoderSettings.codecOverride]
          : this.config.codecPreferences,
        hwPreference: this.encoderSettings.hwPreference,
        ...(source ? { source } : {}),
      });
      if (gen !== this.matrixGen || this.stopping) return;
      this.matrix = matrix;
      this.recomputeAutoTargets();
    } catch (e) {
      log.warn('Support-matrix probe failed; keeping previous targets:', e);
    }
  }

  // Refines the matrix from real frame dimensions — upward only (see the
  // field comment: constrained capture must not feed the ceiling its own
  // output).
  private maybeRefineMatrix(width: number, height: number): void {
    if (!this.prober) return;
    if (
      this.probedSourceDims &&
      width * height <= this.probedSourceDims.width * this.probedSourceDims.height
    ) {
      return;
    }
    this.probedSourceDims = { width, height };
    void this.refreshMatrix({ width, height });
  }

  // Re-resolves fps + ceiling after a matrix change and re-anchors the auto
  // ladder at the new ceiling if anything effective moved. Explicit rungs
  // only react to the resolved fps (the ceiling never binds them).
  private recomputeAutoTargets(): void {
    const prevFps = this.effectiveFpsRung();
    const prevCeiling = this.autoCeiling;
    this.resolveFromMatrix();
    const fps = this.effectiveFpsRung();
    const fpsChanged = fps !== prevFps;
    const ceilingChanged = this.autoCeiling !== prevCeiling;

    if (this.resolutionSelection === 'auto' && (ceilingChanged || fpsChanged)) {
      if (this.autoSrcLongerDim !== null) {
        this.autoRungs = applyCeiling(
          autoLadder(this.autoSrcLongerDim),
          this.autoCeiling,
          this.autoSrcLongerDim,
        );
        this.autoIndex = 0;
        this.stats.autoAtFloor = false;
      }
      this.preprocessor.setTarget(this.currentAutoRung() ?? this.autoCeiling, fps);
      this.pendingEncoderReset = true;
      if (this.encoder) this.controller.noteReset(this.now());
    } else if (fpsChanged) {
      // Explicit resolution + 'auto' fps: the rung stays, the rate follows.
      this.preprocessor.setTarget(this.resolutionSelection as ResolutionRung, fps);
      this.pendingEncoderReset = true;
      if (this.encoder) this.controller.noteReset(this.now());
    }
    this.syncCaptureConstraints();
  }

  // Capture follows the *sticky* target — the explicit
  // rung, or the auto ceiling — never the current auto step (up-probes need
  // the higher-res source still flowing). Failures are non-fatal by
  // construction: the preprocessor keeps scaling whatever actually arrives.
  private syncCaptureConstraints(): void {
    const media = this.media;
    if (!media?.applyConstraints) return;
    const rung = this.resolutionSelection === 'auto' ? this.autoCeiling : this.resolutionSelection;
    const fps = this.effectiveFpsRung();
    const constraints: MediaTrackConstraints = {
      width: { max: rungCapWidth(rung) ?? this.config.width },
      ...(fps === 'native' ? {} : { frameRate: { max: fps } }),
    };
    const key = JSON.stringify(constraints);
    if (key === this.lastConstraintsKey) return;
    this.lastConstraintsKey = key;
    // Promise-wrapped so a synchronous throw is contained like a rejection.
    void Promise.resolve()
      .then(() => media.applyConstraints!(constraints))
      .catch((e) => {
        log.warn('applyConstraints rejected; the preprocessor remains the safety net:', e);
      });
  }

  private async startMedia(): Promise<void> {
    const media = await this.mediaSource(this.config);
    // stop() (or the session dying) while the picker was open already tore
    // down; nothing would ever stop a capture started now.
    if (this.stopping) {
      media.stop();
      return;
    }
    this.media = media;
    log.info('Capture path:', media.capturePath);
    this.cb.onCapturePathChosen(media.capturePath);
    // The worker-path source has no stream — the preview lives on the main
    // thread, and WorkerBroadcastSession fires onSourceStream there.
    if (media.stream) this.cb.onSourceStream(media.stream);

    this.nativeFps = media.nativeFps;

    // Initial capture alignment with the sticky target (explicit rung, or
    // the pre-capture auto ceiling); later matrix refinements and setting
    // changes re-sync as they land.
    this.syncCaptureConstraints();

    media.onEnded(() => {
      log.info('Capture source ended (user stopped sharing).');
      void this.stop();
    });

    this.startAudioLane(media);

    this.lastStatsAt = this.now();
    this.statsTimer = setInterval(() => this.publishStats(), 500);

    await media.startFrames((frame) => {
      if (this.stopping) {
        frame.close();
        return;
      }
      // Funnel stage 1: frames the capture path delivered, pre-gate.
      this.capturedSinceStats++;

      // Auto mode resolves against a per-source effective ladder; the source
      // (captured) dimensions determine which rungs actually shrink the
      // picture. Read them from the raw frame before preprocessing consumes it.
      if (this.resolutionSelection === 'auto') {
        this.updateAutoLadder(frame.displayWidth, frame.displayHeight);
      }

      const processed = this.preprocessor.process(frame);
      if (!processed) return; // fps gate drop; frame closed inside

      // Encoder no longer matches what we're sending (ladder change, or the
      // source itself changed dimensions): dispose and renegotiate from this
      // frame, exactly like first-frame startup.
      if (this.encoder && (this.pendingEncoderReset || this.dimsChanged(processed))) {
        log.info('Encoder reset: ladder or source dimensions changed.');
        this.encoder.dispose();
        this.encoder = null;
        this.encoderDims = null;
      }

      if (!this.encoder) {
        if (this.encoderIniting) {
          processed.close();
          this.stats.droppedFrames++;
          return;
        }
        this.initEncoder(processed);
        return;
      }

      const accepted = this.encoder.encode(processed);
      if (!accepted) this.stats.droppedFrames++;
      processed.close();

      // Feed every accept/reject outcome to the fallback controller and act
      // on its decision. fps-gate drops never reach here — they are not
      // encoder backpressure — so the ratio stays self-normalizing.
      this.applyDecision(this.controller.record(accepted, this.now()));
    });
  }

  // Starts the audio lane when audio was requested and the environment can
  // run it. Every non-active outcome is a
  // stats annotation, never an error — video-only is a first-class state.
  private startAudioLane(media: BroadcastMediaSource): void {
    if (!this.config.audio) return; // audioState stays 'off' — zero audio paths
    const track = media.audioTrack ?? null;
    if (!track) {
      if (media.audioUnavailable) {
        this.stats.audioState = 'unavailable';
        log.info(
          'Audio requested but this browser/OS could not start a system-audio source; broadcasting video-only.',
        );
        return;
      }
      this.stats.audioState = 'no-track';
      log.info('Audio requested but the grant carried no audio track; broadcasting video-only.');
      return;
    }
    if (!audioLaneSupported()) {
      this.stats.audioState = 'unsupported';
      log.info('Audio track present but AudioEncoder/MSTP unavailable here; broadcasting video-only.');
      return;
    }
    // Construction itself can throw (an ended track, a scope whose MSTP
    // rejects audio tracks). This runs inside startMedia(), so an escaping
    // throw would fail the whole broadcast with a capture-phase error. Audio
    // is strictly additive: it may annotate, never abort.
    try {
      this.audioLane = startAudioLane(track, {
        // Reads the live sender so the lane survives transport resumes
        // (packets during the gap reject and drop — live-edge, no buffering).
        send: (datagrams) => {
          const sender = this.sender;
          if (!sender) return Promise.reject(new Error('no transport'));
          return sender.send(datagrams);
        },
        onError: (err) => {
          // Audio-lane-only teardown: annotate and keep broadcasting video.
          this.stats.audioState = 'error';
          this.audioLane = null;
          log.warn('Audio lane failed; broadcast continues video-only:', err);
        },
      });
      this.stats.audioState = 'active';
    } catch (e) {
      this.stats.audioState = 'error';
      this.audioLane = null;
      log.warn('Audio lane could not start; broadcast continues video-only:', e);
    }
  }

  // Recomputes the auto ladder when the source dimensions first appear or
  // change (a window-share resize). A change resets to the ceiling and a
  // fresh baseline — rare, and better than guessing an equivalent index.
  // The ladder is sliced at the HW-aware ceiling, and real dims refine
  // the probe matrix (upward only — see maybeRefineMatrix).
  private updateAutoLadder(srcWidth: number, srcHeight: number): void {
    const srcLongerDim = Math.max(srcWidth, srcHeight);
    if (this.autoRungs === null) {
      this.autoRungs = applyCeiling(autoLadder(srcLongerDim), this.autoCeiling, srcLongerDim);
      this.autoSrcLongerDim = srcLongerDim;
      this.autoIndex = 0;
      this.preprocessor.setTarget(this.autoRungs[0], this.effectiveFpsRung());
      this.maybeRefineMatrix(srcWidth, srcHeight);
      return;
    }
    if (srcLongerDim === this.autoSrcLongerDim) return;
    this.autoRungs = applyCeiling(autoLadder(srcLongerDim), this.autoCeiling, srcLongerDim);
    this.autoSrcLongerDim = srcLongerDim;
    this.autoIndex = 0;
    this.stats.autoAtFloor = false;
    this.preprocessor.setTarget(this.autoRungs[0], this.effectiveFpsRung());
    this.pendingEncoderReset = true;
    this.controller.noteReset(this.now());
    this.maybeRefineMatrix(srcWidth, srcHeight);
  }

  // Acts on a controller decision. In auto mode it walks the ladder; in
  // explicit mode a would-be step-down only raises the passive pressure
  // warning — the rung is never touched.
  private applyDecision(decision: 'none' | 'stepDown' | 'stepUp'): void {
    if (decision === 'none') return;
    if (this.resolutionSelection !== 'auto') {
      this.stats.encoderPressure = decision === 'stepDown';
      return;
    }
    this.stepAuto(decision === 'stepDown' ? 1 : -1);
  }

  // Moves the auto ladder index by delta (+1 down a rung, -1 up), updates the
  // preprocessor and flags the encoder reset. A step demanded past the floor
  // or ceiling takes no action and latches the controller so it doesn't
  // re-fire every frame.
  private stepAuto(delta: 1 | -1): void {
    if (!this.autoRungs) return;
    const next = this.autoIndex + delta;
    if (next < 0) {
      this.controller.stepRejected('up'); // already at the ceiling
      return;
    }
    if (next >= this.autoRungs.length) {
      this.controller.stepRejected('down'); // already at the floor
      this.stats.autoAtFloor = true;
      return;
    }
    this.autoIndex = next;
    this.stats.autoAtFloor = false;
    if (delta > 0) this.stats.autoStepDowns++;
    else this.stats.autoStepUps++;
    // Auto steps are encode-only: no syncCaptureConstraints here — capture
    // stays at the sticky target.
    this.preprocessor.setTarget(this.autoRungs[this.autoIndex], this.effectiveFpsRung());
    this.pendingEncoderReset = true;
  }

  private dimsChanged(frame: VideoFrame): boolean {
    return (
      this.encoderDims !== null &&
      (this.encoderDims.width !== roundDownToEven(frame.displayWidth) ||
        this.encoderDims.height !== roundDownToEven(frame.displayHeight))
    );
  }

  // (Re)creates the encoder from an actual frame's dimensions — never
  // track.getSettings(). Consumes the frame:
  // it becomes the encoder's first input (a keyframe) on success.
  private initEncoder(firstFrame: VideoFrame): void {
    this.encoderIniting = true;
    this.pendingEncoderReset = false;
    log.info(
      `Configuring encoder from frame: display=${firstFrame.displayWidth}x${firstFrame.displayHeight}, coded=${firstFrame.codedWidth}x${firstFrame.codedHeight}`,
    );
    const width = roundDownToEven(firstFrame.displayWidth);
    const height = roundDownToEven(firstFrame.displayHeight);
    const fpsRung = this.effectiveFpsRung();
    const framerate = fpsRung === 'native' ? (this.nativeFps ?? this.config.framerate) : fpsRung;

    const proceedInit = async () => {
      // The advanced settings shape the negotiation — codec pin narrows the
      // preference walk to one, the bitrate override replaces the ladder
      // math, and the tri-state selects the variant cascade. Don't force-cap
      // >1080p to 30 fps here: the HW-aware auto ceiling covers the default
      // path, and an explicit high rung is honored (and merely annotated),
      // never silently capped.
      const settings = this.encoderSettings;
      const negotiatedConfig: CaptureConfig = {
        ...this.config,
        codecPreferences: settings.codecOverride
          ? [settings.codecOverride]
          : this.config.codecPreferences,
        hwPreference: settings.hwPreference,
        width,
        height,
        framerate,
        bitrate:
          settings.bitrateOverride !== null
            ? clampBitrateOverride(settings.bitrateOverride)
            : computeBitrate(width, height, framerate),
      };
      const enc = new Encoder(negotiatedConfig, {
        onEncoded: (encoded) => this.handleEncoded(encoded),
        onError: (e) => this.handleEncoderError(e),
      });
      try {
        const chosen = await enc.configure();
        if (this.stopping) {
          firstFrame.close();
          enc.dispose();
          return;
        }
        this.encoder = enc;
        this.encoderDims = { width, height };
        this.encoderIniting = false;
        // The one place the target is actually decided. Recorded from
        // what the encoder COMMITTED to, not from the settings that asked for
        // it — a rung can be refused, clamped or renegotiated, and telemetry
        // that reported the request would describe a stream that never ran.
        // Re-runs on every encoder recreate, so an auto step or a live
        // settings change moves the target with it.
        this.stats.targetWidth = chosen.width;
        this.stats.targetHeight = chosen.height;
        this.stats.targetFps = chosen.framerate;
        this.stats.targetBitrateBps = chosen.bitrate;
        this.stats.codec = chosen.codec;
        this.stats.acceleration = chosen.acceleration;
        this.cb.onEncoderConfigured(chosen);
        const accepted = enc.encode(firstFrame);
        if (!accepted) this.stats.droppedFrames++;
        firstFrame.close();
      } catch (e) {
        firstFrame.close();
        enc.dispose();
        this.encoderIniting = false;
        this.fail(e instanceof Error ? e : new Error(String(e)));
      }
    };
    void proceedInit();
  }

  private handleEncoded(encoded: EncodedFrame): void {
    if (this.stopping || !this.sender) return;
    const chunk = encoded.chunk;
    this.stats.encodedFrames++;
    this.encodedSinceStats++;
    if (chunk.type === 'key') this.stats.keyframes++;
    this.stats.lastEncodeLatencyMs = encoded.encodeEndMs - encoded.encodeStartMs;
    this.stats.encoderQueueDepth = this.encoder?.queueSize ?? 0;

    // The encoder attaches decoderConfig to the first chunk after configure
    // (and on parameter changes). Turn it into the wire config datagram.
    const decoderConfig = encoded.meta?.decoderConfig;
    if (decoderConfig?.codec) {
      try {
        this.configDatagram = packetizeDecoderConfig(decoderConfig.codec, decoderConfig.description);
      } catch (e) {
        this.fail(e instanceof Error ? e : new Error(String(e)));
        return;
      }
    }

    const data = new Uint8Array(chunk.byteLength);
    chunk.copyTo(data);
    // Wrap at uint32 like the wire encoding does, so the JS counter and the
    // on-wire frameId can never disagree (receivers compare ids serially).
    const frameId = this.nextFrameId;
    this.nextFrameId = nextFrameId(frameId);
    const timestampUs = BigInt(Math.round(chunk.timestamp));

    if (chunk.type === 'key') {
      // Keyframe → one reliable unidirectional stream carrying the current
      // config (embedded, so the keyframe is self-sufficient) + the payload.
      // A lost datagram cannot strand a keyframe for a whole GOP.
      let msg: Uint8Array<ArrayBuffer>;
      try {
        msg = packetizeStreamKeyframe(
          { frameId, timestampUs },
          this.configDatagram ?? new Uint8Array(0),
          data,
        );
      } catch (e) {
        this.fail(e instanceof Error ? e : new Error(String(e)));
        return;
      }
      if (this.configDatagram) this.stats.configsSent++;
      void this.sendKeyframeStream(msg);
      return;
    }

    // Delta → datagrams (fast, lossy; a loss costs one frame, not a GOP).
    // Plus up to parityLevel parity symbols, which turn "a loss costs one
    // frame" into "a loss costs nothing" for the common single-chunk case.
    let datagrams: Uint8Array<ArrayBuffer>[];
    let parity: Uint8Array<ArrayBuffer>[];
    try {
      ({ datagrams, parity } = packetizeFrameWithParity(
        { frameId, keyframe: false, timestampUs },
        data,
        this.parityLevel,
        this.wt?.datagrams.maxDatagramSize,
      ));
    } catch (e) {
      this.fail(e instanceof Error ? e : new Error(String(e)));
      return;
    }

    let bytes = 0;
    for (const d of datagrams) bytes += d.length;
    let parityBytes = 0;
    for (const d of parity) parityBytes += d.length;
    // Parity rides the same send as the data chunks and after them: it is
    // useless before the loss it repairs is known, and sending it first would
    // put it ahead of the frame in the queue for no benefit.
    const all = parity.length > 0 ? [...datagrams, ...parity] : datagrams;
    this.sender
      .send(all)
      .then(() => {
        this.stats.datagramsSent += datagrams.length;
        this.stats.bytesSent += bytes;
        this.stats.parityChunksSent += parity.length;
        this.stats.parityBytesSent += parityBytes;
        // Funnel stage 4: the whole frame actually left, without error.
        this.sentSinceStats++;
      })
      .catch(() => {
        // A send failure means the session is dying (or dead): drop the
        // frame and let wt.closed drive the state change — it is the one
        // signal that carries the close code, and auto-resume hangs off it.
        // Failing here would kill a resumable broadcast.
        if (!this.stopping) this.stats.droppedFrames++;
      });
  }

  // Sends one keyframe over a fresh unidirectional stream. A single stream
  // failure is not fatal — the next keyframe recovers, and genuine session
  // death is surfaced by the wt.closed handler — so it is logged and counted,
  // not propagated to fail().
  private async sendKeyframeStream(msg: Uint8Array<ArrayBuffer>): Promise<void> {
    const wt = this.wt;
    if (!wt || this.stopping) return;
    try {
      const stream = await wt.createUnidirectionalStream();
      const writer = stream.getWriter();
      await writer.write(msg);
      await writer.close();
      this.stats.keyframeStreamsSent++;
      this.stats.bytesSent += msg.length;
      this.stats.keyframeBytesSent += msg.length;
      this.sentSinceStats++;
    } catch (e) {
      if (!this.stopping) {
        this.stats.keyframeStreamsFailed++;
        log.warn('Keyframe stream send failed:', e);
      }
    }
  }

  // A mid-stream encoder error. In explicit mode it stays fatal (never
  // silently switch resolution against an explicit choice). In auto mode it
  // is the strongest backpressure evidence: step down one rung and recreate,
  // bounded — a second error inside the controller's window, or an error at
  // the floor, fails for real.
  private handleEncoderError(err: Error): void {
    if (this.resolutionSelection !== 'auto') {
      this.fail(err);
      return;
    }
    if (this.controller.onEncoderError(this.now()) === 'fail') {
      this.fail(err);
      return;
    }
    log.warn('Encoder error in auto mode; stepping down one rung and recreating:', err);
    this.encoder?.dispose();
    this.encoder = null;
    this.encoderDims = null;
    const before = this.autoIndex;
    this.stepAuto(1);
    if (this.autoIndex === before) {
      // Already at the floor — nothing lower to fall back to.
      this.fail(err);
    }
  }

  // Publishes relayClockUs = timestampUs + offsetUs to viewers (via the relay,
  // which caches it for late joiners). Re-sent every CLOCK_MAPPING_INTERVAL_MS
  // to track clock skew; nothing goes out until the first offset sample.
  private maybeSendClockMapping(): void {
    if (this.stopping || !this.sender) return;
    const sync = this.timeSync?.sample();
    if (!sync) return;
    const now = this.now();
    if (this.lastMappingSentAt !== null && now - this.lastMappingSentAt < CLOCK_MAPPING_INTERVAL_MS) {
      return;
    }
    this.lastMappingSentAt = now;
    this.sender.send([encodeClockMapping(sync.offsetUs)]).catch(() => {});
  }

  // The one authoritative session-death signal (wt.closed — datagram send
  // failures defer to it). Mid-broadcast, with an ID + resume token in hand,
  // the pipeline auto-resumes: capture and encoder stay alive (frames drop
  // while disconnected — live-edge, no buffering) and only the transport
  // reconnects. Anything earlier (no media yet, or the announce/token never
  // landed) fails.
  private handleSessionGone(gen: number, err: Error | null, closeCode: number | null): void {
    if (this.stopping || gen !== this.connGeneration) return;
    if (closeCode === null && this.closingNotice?.gen === gen) closeCode = this.closingNotice.code;
    // Terminal codes are checked BEFORE the resume branch: they are the cases
    // in which coming back is the wrong thing to do, not merely futile. 4004
    // only converges because the deposed session stays down, and 4006 means
    // the operator killed this broadcast and banned the ID for at least the
    // cooldown — every reclaim would collect a 451 whose status the browser
    // cannot even read, so the honest end is here, with a sentence saying
    // what happened.
    if (isTerminalPublisherClose(closeCode)) {
      log.info(`Relay ended this publisher session (code ${closeCode}). Not resuming.`);
      this.fail(new Error(terminalPublisherMessage(closeCode)));
      return;
    }
    if (this.media && this.broadcastId && this.resumeToken) {
      this.teardownTransport();
      this.scheduleResumeAttempt(closeCode, err?.message ?? 'session closed by server');
      return;
    }
    this.fail(err ?? new Error('WebTransport session closed by server'));
  }

  // Tears down everything scoped to the current WebTransport session,
  // leaving media/encoder untouched. Bumps the generation so stale session
  // callbacks are ignored.
  private teardownTransport(): void {
    this.connGeneration++;
    this.timeSync?.stop();
    this.timeSync = null;
    this.sender?.close();
    this.sender = null;
    this.connSampler = null;
    try {
      this.wt?.close();
    } catch {
      // already closed — fine
    }
    this.wt = null;
    // Re-publish the mapping promptly on the next session: the new pod's
    // hub cache starts empty, and viewers need it for absolute latency.
    this.lastMappingSentAt = null;
    // Parity is per relay: the next one states its own level, or none at all.
    this.parityLevel = 0;
    this.stats.parityLevel = 0;
  }

  private scheduleResumeAttempt(closeCode: number | null, reason: string): void {
    // stop() may race a dial that is already in flight; its failure must not
    // schedule anything (or surface errors) after onEnded.
    if (this.stopping) return;
    this.reconnectAttempt += 1;
    if (this.reconnectAttempt > RECONNECT_MAX_ATTEMPTS) {
      this.fail(new Error(`broadcast resume failed after ${RECONNECT_MAX_ATTEMPTS} attempts: ${reason}`));
      return;
    }
    // Same policy as the viewer (reconnect.ts): 4002 drain ⇒ now, abrupt ⇒
    // 250 ms, then the ladder. Dial failures re-enter at attempt 2+.
    const delayMs = reconnectDelayMs(this.reconnectAttempt, closeCode);
    log.info(
      `Broadcast resume attempt ${this.reconnectAttempt}/${RECONNECT_MAX_ATTEMPTS} in ${delayMs}ms (${reason})`,
    );
    this.cb.onReconnecting?.({ attempt: this.reconnectAttempt, delayMs, reason, closeCode });
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.tryResume();
    }, delayMs);
  }

  private async tryResume(): Promise<void> {
    if (this.stopping) return;
    try {
      await this.connectTransport();
    } catch (e) {
      this.scheduleResumeAttempt(null, e instanceof Error ? e.message : String(e));
      return;
    }
    if (this.stopping) {
      // stop() raced the dial: connectTransport just re-armed a live session
      // (wt, sender, TimeSync) after teardown() already ran. Release it — an
      // abandoned session is a zombie publisher holding the broadcast ID
      // hostage until the tab closes.
      this.teardownTransport();
      return;
    }
    this.reconnectAttempt = 0;
    // Prime the (possibly fresh) pod immediately: the next encoded frame
    // becomes a keyframe stream with the embedded config, so join-priming
    // works within ~RTT instead of up to one GOP. frameIDs continue — the
    // viewer sees an ordinary forward gap and recovers at this keyframe.
    this.encoder?.forceNextKeyframe();
    this.cb.onResumed?.();
  }

  private publishStats(): void {
    const now = this.now();
    const dt = (now - this.lastStatsAt) / 1000;
    if (dt > 0) {
      this.stats.encoderFps = this.encodedSinceStats / dt;
      this.stats.captureFps = this.capturedSinceStats / dt;
      this.stats.sentFps = this.sentSinceStats / dt;
    }
    this.encodedSinceStats = 0;
    this.capturedSinceStats = 0;
    this.sentSinceStats = 0;
    this.lastStatsAt = now;
    this.stats.fpsGateDropped = this.preprocessor.getStats().gateDropped;
    const audio = this.audioLane?.getStats();
    if (audio) {
      this.stats.audioEncodedPackets = audio.encodedPackets;
      this.stats.audioPacketsSent = audio.packetsSent;
      this.stats.audioBytesSent = audio.bytesSent;
      this.stats.audioConfigsSent = audio.configsSent;
      this.stats.audioSampleRate = audio.sampleRate;
      this.stats.audioChannels = audio.channels;
      this.stats.audioCodec = audio.codec;
      this.stats.audioBitrateBps = audio.bitrateBps;
      this.stats.audioEncodeLagMs = audio.encodeLagMs;
      this.stats.audioAnchorReanchors = audio.anchorReanchors;
      if (dt > 0) {
        this.stats.audioEncodedPerSec = (audio.encodedPackets - this.lastAudioEncoded) / dt;
        this.stats.audioSentPerSec = (audio.packetsSent - this.lastAudioSent) / dt;
      }
      this.lastAudioEncoded = audio.encodedPackets;
      this.lastAudioSent = audio.packetsSent;
    }
    this.stats.autoRung = this.currentAutoRung();
    this.stats.autoCeiling = this.resolutionSelection === 'auto' ? this.autoCeiling : null;
    this.stats.autoFps = this.ladderFps === 'auto' ? this.resolvedAutoFps : null;
    // Async refresh; the latest completed sample rides this (or the next) tick.
    this.connSampler?.tick();
    this.stats.connection = this.connSampler?.latest() ?? null;
    this.stats.timeSyncRttMs = this.timeSync?.sample()?.rttMs ?? null;
    this.cb.onStats({ ...this.stats });
  }

  private fail(err: Error): void {
    log.error('Broadcast pipeline error:', err);
    this.cb.onError(err);
    void this.stop();
  }

  async stop(): Promise<void> {
    if (this.stopping) return;
    this.stopping = true;
    await this.teardown();
    this.cb.onEnded();
  }

  // Releases everything start() acquired. Shared by stop() and start()'s own
  // failure path, which must not fire onEnded — the start() rejection is the
  // caller's error surface there.
  private async teardown(): Promise<void> {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.statsTimer !== null) {
      clearInterval(this.statsTimer);
      this.statsTimer = null;
    }
    if (this.clockMappingTimer !== null) {
      clearInterval(this.clockMappingTimer);
      this.clockMappingTimer = null;
    }
    this.timeSync?.stop();
    this.timeSync = null;

    this.audioLane?.stop();
    this.audioLane = null;
    // Dispose, not a flushing close: output is discarded once stopping, and a
    // wedged hardware encoder's flush would hold the publisher session open.
    this.encoder?.dispose();
    this.media?.stop();
    this.sender?.close();
    try {
      this.wt?.close();
    } catch {
      // already closed by the server — fine
    }

    this.encoder = null;
    this.media = null;
    this.sender = null;
    this.wt = null;
  }
}
