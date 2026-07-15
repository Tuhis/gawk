// Broadcaster pipeline: capture → encode → packetize → /publish datagrams.
// The capture/encode half mirrors media/loopback.ts (first-frame encoder
// negotiation and all); the decode half is replaced by the network.

import { log } from '../lib/logger';
import {
  startCapture,
  stopCapture,
  type BroadcastMediaSource,
  type BroadcastMediaSourceFactory,
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
import { connectWebTransport, DatagramSender, readDatagrams, type ConnectOptions } from './connection';
import { ConnectionStatsSampler, type TransportConnectionStats } from './net-stats';
import { packetizeDecoderConfig, packetizeFrame, packetizeStreamKeyframe } from './packetizer';
import { CLOCK_MAPPING_INTERVAL_MS, TimeSyncClient } from './time-sync';
import { encodeClockMapping, nextFrameId, parseBroadcastAnnounce } from './wire';

export interface BroadcastStats {
  encodedFrames: number;
  keyframes: number;
  droppedFrames: number;
  fpsGateDropped: number;
  datagramsSent: number;
  bytesSent: number;
  configsSent: number;
  // R8: keyframes travel over reliable uni streams, deltas over datagrams.
  keyframeStreamsSent: number;
  keyframeStreamsFailed: number;
  keyframeBytesSent: number;
  encoderQueueDepth: number;
  encoderFps: number;
  lastEncodeLatencyMs: number;
  // R9 funnel rates (docs/13 D5): capture → post-gate → encoded → sent.
  // A rate gap between two adjacent stages localizes the bottleneck to that
  // stage. captureFps counts frames delivered by the capture path *before*
  // the fps gate; sentFps counts frames whose bytes were actually handed to
  // the transport without error (the "actually sent framerate").
  captureFps: number;
  sentFps: number;
  // R9 connection health for this leg (broadcaster→relay); null when the
  // browser doesn't implement WebTransport.getStats().
  connection: TransportConnectionStats | null;
  // R5 Q2: self-owned broadcaster↔relay RTT from the TimeSync exchange —
  // works where getStats() doesn't (no browser ships it today — docs/13 D7).
  // Null until
  // the first ping/pong completes.
  timeSyncRttMs: number | null;
  // R4 automatic-fallback observability (docs/09). autoRung is the currently
  // applied ladder rung in auto mode, null in explicit mode. encoderPressure
  // is the explicit-mode passive warning: the encoder can't keep up but the
  // rung is held because the broadcaster chose it.
  autoRung: ResolutionRung | null;
  autoAtFloor: boolean;
  autoStepDowns: number;
  autoStepUps: number;
  encoderPressure: boolean;
  // R12 (docs/17): the HW-aware auto ceiling (null in explicit resolution
  // mode) and the resolved 'auto' framerate (null when the fps selection is
  // an explicit rung).
  autoCeiling: ResolutionRung | null;
  autoFps: number | null;
  // R11 (docs/16): where the pipeline runs, detected via `window` absence
  // (the viewer's R10 convention) — 'worker' on the offloaded path.
  pipelineContext: 'worker' | 'main-thread';
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
};

export interface BroadcastCallbacks {
  onSourceStream: (stream: MediaStream) => void;
  onEncoderConfigured: (info: EncoderConfigured) => void;
  onCapturePathChosen: (path: string) => void;
  onStats: (stats: BroadcastStats) => void;
  onError: (err: Error) => void;
  onEnded: () => void;
  onBroadcastId?: (id: string) => void;
}

function roundDownToEven(n: number): number {
  return n - (n % 2);
}

export type BroadcastStartPhase = 'connect' | 'capture';

// R12 (docs/17): the advanced encoder settings. hwPreference selects the
// variant cascade (Decision 5); bitrateOverride (bps, clamped by
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

// R11 (docs/16): the surface BroadcasterScreen drives. Implemented by
// BroadcastPipeline (main thread) and WorkerBroadcastSession (worker path),
// so the screen's reclaim/mint/error logic is path-agnostic.
export interface BroadcastSessionLike {
  start(): Promise<void>;
  stop(): Promise<void>;
  setLadder(selection: ResolutionSelection, framerate: FramerateRung): void;
  setEncoderSettings(settings: EncoderSettings): void;
}

// Default media source: the existing main-thread capture path, unchanged.
// Lives here (not capture.ts) so tests that mock '../media/capture' keep
// stubbing startCapture/stopCapture without also faking the adapter.
const captureMediaSource: BroadcastMediaSourceFactory = async (config) => {
  const handle = await startCapture(config);
  return {
    capturePath: handle.capturePath,
    stream: handle.stream,
    // Framerate is the one setting still taken from getSettings(): frames
    // don't carry a rate, and it only seeds the encoder's rate-control hint
    // when the framerate rung is 'native'.
    nativeFps: handle.track.getSettings().frameRate ?? null,
    onEnded: (cb) => handle.track.addEventListener('ended', cb),
    startFrames: (onFrame) => handle.startFrames(onFrame),
    stop: () => stopCapture(handle),
    ...(typeof handle.track.applyConstraints === 'function'
      ? { applyConstraints: (c: MediaTrackConstraints) => handle.track.applyConstraints(c) }
      : {}),
  };
};

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

  // R3 ladder (docs/08): gate + scale before encode. The encoder is
  // recreated — not reconfigured — whenever the preprocessed frames stop
  // matching its configured size, or a ladder change is flagged.
  private preprocessor = new FramePreprocessor();
  private ladderFps: FramerateSelection = 'native';
  private pendingEncoderReset = false;
  private encoderIniting = false;
  private encoderDims: { width: number; height: number } | null = null;
  private nativeFps: number | null = null;

  // R12 probe matrix state (docs/17). The matrix is probed once at start
  // (pre-capture 4K upper bound) and refined from real frame dims — but only
  // upward (monotonic max): our own applyConstraints shrinks the frames, and
  // re-probing at constrained dims would feed the ceiling its own output
  // (constrain → smaller frames → "source is smaller" → different ceiling →
  // constrain…). A null prober (no WebCodecs in scope) keeps the pre-R12
  // optimistic defaults: ceiling native, auto fps 30.
  private prober: EncoderSupportProber | null;
  private matrix: SupportMatrix | null = null;
  private matrixGen = 0;
  private resolvedAutoFps: 60 | 30 = 30;
  private autoCeiling: ResolutionRung = 'native';
  private probedSourceDims: SourceDims | null = null;
  private lastConstraintsKey: string | null = null;

  // R4 automatic fallback (docs/09). The controller is pure and timer-free;
  // the pipeline resolves its direction decisions against a per-source
  // effective ladder. Auto state is runtime-only — reset to the ceiling on
  // every start and on any resolution-selection change (Decision 6).
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

  private stats: BroadcastStats = { ...EMPTY_BROADCAST_STATS };
  private lastStatsAt = 0;
  private encodedSinceStats = 0;
  private capturedSinceStats = 0;
  private sentSinceStats = 0;
  private connSampler: ConnectionStatsSampler | null = null;
  private statsTimer: ReturnType<typeof setInterval> | null = null;

  // R5 Q2 (docs/16): relay clock sync + the ClockMapping publication. Frame
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
    // Injectable clock (defaults to performance.now); the R4 controller's
    // decisions are time-based, so tests drive it deterministically.
    now: () => number = () => performance.now(),
    // R11 (docs/16): where frames come from. Default is the main-thread
    // getDisplayMedia capture; the broadcast worker injects a source built
    // around a transferred track.
    mediaSource: BroadcastMediaSourceFactory = captureMediaSource,
    // R12 (docs/17): injectable for tests; null (no WebCodecs in scope)
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
    // Connect before prompting for screen capture: if the publisher slot is
    // taken (409) or the server is unreachable, fail without the share
    // picker ever appearing.
    const path = this.broadcastId ? `/publish/${this.broadcastId}` : '/publish';
    const urlObj = new URL(path, this.serverUrl);
    if (this.connectOpts.publishSecret) {
      urlObj.searchParams.set('secret', this.connectOpts.publishSecret);
    }
    const url = urlObj.toString();
    try {
      this.wt = await connectWebTransport(url, this.connectOpts);
    } catch (e) {
      throw new BroadcastStartError('connect', e);
    }
    this.sender = new DatagramSender(this.wt);
    this.connSampler = new ConnectionStatsSampler(this.wt);
    void this.wt.closed
      .then(() => this.handleSessionGone(null))
      .catch((e) => this.handleSessionGone(e instanceof Error ? e : new Error(String(e))));

    // The announce read is detached: media flow must never wait on the
    // announce — only the UI code display consumes it (docs/06).
    void this.readAnnounce(this.wt);

    // Relay clock sync (R5 Q2). Pings ride the ordinary datagram sender; the
    // read loop exists solely to catch replies (the relay sends the publisher
    // nothing else as datagrams). Failures are the session's problem, not the
    // ping loop's. The mapping check runs on a 1s timer so the first mapping
    // goes out promptly after the first pong, then refreshes on the cadence.
    const sender = this.sender;
    this.timeSync = new TimeSyncClient((d) => void sender.send([d]).catch(() => {}));
    this.timeSync.start();
    void readDatagrams(this.wt, (dgram) => {
      this.timeSync?.handleDatagram(dgram);
    }).catch(() => {});
    this.clockMappingTimer = setInterval(() => this.maybeSendClockMapping(), 1000);

    // R12: probe the support matrix before media starts, so the first
    // encoder init already resolves the auto ceiling / auto fps (docs/17
    // Decision 3). Never throws; a probe-less scope keeps the defaults.
    await this.refreshMatrix();

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

  // Reads the server's BroadcastAnnounce from the first server-initiated
  // unidirectional stream, buffering to EOF (the 9 bytes may arrive in
  // multiple chunks). Failures are logged, never fatal: the broadcast runs
  // fine without the code being displayed.
  private async readAnnounce(wt: WebTransport): Promise<void> {
    try {
      const reader = wt.incomingUnidirectionalStreams.getReader();
      let stream: ReadableStream<Uint8Array>;
      try {
        const { value, done } = await reader.read();
        if (done || !value) return;
        stream = value;
      } finally {
        reader.releaseLock();
      }
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
      const id = parseBroadcastAnnounce(data);
      if (!this.stopping) this.cb.onBroadcastId?.(id);
    } catch (e) {
      if (!this.stopping) log.warn('Broadcast announce read failed:', e);
    }
  }

  // Live ladder change: takes effect on the next captured frame. Safe to
  // call any time (before start(), while running). Resolution changes are
  // caught by the dimension check; framerate-only changes need the explicit
  // reset flag because the frame size doesn't move (the encoder's
  // framerate/bitrate config must still follow the rung).
  //
  // R4 (docs/09): the resolution axis is a ResolutionSelection. 'auto' walks
  // the ladder on its own; an explicit rung is honored unconditionally (never
  // auto-stepped). A resolution-selection change resets auto state to the
  // ceiling (Decision 6); a framerate-only change keeps the current auto rung.
  setLadder(selection: ResolutionSelection, framerate: FramerateSelection): void {
    const selectionChanged = selection !== this.resolutionSelection;
    this.resolutionSelection = selection;
    this.ladderFps = framerate;
    // Both R12 'auto' resolutions are matrix lookups — sync, no re-probe
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

  // R12 (docs/17): advanced encoder settings — acceleration tri-state,
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
    if (probeAxesChanged) void this.refreshMatrix(this.matrix?.source);
  }

  private currentAutoRung(): ResolutionRung | null {
    if (this.resolutionSelection !== 'auto') return null;
    if (this.autoRungs === null) return this.autoCeiling; // before the first frame
    return this.autoRungs[this.autoIndex];
  }

  // R12 (docs/17 Decision 4): the effective framerate rung — 'auto' resolves
  // to the matrix's framerate-first answer (conservative 30 until probed).
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

  // docs/17 Decision 6: capture follows the *sticky* target — the explicit
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
    this.media = media;
    log.info('Capture path:', media.capturePath);
    this.cb.onCapturePathChosen(media.capturePath);
    // The worker-path source has no stream — the preview lives on the main
    // thread, and WorkerBroadcastSession fires onSourceStream there.
    if (media.stream) this.cb.onSourceStream(media.stream);

    this.nativeFps = media.nativeFps;

    // Initial capture alignment with the sticky target (explicit rung, or
    // the pre-capture auto ceiling); later matrix refinements and setting
    // changes re-sync as they land (docs/17 Decision 6).
    this.syncCaptureConstraints();

    media.onEnded(() => {
      log.info('Capture source ended (user stopped sharing).');
      void this.stop();
    });

    this.lastStatsAt = this.now();
    this.statsTimer = setInterval(() => this.publishStats(), 500);

    await media.startFrames((frame) => {
      if (this.stopping) {
        frame.close();
        return;
      }
      // Funnel stage 1 (R9): frames the capture path delivered, pre-gate.
      this.capturedSinceStats++;

      // Auto mode resolves against a per-source effective ladder; the source
      // (captured) dimensions determine which rungs actually shrink the
      // picture. Read them from the raw frame before preprocessing consumes it.
      if (this.resolutionSelection === 'auto') {
        this.updateAutoLadder(frame.displayWidth, frame.displayHeight);
      }

      const processed = this.preprocessor.process(frame, this.nativeFps);
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
      // on its decision (R4, docs/09). fps-gate drops never reach here — they
      // are not encoder backpressure — so the ratio stays self-normalizing.
      this.applyDecision(this.controller.record(accepted, this.now()));
    });
  }

  // Recomputes the auto ladder when the source dimensions first appear or
  // change (a window-share resize). A change resets to the ceiling and a
  // fresh baseline — rare, and better than guessing an equivalent index.
  // R12: the ladder is sliced at the HW-aware ceiling, and real dims refine
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
  // warning (Decision 8) — the rung is never touched.
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
    // Auto steps are encode-only (docs/17 Decision 7): no
    // syncCaptureConstraints here — capture stays at the sticky target.
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
  // track.getSettings() (see docs/01-loopback-test.md). Consumes the frame:
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
      // R12: the advanced settings shape the negotiation — codec pin narrows
      // the preference walk to one, the bitrate override replaces the ladder
      // math, and the tri-state selects the variant cascade. The old
      // >1080p@>30 force-cap is gone (docs/17 Decision 10): the HW-aware
      // auto ceiling covers the default path, and an explicit high rung is
      // honored (and merely annotated), never silently capped.
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
      // A lost datagram can no longer strand a keyframe for a whole GOP.
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
    let datagrams: Uint8Array<ArrayBuffer>[];
    try {
      datagrams = packetizeFrame(
        { frameId, keyframe: false, timestampUs },
        data,
        this.wt?.datagrams.maxDatagramSize,
      );
    } catch (e) {
      this.fail(e instanceof Error ? e : new Error(String(e)));
      return;
    }

    let bytes = 0;
    for (const d of datagrams) bytes += d.length;
    this.sender
      .send(datagrams)
      .then(() => {
        this.stats.datagramsSent += datagrams.length;
        this.stats.bytesSent += bytes;
        // Funnel stage 4 (R9): the whole frame actually left, without error.
        this.sentSinceStats++;
      })
      .catch((e) => {
        if (!this.stopping) this.fail(e instanceof Error ? e : new Error(String(e)));
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

  // A mid-stream encoder error. In explicit mode it stays fatal (silently
  // switching resolution against an explicit choice is exactly what Decision 4
  // rules out). In auto mode it is the strongest backpressure evidence: step
  // down one rung and recreate, bounded — a second error inside the controller's
  // window, or an error at the floor, fails for real (Decision 7).
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

  private handleSessionGone(err: Error | null): void {
    if (this.stopping) return;
    this.fail(err ?? new Error('WebTransport session closed by server'));
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

    if (this.encoder) await this.encoder.close();
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
