// Viewer pipeline: /subscribe datagrams → reassemble → decode → callback.
// The decode half mirrors media/loopback.ts (ordered decoder chain); the
// encode half is replaced by the network.

import { log } from '../lib/logger';
import { Decoder, type DecodedFrame } from '../media/decoder';
import type { ConnectOptions, KeyframeStreamFrame } from './connection';
import type { TransportConnectionStats } from './net-stats';
import {
  LocalViewerTransport,
  type TransportClosedInfo,
  type ViewerTransport,
  type ViewerTransportFactory,
  type ViewerTransportKind,
} from './viewer-transport';
import { LiveEdgeTracker } from './live-edge';
import { getPlayoutOffsetMs } from './playout';
import { Reassembler, type ReassemblerStats } from './reassembler';
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
  // R9 connection health for this leg (relay→viewer); null when the browser
  // doesn't implement WebTransport.getStats().
  connection: TransportConnectionStats | null;
}

export interface ViewerCallbacks {
  onDecodedFrame: (decoded: DecodedFrame) => void;
  onConfig: (config: DecoderConfigMessage) => void;
  onStats: (stats: ViewerStats) => void;
  onError: (err: Error) => void;
  onEnded: () => void;
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
  private lastStatsAt = 0;
  private statsTimer: number | null = null;
  // R9 funnel + stall tracking.
  private lastReceivedTotal = 0;
  private lastRenderedTotal = 0;
  private lastFrameReceivedAt: number | null = null;
  private lastKeyframeReceivedAt: number | null = null;
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

    const url = new URL(`/subscribe/${this.broadcastId}`, this.serverUrl).toString();
    const transport = this.transportFactory(url, this.connectOpts);
    this.transport = transport;
    try {
      await transport.connect({
        onDatagram: (dgram) => {
          if (!this.stopping) this.reassembler?.push(dgram);
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

  private handleKeyframeStream(kf: KeyframeStreamFrame): void {
    if (this.stopping || !this.reorder) return;
    // Keyframes bypass the datagram reassembler, so sync its late-delta
    // watermark here — this is what makes a broadcaster restart (frameIds
    // reset to 0) recover instead of dropping every new-session delta as
    // late (R10 field finding, docs/14).
    this.reassembler?.noteStreamKeyframe(kf.frameId);
    this.keyframeStreamsReceived++;
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
  }

  private handleDecoded(decoded: DecodedFrame): void {
    this.decodedFrames++;
    this.decodedSinceStats++;
    this.lastDecodeLatencyMs = decoded.decodeEndMs - decoded.decodeStartMs;
    this.liveEdge.observe(decoded.frame.timestamp);
    this.observeCapToRender(decoded.frame.timestamp);
    // Worker path: draw + close in place so the frame never crosses a boundary.
    // Main-thread path: hand it to the callback, which draws and closes it.
    if (this.renderSink) {
      this.renderSink.draw(decoded.frame);
    } else {
      this.cb.onDecodedFrame(decoded);
    }
  }

  // R5 Q2: absolute capture→render, both sides translated to the relay clock:
  //   (viewerNow + viewerOffset) − (frame.timestamp + broadcastOffset)
  // Needs both clock legs; until then it stays null. Negative raw values
  // (asymmetry error exceeding the true latency) clamp to 0.
  private observeCapToRender(timestampUs: number): void {
    const sync = this.transport?.sampleTimeSync();
    if (!sync || this.broadcastClockOffsetUs === null) return;
    const nowU = BigInt(Math.round(performance.now() * 1000));
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
      connection: this.transport?.sampleConnectionStats() ?? null,
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
    this.transport?.close();

    if (this.decoder) await this.decoder.close();

    this.decoder = null;
    this.reassembler = null;
    this.reorder = null;
    this.transport = null;

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
