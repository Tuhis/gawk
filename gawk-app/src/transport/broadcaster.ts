// Broadcaster pipeline: capture → encode → packetize → /publish datagrams.
// The capture/encode half mirrors media/loopback.ts (first-frame encoder
// negotiation and all); the decode half is replaced by the network.

import { log } from '../lib/logger';
import { startCapture, stopCapture, type CaptureHandle } from '../media/capture';
import { Encoder, probeHardwareSupport, type EncodedFrame, type EncoderConfigured } from '../media/encoder';
import {
  autoLadder,
  computeBitrate,
  type FramerateRung,
  type ResolutionRung,
  type ResolutionSelection,
} from '../media/ladder';
import { FallbackController } from '../media/fallback';
import { FramePreprocessor } from '../media/preprocess';
import type { CaptureConfig } from '../media/types';
import { connectWebTransport, DatagramSender, type ConnectOptions } from './connection';
import { ConnectionStatsSampler, type TransportConnectionStats } from './net-stats';
import { packetizeDecoderConfig, packetizeFrame, packetizeStreamKeyframe } from './packetizer';
import { nextFrameId, parseBroadcastAnnounce } from './wire';

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
  // R4 automatic-fallback observability (docs/09). autoRung is the currently
  // applied ladder rung in auto mode, null in explicit mode. encoderPressure
  // is the explicit-mode passive warning: the encoder can't keep up but the
  // rung is held because the broadcaster chose it.
  autoRung: ResolutionRung | null;
  autoAtFloor: boolean;
  autoStepDowns: number;
  autoStepUps: number;
  encoderPressure: boolean;
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
  autoRung: null,
  autoAtFloor: false,
  autoStepDowns: 0,
  autoStepUps: 0,
  encoderPressure: false,
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
  private capture: CaptureHandle | null = null;
  private encoder: Encoder | null = null;
  private stopping = false;

  // R3 ladder (docs/08): gate + scale before encode. The encoder is
  // recreated — not reconfigured — whenever the preprocessed frames stop
  // matching its configured size, or a ladder change is flagged.
  private preprocessor = new FramePreprocessor();
  private ladderFps: FramerateRung = 'native';
  private pendingEncoderReset = false;
  private encoderIniting = false;
  private encoderDims: { width: number; height: number } | null = null;
  private nativeFps: number | null = null;

  // R4 automatic fallback (docs/09). The controller is pure and timer-free;
  // the pipeline resolves its direction decisions against a per-source
  // effective ladder. Auto state is runtime-only — reset to the ceiling on
  // every start and on any resolution-selection change (Decision 6).
  private resolutionSelection: ResolutionSelection = 'auto';
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
  private statsTimer: number | null = null;

  constructor(
    config: CaptureConfig,
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
    // Injectable clock (defaults to performance.now); the R4 controller's
    // decisions are time-based, so tests drive it deterministically.
    now: () => number = () => performance.now(),
  ) {
    this.config = config;
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.broadcastId = broadcastId;
    this.now = now;
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
  setLadder(selection: ResolutionSelection, framerate: FramerateRung): void {
    const selectionChanged = selection !== this.resolutionSelection;
    this.resolutionSelection = selection;
    this.ladderFps = framerate;

    if (selection === 'auto') {
      if (selectionChanged) {
        // Switched into auto: restart optimistically at the ceiling.
        this.autoRungs = null;
        this.autoSrcLongerDim = null;
        this.autoIndex = 0;
        this.stats.autoAtFloor = false;
        this.stats.encoderPressure = false;
      }
      // 'native' is always the ceiling; a framerate-only change keeps the
      // rung the auto ladder had already settled on.
      this.preprocessor.setTarget(this.currentAutoRung() ?? 'native', framerate);
    } else {
      // Explicit rung: drives the preprocessor directly, forever.
      this.autoRungs = null;
      this.autoSrcLongerDim = null;
      this.autoIndex = 0;
      this.stats.autoAtFloor = false;
      this.stats.encoderPressure = false;
      this.preprocessor.setTarget(selection, framerate);
    }

    this.pendingEncoderReset = true;
    // A live encoder means this call causes a real recreate; discard the
    // renegotiation churn and give the new config a fair evaluation window.
    // Before start() there is no encoder and nothing to reset — and a
    // startup cooldown would needlessly blind the controller to the first
    // seconds of the stream.
    if (this.encoder) this.controller.noteReset(this.now());
  }

  private currentAutoRung(): ResolutionRung | null {
    if (this.resolutionSelection !== 'auto') return null;
    if (this.autoRungs === null) return 'native'; // ceiling, before the first frame
    return this.autoRungs[this.autoIndex];
  }

  private async startMedia(): Promise<void> {
    this.capture = await startCapture(this.config);
    log.info('Capture path:', this.capture.capturePath);
    this.cb.onCapturePathChosen(this.capture.capturePath);
    this.cb.onSourceStream(this.capture.stream);

    // Framerate is the one setting still taken from getSettings(): frames
    // don't carry a rate, and it only seeds the encoder's rate-control hint
    // when the framerate rung is 'native'.
    this.nativeFps = this.capture.track.getSettings().frameRate ?? null;

    this.capture.track.addEventListener('ended', () => {
      log.info('Capture track ended (user stopped sharing).');
      void this.stop();
    });

    this.lastStatsAt = this.now();
    this.statsTimer = window.setInterval(() => this.publishStats(), 500);

    await this.capture.startFrames((frame) => {
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
        this.updateAutoLadder(Math.max(frame.displayWidth, frame.displayHeight));
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
  private updateAutoLadder(srcLongerDim: number): void {
    if (this.autoRungs === null) {
      this.autoRungs = autoLadder(srcLongerDim);
      this.autoSrcLongerDim = srcLongerDim;
      this.autoIndex = 0;
      return;
    }
    if (srcLongerDim === this.autoSrcLongerDim) return;
    this.autoRungs = autoLadder(srcLongerDim);
    this.autoSrcLongerDim = srcLongerDim;
    this.autoIndex = 0;
    this.stats.autoAtFloor = false;
    this.preprocessor.setTarget('native', this.ladderFps);
    this.pendingEncoderReset = true;
    this.controller.noteReset(this.now());
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
    this.preprocessor.setTarget(this.autoRungs[this.autoIndex], this.ladderFps);
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
    let framerate = this.ladderFps === 'native' ? (this.nativeFps ?? this.config.framerate) : this.ladderFps;

    const proceedInit = async () => {
      let cappedFps: number | null = null;
      if ((width > 1920 || height > 1080) && framerate > 30) {
        const hwSupported = await probeHardwareSupport(
          this.config.codecPreferences,
          width,
          height,
          framerate,
        );
        if (!hwSupported) {
          log.info(
            `HW encoding not supported for encoder target ${width}x${height}@${framerate}fps. Capping to 30fps.`,
          );
          framerate = 30;
          cappedFps = 30;
        }
      }
      this.preprocessor.setCappedFps(cappedFps);

      const negotiatedConfig: CaptureConfig = {
        ...this.config,
        width,
        height,
        framerate,
        bitrate: computeBitrate(width, height, framerate),
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
    // Async refresh; the latest completed sample rides this (or the next) tick.
    this.connSampler?.tick();
    this.stats.connection = this.connSampler?.latest() ?? null;
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

    if (this.encoder) await this.encoder.close();
    if (this.capture) stopCapture(this.capture);
    this.sender?.close();
    try {
      this.wt?.close();
    } catch {
      // already closed by the server — fine
    }

    this.encoder = null;
    this.capture = null;
    this.sender = null;
    this.wt = null;
  }
}
