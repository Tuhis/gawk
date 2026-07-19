// Viewer pipeline: /subscribe datagrams → reassemble → decode → callback.
// The decode half mirrors media/loopback.ts (ordered decoder chain); the
// encode half is replaced by the network.

import { log } from '../lib/logger';
import { Decoder, type DecodedFrame } from '../media/decoder';
import {
  AudioDecodeLane,
  audioDecodeSupported,
  type DecodedAudioChunk,
} from './audio-decode';
import type { ConnectOptions, KeyframeStreamFrame } from './connection';
import type { TransportConnectionStats } from './net-stats';
import {
  LocalViewerTransport,
  type TransportClosedInfo,
  type ViewerTransport,
  type ViewerTransportFactory,
  type ViewerTransportKind,
} from './viewer-transport';
import { getInterpolationEnabled } from './interpolation';
import { LiveEdgeTracker } from './live-edge';
import {
  getPlayoutMode,
  getPlayoutOffsetMs,
  resetPlayoutController,
  updatePlayoutController,
  type PlayoutMode,
} from './playout';
import { Reassembler, type ReassemblerStats } from './reassembler';
import { timeOriginMs } from './time-sync';
import type { FeatureGate, PresentationSurfaceStats } from '../lib/featureGates';
import type { TeeStats } from './tee-render-sink';
import { ReorderBuffer, type ReleasedFrame, type ReorderStats } from './reorder-buffer';
import type { RenderSink, RenderSinkKind } from './render-sink';
import type { DecoderConfigMessage } from './wire';
import { getMaxDecoderQueueSize } from '../config';

// How often the reorder buffer is advanced (its bounded waits and the
// decoder-backpressure resync are time-based). ~1 frame at 60 fps.
const REORDER_TICK_MS = 16;

export interface ViewerStats extends ReassemblerStats {
  decodedFrames: number;
  decoderQueueDepth: number;
  decoderFps: number;
  configsApplied: number;
  framesDiscardedAwaitingKey: number;
  lastDecodeLatencyMs: number;
  isHardwareAccelerated: boolean | null;
  // Dimensions of the newest decoded frame — the stream as actually decoded
  // (trust the VideoFrame in hand, not metadata — docs/01). Mid-stream ladder
  // changes (R3/R4) update on the next decoded frame. Null before the first.
  frameWidth: number | null;
  frameHeight: number | null;
  // R8 keyframe-stream + reorder observability.
  keyframeStreamsReceived: number;
  reorderGapResyncs: number;
  reorderKeyframeWaitDrops: number;
  reorderBuffered: number;
  // R9 funnel + stall indicators (docs/13 D5): received → decoded → rendered.
  // receivedFps counts complete frames arriving (reassembled datagram frames
  // + keyframe streams); renderedFps comes from the RenderSink and is null on
  // the main-thread path (the screen draws there, not the pipeline).
  receivedFps: number;
  renderedFps: number | null;
  // Which sink paints (R10, docs/14): 'webgl' | '2d' on the worker path, null
  // on the main-thread path. Note renderedFps is rAF-coalesced since R10 —
  // ≈min(decoded fps, display Hz); below decoded fps under load is healthy.
  renderer: RenderSinkKind | null;
  // Where things actually run (R10, docs/14) — ground truth, not intent, so
  // a silently-degraded fallback is visible in the overlay / diagnostics.
  // pipelineContext: is this pipeline in the viewer worker or the main-thread
  // fallback? transport: are the read loops in the nested transport worker or
  // in-process next to decode?
  pipelineContext: 'worker' | 'main-thread';
  transport: ViewerTransportKind | null;
  // Time since the last complete frame arrived (stall detector) and since
  // the last keyframe (recovery bound: should hover at or under the GOP).
  timeSinceLastFrameMs: number | null;
  lastKeyframeAgeMs: number | null;
  // R5 Q1 (docs/15): how far the newest decoded frame lags behind this
  // session's best capture→decode delta (windowed min — clock offset cancels).
  // ~0 = at live edge; growth = decoder backlog / reorder holds / queue
  // growth. Null before the first decoded frame. Relative only — absolute
  // capture→render latency is capToRenderMs (Q2).
  liveEdgeDriftMs: number | null;
  // R5 Q2 (docs/15): absolute capture→render latency via the relay clock as
  // the common reference (broadcaster ClockMapping + this leg's TimeSync
  // offset). Error ≈ sum of both legs' best-sample rtt/2 asymmetries; a
  // negative raw value (asymmetry pathology) is clamped to 0. Null until both
  // clock legs have synced. The render paint follows the measurement point by
  // at most one display interval.
  capToRenderMs: number | null;
  // R5 Q2: self-owned relay↔viewer RTT from the TimeSync exchange — works
  // where WebTransport.getStats() doesn't (no browser ships it today —
  // Chromium removed its pre-spec impl in 152; see docs/13 D7).
  timeSyncRttMs: number | null;
  // R5 Q3: the active playout offset — 0 = live-edge (default), >0 = the
  // opt-in smoothed mode. Ground truth from the context the pipeline runs in,
  // so a toggle that failed to cross the worker boundary is visible.
  playoutOffsetMs: number;
  // R12 T2: the playout mode itself ('off' | 'fixed' | 'adaptive') and where
  // presentation happens — paced on rAF, paced on the timer fallback, or
  // immediate (live-edge/fixed). Null presentation = main-thread path (the
  // screen draws there, not the pipeline).
  playoutMode: PlayoutMode;
  presentation: 'paced-raf' | 'paced-timer' | 'immediate' | null;
  // R12 T4: experimental frame interpolation — 'on'/'off' when available
  // (WebGL2 sink + adaptive mode), null when this pipeline can't offer it
  // (main-thread path, non-WebGL2 sink, or playout not adaptive). The menu
  // shows the toggle only when non-null.
  interpolation: 'on' | 'off' | null;
  // R12 T1 (docs/17): jitter, per stats window. Render cadence = how much the
  // paint intervals deviate from the frames' capture intervals (σ + p95 of
  // |err|; ≡0 for perfect pacing at any fps) — worker path only, null on the
  // main-thread path like renderedFps. Arrival jitter = windowed p95 − min of
  // the reorder buffer's arrival delta. Decode jitter = σ of per-frame decode
  // time. These are the numbers T2/T3's pacing must move.
  renderCadenceStdDevMs: number | null;
  renderCadenceP95Ms: number | null;
  arrivalJitterMs: number | null;
  decodeJitterMs: number | null;
  // R9 connection health for this leg (relay→viewer); null when the browser
  // doesn't implement WebTransport.getStats().
  connection: TransportConnectionStats | null;
  // Cumulative video bytes this pipeline received itself — datagram payloads
  // plus whole keyframe StreamFrame messages, mirroring the broadcaster's
  // bytesSent. The overlay's "Video bitrate (recv)" is derived from this
  // counter because WebTransport.getStats() ships in no browser (docs/13 D7).
  // Undercounts wire truth: no QUIC/UDP overhead, lost datagrams invisible.
  videoBytesReceived: number;
  // R19 (docs/24 Decision 10): how deltas actually arrive. 'datagrams' is
  // the default mode; 'reliable' means carrier streams are observed;
  // 'reliable-requested' is the Decision 8 degradation — reliable was
  // requested but no carrier has appeared (old relay, or none rotated in
  // yet), so buffering is resilient while delivery stays datagrams.
  deliveryMode: 'datagrams' | 'reliable' | 'reliable-requested';
  // R18 (docs/23 Decision 8): the live "N watching" number the relay fans
  // out (~1 s cadence; the fleet-global total in cluster mode). Null until
  // the first push — usually the join-prime — lands.
  viewerCount: number | null;
  // R19 carrier tallies from the transport (null before connect / where the
  // transport can't report them).
  carrierStreams: number | null;
  carrierRecords: number | null;
  // Carriers ending in a reset — the relay shedding a stalled/superseded GOP
  // tail; each costs at most one resync at the next keyframe.
  carrierStreamsAborted: number | null;
  // R15 (docs/20): the audio lane, as observed by this pipeline. audioPresent
  // flips true on the first AudioConfig/packet and is what gates every piece
  // of viewer audio UI — a video-only stream renders exactly today's viewer.
  // 'unsupported' means audio arrived but this scope has no AudioDecoder;
  // 'error' means the lane died (video plays on).
  audioState: 'absent' | 'active' | 'unsupported' | 'error';
  audioPacketsReceived: number;
  audioPacketsDecoded: number;
  audioBytesReceived: number;
  audioCodec: string | null;
  audioSampleRate: number | null;
  audioChannels: number | null;
  // R16 (docs/21 Decision 9). The pipeline itself never sets these three:
  // presentationTee is merged in by the viewer *worker shell* when the
  // presentation tee exists (gated devices only); featureGates and
  // presentationSurface are attached on the main thread by the viewer screen
  // before stats reach the overlay / Copy diagnostics.
  presentationTee?: TeeStats;
  featureGates?: FeatureGate[];
  presentationSurface?: PresentationSurfaceStats;
}

export interface ViewerCallbacks {
  onDecodedFrame: (decoded: DecodedFrame) => void;
  onConfig: (config: DecoderConfigMessage) => void;
  onStats: (stats: ViewerStats) => void;
  onError: (err: Error) => void;
  onEnded: () => void;
  // R15 (docs/20 Decision 7): decoded planar PCM, headed for the main-thread
  // AudioWorklet sink. On the worker path the host transfers it across; on
  // the main-thread path it goes straight to the sink. Absent callback = the
  // consumer doesn't do audio, and the lane is never built.
  onAudioChunk?: (chunk: DecodedAudioChunk) => void;
  // Broadcaster restart / resync: the sink must flush and re-anchor, or every
  // packet on the new timeline is late forever (docs/20 Decision 8).
  onAudioReset?: () => void;
}

export class ViewerPipeline {
  private serverUrl: string;
  private connectOpts: ConnectOptions;
  private cb: ViewerCallbacks;
  // When set (worker/OffscreenCanvas path, R8 S6), decoded frames are drawn
  // straight to the sink and closed there — never handed to onDecodedFrame,
  // so a VideoFrame never crosses the worker boundary. Null on the main-thread
  // path, where onDecodedFrame draws and the caller closes the frame.
  private renderSink: RenderSink | null;

  // Connection + read loops live behind the transport seam (R10 P3): local
  // (in-process) by default, or proxied to a dedicated transport worker.
  private transport: ViewerTransport | null = null;
  private transportFactory: ViewerTransportFactory;
  private decoder: Decoder | null = null;
  private reassembler: Reassembler | null = null;
  // Merges reliable stream keyframes with lossy datagram deltas by frameId and
  // owns the freeze-on-gap / drop-to-keyframe ordering policy (R8, docs/12).
  private reorder: ReorderBuffer | null = null;
  private reorderTimer: number | null = null;
  private stopping = false;

  // Decoder ops chain so configure completes before any decode and decodes
  // stay in arrival order — same discipline as the loopback pipeline.
  private decoderChain: Promise<void> = Promise.resolve();
  // WebCodecs requires the first chunk after configure() to be a keyframe;
  // set on every (re)configure, cleared by the first keyframe. This is the
  // decoder-level guard; cross-frame ordering lives in the reorder buffer.
  private waitingForKeyframe = true;
  // Dedup key of the last applied config: the broadcaster embeds the config in
  // every keyframe stream, so we reconfigure only when it actually changes.
  private lastConfigKey: string | null = null;
  private keyframeStreamsReceived = 0;

  private decodedFrames = 0;
  private decodedSinceStats = 0;
  private configsApplied = 0;
  private framesDiscardedAwaitingKey = 0;
  private lastDecodeLatencyMs = 0;
  private lastFrameWidth: number | null = null;
  private lastFrameHeight: number | null = null;
  private lastStatsAt = 0;
  private statsTimer: number | null = null;
  // R9 funnel + stall tracking.
  private lastReceivedTotal = 0;
  private lastRenderedTotal = 0;
  private lastFrameReceivedAt: number | null = null;
  private lastKeyframeReceivedAt: number | null = null;
  private videoBytesReceived = 0;
  // The codec of the last applied config — for a clear "can't decode" message.
  private lastCodec: string | null = null;
  private pendingConfig: DecoderConfigMessage | null = null;
  private preferSoftware = false;
  private lastConfigMessage: DecoderConfigMessage | null = null;
  private pendingDecodes = 0;
  // Detected, not configured: `window` exists only on the main thread, so
  // this is ground truth about where the pipeline actually runs.
  private pipelineContext: 'worker' | 'main-thread' =
    typeof window === 'undefined' ? 'worker' : 'main-thread';
  // R5 Q1: drift over the session-best capture→decode delta, observed at
  // decoder output (the paint that follows is ≤ one display interval later —
  // a constant the windowed-min baseline cancels).
  private liveEdge = new LiveEdgeTracker();
  // R5 Q2: the broadcaster's timestamp→relay-clock mapping (last one wins;
  // invalidated by a broadcaster restart) and the newest absolute latency.
  private broadcastClockOffsetUs: bigint | null = null;
  private lastCapToRenderMs: number | null = null;
  // R18: the relay's latest "N watching" push (last one wins).
  private viewerCount: number | null = null;
  // R15 (docs/20): the audio lane. Built lazily on the first audio message —
  // a video-only stream never constructs it, so nothing about this pipeline
  // changes for broadcasts without audio.
  private audioLane: AudioDecodeLane | null = null;
  private audioState: ViewerStats['audioState'] = 'absent';
  // R12 T1: per-stats-window decode latencies (σ published as decodeJitterMs).
  private decodeLatencies: number[] = [];

  private broadcastId: string;

  constructor(
    serverUrl: string,
    broadcastId: string,
    connectOpts: ConnectOptions,
    callbacks: ViewerCallbacks,
    renderSink: RenderSink | null = null,
    transportFactory: ViewerTransportFactory = (url, opts) => new LocalViewerTransport(url, opts),
  ) {
    this.serverUrl = serverUrl;
    this.broadcastId = broadcastId;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.renderSink = renderSink;
    this.transportFactory = transportFactory;
  }

  async start(): Promise<void> {
    // Pipeline stages exist before the transport connects: the relay primes a
    // joining viewer with the cached keyframe immediately, and that must land
    // in the reorder buffer, not race the setup.
    this.decoder = new Decoder({
      onDecoded: (decoded) => this.handleDecoded(decoded),
      // A decoder error (unsupported codec, decode failure) is not recoverable
      // by reconnecting — mark it fatal so the session surfaces it and stops.
      onError: (e) => this.failDecode(e),
    });

    this.reorder = new ReorderBuffer(
      (frame) => this.decodeReleased(frame),
      () => performance.now(),
      // Broadcaster restart: timestamps move to a new timeline, so the
      // drift baseline must rebuild against it.
      { onRestart: () => this.handleBroadcasterRestart() },
    );

    this.reassembler = new Reassembler({
      // A datagram-borne config (legacy path) still applies; keyframes now
      // carry their own config on the stream.
      onConfig: (config) => this.maybeApplyConfig(config),
      // R5 Q2: the broadcaster's clock mapping — relayed live and replayed to
      // late joiners by the relay's cache.
      onClockMapping: (offsetUs) => {
        this.broadcastClockOffsetUs = offsetUs;
      },
      // R18: the relay's live viewer count (join-primed, then ~1 s cadence).
      onSubscriberCount: (count) => {
        this.viewerCount = count;
      },
      // R15 (docs/20 Decision 7): the audio lane's demux points. Both are
      // no-ops without an onAudioChunk consumer, so a viewer that can't play
      // audio never builds a decoder.
      onAudioConfig: (config) => {
        const lane = this.ensureAudioLane();
        lane?.configure(config);
      },
      onAudioFrame: (packet) => {
        const lane = this.ensureAudioLane();
        lane?.push(packet);
      },
      // Reassembled datagram frames feed the reorder buffer. Keyframes only
      // arrive over streams in practice, but routing a keyframe-flagged
      // datagram here too keeps the viewer robust to any keyframe source.
      onFrame: (frame) => {
        if (!this.reorder) return;
        this.lastFrameReceivedAt = performance.now();
        if (frame.keyframe) {
          this.lastKeyframeReceivedAt = this.lastFrameReceivedAt;
          this.reorder.pushKeyframe({
            frameId: frame.frameId,
            timestampUs: frame.timestampUs,
            config: null,
            data: frame.data,
          });
        } else {
          this.reorder.pushDelta({
            frameId: frame.frameId,
            timestampUs: frame.timestampUs,
            data: frame.data,
          });
        }
      },
    });

    const url = new URL(`/subscribe/${this.broadcastId}`, this.serverUrl);
    // R19 (docs/24 Decision 6): reliable delivery is negotiated at subscribe
    // time via the query param — the WebTransport JS API can't set headers.
    if (this.connectOpts.deliveryMode === 'reliable') {
      url.searchParams.set('delivery', 'reliable');
    }
    const transport = this.transportFactory(url.toString(), this.connectOpts);
    this.transport = transport;
    try {
      await transport.connect({
        onDatagram: (dgram) => {
          if (this.stopping) return;
          this.videoBytesReceived += dgram.byteLength;
          this.reassembler?.push(dgram);
        },
        onKeyframe: (kf) => this.handleKeyframeStream(kf),
        onClosed: (info) => this.handleClosed(info),
      });
    } catch (e) {
      // Release what start() acquired: a never-connected failure is surfaced
      // to the caller (ViewerSession treats it as fatal), not via onError.
      const decoder = this.decoder;
      this.decoder = null;
      this.reassembler = null;
      this.reorder = null;
      this.transport = null;
      transport.close();
      if (decoder) void decoder.close();
      throw e;
    }

    this.lastStatsAt = performance.now();
    // Bare setInterval (not window.*) so the pipeline runs unchanged inside a
    // Web Worker (R8 S6), where `window` is undefined.
    this.statsTimer = setInterval(() => this.publishStats(), 500) as unknown as number;
    this.reorderTimer = setInterval(() => this.reorderTick(), REORDER_TICK_MS) as unknown as number;
  }

  // R15: builds the audio lane on first use. Returns null when this viewer
  // has no audio consumer (main-thread paths that don't render audio) or the
  // scope lacks AudioDecoder — both annotate and keep video untouched.
  private ensureAudioLane(): AudioDecodeLane | null {
    if (this.audioLane) return this.audioLane;
    if (this.stopping || !this.cb.onAudioChunk) return null;
    if (!audioDecodeSupported()) {
      this.audioState = 'unsupported';
      return null;
    }
    this.audioLane = new AudioDecodeLane({
      onChunk: (chunk) => this.cb.onAudioChunk?.(chunk),
      onError: (err) => {
        log.warn('Audio decode lane failed; the stream plays video-only:', err);
        this.audioState = 'error';
        this.audioLane = null;
      },
    });
    this.audioState = 'active';
    return this.audioLane;
  }

  private handleKeyframeStream(kf: KeyframeStreamFrame): void {
    if (this.stopping || !this.reorder) return;
    // Keyframes bypass the datagram reassembler, so sync its late-delta
    // watermark here — this is what makes a broadcaster restart (frameIds
    // reset to 0) recover instead of dropping every new-session delta as
    // late (R10 field finding, docs/14).
    this.reassembler?.noteStreamKeyframe(kf.frameId);
    this.keyframeStreamsReceived++;
    this.videoBytesReceived += kf.streamBytes;
    this.lastFrameReceivedAt = performance.now();
    this.lastKeyframeReceivedAt = this.lastFrameReceivedAt;
    this.reorder.pushKeyframe({
      frameId: kf.frameId,
      timestampUs: kf.timestampUs,
      config: kf.config,
      data: kf.data,
    });
  }

  private reorderTick(): void {
    if (this.stopping || !this.reorder) return;
    // Decoder backpressure: if the decode queue is deep, stop feeding it and
    // resync at the next keyframe so the viewer catches up to live.
    const dec = this.decoder;
    if (dec && (dec.queueSize + this.pendingDecodes) > getMaxDecoderQueueSize()) {
      this.reorder.requestResync();
      // The resync jumps ahead; frames held for pacing are already superseded.
      this.renderSink?.flush?.();
    }
    this.reorder.tick();
  }

  // The transport reports the session end exactly once (the wt.closed vs
  // read-loop settle-order race lives inside the transport impl — see
  // LocalViewerTransport). Only the close code carries semantics: 4000 =
  // broadcast ended, which ViewerSession treats as terminal.
  private handleClosed(info: TransportClosedInfo): void {
    if (this.stopping) return;
    const err = new Error(info.message) as Error & { closeCode?: number };
    if (info.closeCode !== undefined) {
      err.closeCode = info.closeCode;
    }
    this.fail(err);
  }

  // Dedups: the broadcaster embeds the config in every keyframe stream, so we
  // reconfigure the decoder only when the codec/extradata actually change.
  private maybeApplyConfig(config: DecoderConfigMessage): void {
    const key = `${config.codec}:${Array.from(config.extradata).join(',')}`;
    if (key === this.lastConfigKey) return;
    this.lastConfigKey = key;
    this.applyConfig(config);
  }

  private applyConfig(config: DecoderConfigMessage): void {
    this.pendingConfig = config;
    this.lastConfigMessage = config;
    this.lastCodec = config.codec;
    this.waitingForKeyframe = true;
    this.cb.onConfig(config);
  }

  // Called by the reorder buffer in decode order. A keyframe may carry a config
  // (stream-embedded); apply it before decoding that keyframe.
  private decodeReleased(frame: ReleasedFrame): void {
    if (frame.config) this.maybeApplyConfig(frame.config);
    this.feedDecoder(frame.keyframe, frame.timestampUs, frame.data);
  }

  private feedDecoder(keyframe: boolean, timestampUs: bigint, data: Uint8Array): void {
    const dec = this.decoder;
    if (!dec || this.stopping) return;

    const totalQueueSize = this.pendingDecodes + dec.queueSize;
    if (totalQueueSize > getMaxDecoderQueueSize()) {
      if (this.reorder) this.reorder.requestResync();
      this.renderSink?.flush?.();
      // Drop this frame unless it is a keyframe (we need keyframes to recover)
      // Actually, viewer-level waitingForKeyframe will also drop deltas if true.
      this.waitingForKeyframe = true;
    }

    if (this.pendingConfig) {
      const config = this.pendingConfig;
      this.pendingConfig = null;

      // Annex-B H.264 stream starts with a start code prefix: 0x00000001 (4 bytes) or 0x000001 (3 bytes).
      const isAnnexB =
        data.length >= 3 &&
        data[0] === 0x00 &&
        data[1] === 0x00 &&
        (data[2] === 0x01 || (data.length >= 4 && data[2] === 0x00 && data[3] === 0x01));
      const isAvcc =
        !isAnnexB &&
        config.codec.startsWith('avc1') &&
        config.extradata.length > 0 &&
        config.extradata[0] === 0x01;

      let codec = config.codec;
      let extradata = config.extradata;
      
      if (isAvcc && config.extradata.length >= 4) {
        extradata = normalizeAvccExtradata(config.extradata);
        const profile = extradata[1].toString(16).toUpperCase().padStart(2, '0');
        const compat = extradata[2].toString(16).toUpperCase().padStart(2, '0');
        const level = extradata[3].toString(16).toUpperCase().padStart(2, '0');
        const extracted = `avc1.${profile}${compat}${level}`;
        if (extracted.toLowerCase() !== codec.toLowerCase()) {
          log.info(
            `Codec mismatch detected: negotiated ${codec}, actual in extradata ${extracted}. Using actual.`,
          );
          codec = extracted;
        }
      }

      const decoderConfig: VideoDecoderConfig = {
        codec,
        ...(isAvcc ? { description: extradata.slice() } : {}),
        optimizeForLatency: true,
        ...(this.preferSoftware ? { hardwareAcceleration: 'prefer-software' } : {}),
      };

      const extradataHex = Array.from(extradata)
        .map((b) => b.toString(16).padStart(2, '0'))
        .join(' ');
      const frameStartHex = Array.from(data.subarray(0, Math.min(16, data.length)))
        .map((b) => b.toString(16).padStart(2, '0'))
        .join(' ');

      log.info(
        'Applying decoder config:',
        codec,
        `(${extradata.length}B extradata, detected format: ${isAnnexB ? 'Annex-B' : 'AVCC'}${
          this.preferSoftware ? ', SW fallback' : ''
        })`,
      );
      log.info(`Extradata hex: ${extradataHex}`);
      log.info(`Frame start hex: ${frameStartHex}`);

      this.lastCodec = codec;
      this.configsApplied++;
      this.chainDecoderOp(() => dec.configure(decoderConfig));
    }

    if (this.waitingForKeyframe && !keyframe) {
      this.framesDiscardedAwaitingKey++;
      return;
    }
    if (keyframe) this.waitingForKeyframe = false;
    const chunk = new EncodedVideoChunk({
      type: keyframe ? 'key' : 'delta',
      timestamp: Number(timestampUs),
      data,
    });
    this.pendingDecodes++;
    this.chainDecoderOp(() => {
      this.pendingDecodes--;
      if (!this.stopping) dec.decode(chunk);
    });
  }

  private chainDecoderOp(op: () => void | Promise<void>): void {
    this.decoderChain = this.decoderChain.then(op);
    this.decoderChain = this.decoderChain.catch((e) => {
      // configure()/decode() rejections land here — a codec/decode failure.
      this.failDecode(e instanceof Error ? e : new Error(String(e)));
    });
  }

  // A keyframe serially behind the decode position is the broadcaster-restart
  // signal (docs/14): the new session's frame timestamps live on a fresh
  // clock timeline, invalidating anything derived from the old one — the
  // drift baseline AND the clock mapping (the new session's mapping arrives
  // with its first re-send / relay prime).
  private handleBroadcasterRestart(): void {
    this.liveEdge.reset();
    this.broadcastClockOffsetUs = null;
    this.lastCapToRenderMs = null;
    // R15 (docs/20 Decision 8): audio timestamps live on the same restarted
    // timeline — the sink must drop its queue and re-anchor, or every new
    // packet reads as older than the playhead and is late-dropped forever.
    this.cb.onAudioReset?.();
    // Held paced frames are from the old timeline — their targets are junk,
    // and so is any adaptive offset learned against it.
    this.renderSink?.flush?.();
    resetPlayoutController();
  }

  private handleDecoded(decoded: DecodedFrame): void {
    this.decodedFrames++;
    this.decodedSinceStats++;
    this.lastDecodeLatencyMs = decoded.decodeEndMs - decoded.decodeStartMs;
    this.decodeLatencies.push(this.lastDecodeLatencyMs);
    if (decoded.frame.displayWidth) {
      this.lastFrameWidth = decoded.frame.displayWidth;
      this.lastFrameHeight = decoded.frame.displayHeight;
    }
    this.liveEdge.observe(decoded.frame.timestamp);
    this.observeCapToRender(decoded.frame.timestamp);
    // Worker path: draw + close in place so the frame never crosses a boundary.
    // Main-thread path: hand it to the callback, which draws and closes it.
    if (this.renderSink) {
      this.renderSink.draw(decoded.frame, this.displayTargetMs(decoded.frame.timestamp));
    } else {
      this.cb.onDecodedFrame(decoded);
    }
  }

  // R12 T2: in adaptive mode, each decoded frame's display slot —
  // timestamp + arrival baseline + offset, the same schedule the reorder
  // buffer released it (DECODE_LEAD_MS early) against. Undefined everywhere
  // else ⇒ the sink presents ASAP, exactly the pre-R12 behavior.
  private displayTargetMs(timestampUs: number): number | undefined {
    if (getPlayoutMode() !== 'adaptive') return undefined;
    const base = this.reorder?.arrivalBaselineMs();
    const offset = getPlayoutOffsetMs();
    if (base == null || offset <= 0) return undefined;
    return timestampUs / 1000 + base + offset;
  }

  // R5 Q2: absolute capture→render, both sides translated to the relay clock:
  //   (viewerNow + viewerOffset) − (frame.timestamp + broadcastOffset)
  // Needs both clock legs; until then it stays null. Negative raw values
  // (asymmetry error exceeding the true latency) clamp to 0.
  private observeCapToRender(timestampUs: number): void {
    const sync = this.transport?.sampleTimeSync();
    if (!sync || this.broadcastClockOffsetUs === null) return;
    // The sample's offset maps the MEASURING context's performance.now() to
    // the relay clock, and that context can be the nested transport worker —
    // whose timeOrigin is its own creation moment, minutes after this
    // worker's once a reconnect has spawned a fresh one mid-view. Rebase this
    // context's now onto the sample's timeline first, or the metric inflates
    // by the age gap between the two workers.
    const nowOnSampleClockMs = timeOriginMs() + performance.now() - sync.timeOriginMs;
    const nowU = BigInt(Math.round(nowOnSampleClockMs * 1000));
    const raw =
      Number(nowU + sync.offsetUs - (BigInt(Math.round(timestampUs)) + this.broadcastClockOffsetUs)) /
      1000;
    this.lastCapToRenderMs = Math.max(0, raw);
  }

  private publishStats(): void {
    const now = performance.now();
    const dt = (now - this.lastStatsAt) / 1000;
    const decoderFps = dt > 0 ? this.decodedSinceStats / dt : 0;
    this.decodedSinceStats = 0;
    this.lastStatsAt = now;
    const reasm = this.reassembler?.getStats();
    const reorder: ReorderStats | undefined = this.reorder?.getStats();

    // R9 funnel: complete frames arriving per second (datagram-reassembled +
    // stream keyframes), and — on the worker path — frames actually drawn.
    const receivedTotal = (reasm?.framesCompleted ?? 0) + this.keyframeStreamsReceived;
    const receivedFps = dt > 0 ? (receivedTotal - this.lastReceivedTotal) / dt : 0;
    this.lastReceivedTotal = receivedTotal;
    let renderedFps: number | null = null;
    if (this.renderSink) {
      const drawn = this.renderSink.drawnFrames();
      renderedFps = dt > 0 ? (drawn - this.lastRenderedTotal) / dt : 0;
      this.lastRenderedTotal = drawn;
    }
    // R12 T1: this window's jitter numbers.
    const cadence = this.renderSink?.drainCadence?.() ?? null;
    const arrivalJitterMs = this.reorder?.arrivalJitterMs() ?? null;
    // R12 T3: the adaptive offset controller reads the same jitter estimate
    // the overlay shows (no-op outside adaptive mode).
    updatePlayoutController(arrivalJitterMs, now);
    const lats = this.decodeLatencies;
    this.decodeLatencies = [];
    let decodeJitterMs: number | null = null;
    if (lats.length >= 2) {
      const mean = lats.reduce((a, b) => a + b, 0) / lats.length;
      decodeJitterMs = Math.sqrt(
        lats.reduce((a, b) => a + (b - mean) * (b - mean), 0) / lats.length,
      );
    }
    // R19 delivery-mode ground truth (docs/24 Decisions 8/10): requested is
    // what this pipeline asked for; carriers observed is what the relay
    // actually serves.
    const carrier = this.transport?.sampleCarrierStats?.() ?? null;
    const audioStats = this.audioLane?.getStats() ?? null;
    const deliveryMode: ViewerStats['deliveryMode'] =
      this.connectOpts.deliveryMode === 'reliable'
        ? carrier && carrier.streamsOpened > 0
          ? 'reliable'
          : 'reliable-requested'
        : 'datagrams';
    this.cb.onStats({
      datagramsReceived: reasm?.datagramsReceived ?? 0,
      badDatagrams: reasm?.badDatagrams ?? 0,
      duplicateChunks: reasm?.duplicateChunks ?? 0,
      duplicateConfigs: reasm?.duplicateConfigs ?? 0,
      framesCompleted: reasm?.framesCompleted ?? 0,
      framesDroppedIncomplete: reasm?.framesDroppedIncomplete ?? 0,
      framesDroppedLate: reasm?.framesDroppedLate ?? 0,
      decodedFrames: this.decodedFrames,
      decoderQueueDepth: (this.decoder?.queueSize ?? 0) + this.pendingDecodes,
      decoderFps,
      configsApplied: this.configsApplied,
      framesDiscardedAwaitingKey: this.framesDiscardedAwaitingKey,
      lastDecodeLatencyMs: this.lastDecodeLatencyMs,
      isHardwareAccelerated: this.decoder?.isHardwareAccelerated ?? null,
      frameWidth: this.lastFrameWidth,
      frameHeight: this.lastFrameHeight,
      keyframeStreamsReceived: this.keyframeStreamsReceived,
      reorderGapResyncs: reorder?.gapResyncs ?? 0,
      reorderKeyframeWaitDrops: reorder?.keyframeWaitDrops ?? 0,
      reorderBuffered: reorder?.buffered ?? 0,
      receivedFps,
      renderedFps,
      renderer: this.renderSink?.kind ?? null,
      pipelineContext: this.pipelineContext,
      transport: this.transport?.kind ?? null,
      timeSinceLastFrameMs: this.lastFrameReceivedAt === null ? null : now - this.lastFrameReceivedAt,
      lastKeyframeAgeMs: this.lastKeyframeReceivedAt === null ? null : now - this.lastKeyframeReceivedAt,
      liveEdgeDriftMs: this.liveEdge.driftMs(),
      capToRenderMs: this.lastCapToRenderMs,
      timeSyncRttMs: this.transport?.sampleTimeSync()?.rttMs ?? null,
      playoutOffsetMs: getPlayoutOffsetMs(),
      playoutMode: getPlayoutMode(),
      presentation: this.renderSink
        ? getPlayoutMode() === 'adaptive'
          ? this.renderSink.scheduleKind === 'timer'
            ? 'paced-timer'
            : 'paced-raf'
          : 'immediate'
        : null,
      interpolation:
        this.renderSink?.supportsInterpolation && getPlayoutMode() === 'adaptive'
          ? getInterpolationEnabled()
            ? 'on'
            : 'off'
          : null,
      renderCadenceStdDevMs: cadence?.stdDevMs ?? null,
      renderCadenceP95Ms: cadence?.p95Ms ?? null,
      arrivalJitterMs,
      decodeJitterMs,
      connection: this.transport?.sampleConnectionStats() ?? null,
      videoBytesReceived: this.videoBytesReceived,
      viewerCount: this.viewerCount,
      deliveryMode,
      carrierStreams: carrier?.streamsOpened ?? null,
      carrierRecords: carrier?.recordsReceived ?? null,
      carrierStreamsAborted: carrier?.streamsAborted ?? null,
      audioState: this.audioState,
      audioPacketsReceived: reasm?.audioPacketsReceived ?? 0,
      audioPacketsDecoded: audioStats?.packetsDecoded ?? 0,
      audioBytesReceived: reasm?.audioBytesReceived ?? 0,
      audioCodec: audioStats?.codec ?? null,
      audioSampleRate: audioStats?.sampleRate ?? null,
      audioChannels: audioStats?.channels ?? null,
    });
  }

  private fail(err: Error): void {
    log.error('Viewer pipeline error:', err);
    this.cb.onError(err);
    void this.stop();
  }

  // A decoder/codec failure: reconnecting re-feeds the same unplayable stream
  // and fails identically, so this is marked `fatal` — ViewerSession surfaces
  // it to the user and stops instead of looping. Guarded so the decoder's
  // error callback and the configure() rejection can't double-report.
  // A decoder/codec failure: we try to fall back to software-based decoding first.
  // If it still fails, it's marked fatal — ViewerSession surfaces it to the user.
  private failDecode(err: Error): void {
    if (this.stopping) return;

    if (!this.preferSoftware) {
      log.warn('Decode error encountered; trying software decoder fallback:', err.message);
      this.preferSoftware = true;

      // Dispose old decoder
      if (this.decoder) {
        const oldDecoder = this.decoder;
        this.decoder = null;
        void oldDecoder.close();
      }

      // Recreate decoder
      this.decoder = new Decoder({
        onDecoded: (decoded) => this.handleDecoded(decoded),
        onError: (e) => this.failDecode(e),
      });

      // Reset decoder queue/chain
      this.decoderChain = Promise.resolve();
      this.waitingForKeyframe = true;

      // Re-trigger configuration
      if (this.lastConfigMessage) {
        this.pendingConfig = this.lastConfigMessage;
      }
      return;
    }

    log.error('Viewer decode error:', err);
    const codec = this.lastCodec ?? 'unknown';
    const fatal = new Error(
      `Can't play this stream — your browser can't decode its video (codec ${codec}).`,
    ) as Error & { fatal?: boolean };
    fatal.fatal = true;
    this.cb.onError(fatal);
    void this.stop();
  }

  async stop(): Promise<void> {
    if (this.stopping) return;
    this.stopping = true;

    if (this.statsTimer !== null) {
      clearInterval(this.statsTimer);
      this.statsTimer = null;
    }
    if (this.reorderTimer !== null) {
      clearInterval(this.reorderTimer);
      this.reorderTimer = null;
    }
    this.renderSink?.flush?.();
    this.transport?.close();
    this.audioLane?.stop();

    if (this.decoder) await this.decoder.close();

    this.decoder = null;
    this.reassembler = null;
    this.reorder = null;
    this.transport = null;
    this.audioLane = null;

    this.cb.onEnded();
  }
}

function normalizeAvccExtradata(extradata: Uint8Array): Uint8Array {
  if (extradata.length < 7 || extradata[0] !== 0x01) return extradata;

  const out: number[] = [];
  
  // Bytes 0-3: Version, Profile, Compat, Level
  out.push(extradata[0], extradata[1], extradata[2], extradata[3]);

  // Byte 4: Fix reserved bits (set top 6 bits to 1, Chrome strictly requires this)
  out.push(extradata[4] | 0xfc);

  // Byte 5: Fix reserved bits (set top 3 bits to 1)
  const numOfSps = extradata[5] & 0x1f;
  out.push(numOfSps | 0xe0);

  let offset = 6;
  let spsWasBuggy = false;
  
  // Parse SPS
  for (let i = 0; i < numOfSps; i++) {
    if (offset + 2 > extradata.length) break;
    let len = (extradata[offset] << 8) | extradata[offset + 1];
    offset += 2;
    if (offset + len > extradata.length) break;
    
    const originalLen = len;
    let naluData = extradata.subarray(offset, offset + len);
    
    // Detect Firefox double-byte bug: NALU type (e.g. 0x67) is duplicated, 
    // shifting the true profile_idc to index 2.
    if (naluData.length > 2 && (naluData[0] & 0x1f) === 7) {
      if (naluData[0] === naluData[1] && naluData[2] === extradata[1]) {
        log.warn('Normalizing Firefox SPS double-byte bug');
        naluData = naluData.subarray(1);
        len -= 1;
        spsWasBuggy = true;
      }
    }
    
    out.push((len >> 8) & 0xff, len & 0xff);
    for (let j = 0; j < len; j++) out.push(naluData[j]);
    offset += originalLen;
  }

  // Parse PPS
  if (offset < extradata.length) {
    const numOfPps = extradata[offset++];
    out.push(numOfPps);
    
    for (let i = 0; i < numOfPps; i++) {
      if (offset + 2 > extradata.length) break;
      let len = (extradata[offset] << 8) | extradata[offset + 1];
      offset += 2;
      if (offset + len > extradata.length) break;
      
      const originalLen = len;
      let naluData = extradata.subarray(offset, offset + len);
      
      // Fix PPS double-byte bug if SPS was buggy
      if (naluData.length > 2 && (naluData[0] & 0x1f) === 8) {
        if (spsWasBuggy && naluData[0] === naluData[1]) {
          log.warn('Normalizing Firefox PPS double-byte bug');
          naluData = naluData.subarray(1);
          len -= 1;
        }
      }
      
      out.push((len >> 8) & 0xff, len & 0xff);
      for (let j = 0; j < len; j++) out.push(naluData[j]);
      offset += originalLen;
    }
  }

  return new Uint8Array(out);
}
