// Viewer pipeline: /subscribe datagrams → reassemble → decode → callback.
// The decode half mirrors media/loopback.ts (ordered decoder chain); the
// encode half is replaced by the network.

import { log } from '../lib/logger';
import { Decoder, type DecodedFrame } from '../media/decoder';
import {
  connectWebTransport,
  readDatagrams,
  readKeyframeStreams,
  type ConnectOptions,
} from './connection';
import { Reassembler, type ReassemblerStats } from './reassembler';
import { ReorderBuffer, type ReleasedFrame, type ReorderStats } from './reorder-buffer';
import type { RenderSink } from './render-sink';
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

  private wt: WebTransport | null = null;
  private decoder: Decoder | null = null;
  private reassembler: Reassembler | null = null;
  // Merges reliable stream keyframes with lossy datagram deltas by frameId and
  // owns the freeze-on-gap / drop-to-keyframe ordering policy (R8, docs/12).
  private reorder: ReorderBuffer | null = null;
  private reorderTimer: number | null = null;
  private abort = new AbortController();
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
  // The codec of the last applied config — for a clear "can't decode" message.
  private lastCodec: string | null = null;
  private pendingConfig: DecoderConfigMessage | null = null;
  private preferSoftware = false;
  private lastConfigMessage: DecoderConfigMessage | null = null;
  private pendingDecodes = 0;

  private broadcastId: string;

  constructor(
    serverUrl: string,
    broadcastId: string,
    connectOpts: ConnectOptions,
    callbacks: ViewerCallbacks,
    renderSink: RenderSink | null = null,
  ) {
    this.serverUrl = serverUrl;
    this.broadcastId = broadcastId;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.renderSink = renderSink;
  }

  async start(): Promise<void> {
    const url = new URL(`/subscribe/${this.broadcastId}`, this.serverUrl).toString();
    this.wt = await connectWebTransport(url, this.connectOpts);

    this.decoder = new Decoder({
      onDecoded: (decoded) => this.handleDecoded(decoded),
      // A decoder error (unsupported codec, decode failure) is not recoverable
      // by reconnecting — mark it fatal so the session surfaces it and stops.
      onError: (e) => this.failDecode(e),
    });

    this.reorder = new ReorderBuffer((frame) => this.decodeReleased(frame));

    this.reassembler = new Reassembler({
      // A datagram-borne config (legacy path) still applies; keyframes now
      // carry their own config on the stream.
      onConfig: (config) => this.maybeApplyConfig(config),
      // Reassembled datagram frames feed the reorder buffer. Keyframes only
      // arrive over streams in practice, but routing a keyframe-flagged
      // datagram here too keeps the viewer robust to any keyframe source.
      onFrame: (frame) => {
        if (!this.reorder) return;
        if (frame.keyframe) {
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

    this.lastStatsAt = performance.now();
    // Bare setInterval (not window.*) so the pipeline runs unchanged inside a
    // Web Worker (R8 S6), where `window` is undefined.
    this.statsTimer = setInterval(() => this.publishStats(), 500) as unknown as number;
    this.reorderTimer = setInterval(() => this.reorderTick(), REORDER_TICK_MS) as unknown as number;

    void this.wt.closed
      .then((closeInfo) => {
        if (!this.stopping) {
          const code = (closeInfo as any)?.closeCode;
          const reason = (closeInfo as any)?.reason;
          this.handleClose(code, reason);
        }
      })
      .catch((err) => {
        if (!this.stopping) {
          const code = err?.closeCode;
          const reason = err?.reason || err?.message;
          this.handleClose(code, reason);
        }
      });

    // Read loops run for the life of the session. Deltas arrive as datagrams;
    // keyframes arrive as reliable unidirectional streams (R8). On a joining
    // viewer the relay primes us with the last keyframe over a stream, so the
    // first picture typically appears without waiting for the next keyframe.
    const reassembler = this.reassembler;
    const wt = this.wt;
    void readDatagrams(wt, (dgram) => reassembler.push(dgram), this.abort.signal)
      .then(() => this.handleReadLoopEnd(wt, null))
      .catch((e) => this.handleReadLoopEnd(wt, e instanceof Error ? e : new Error(String(e))));

    // Keyframe streams: failures here are not fatal to the session (the next
    // keyframe recovers, and a real drop surfaces via the datagram loop /
    // wt.closed), so they are logged, not propagated.
    void readKeyframeStreams(
      wt,
      (kf) => {
        if (this.stopping || !this.reorder) return;
        this.keyframeStreamsReceived++;
        this.reorder.pushKeyframe({
          frameId: kf.frameId,
          timestampUs: kf.timestampUs,
          config: kf.config,
          data: kf.data,
        });
      },
      this.abort.signal,
    ).catch((e) => {
      if (!this.stopping) log.warn('Keyframe stream loop ended:', e);
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

  // On a server close, the datagram read loop and wt.closed settle in
  // unspecified, browser-dependent order — and only wt.closed carries the
  // close code (4000 = broadcast ended, the one signal that must stop
  // reconnecting). Give wt.closed a short window to settle before treating
  // the read-loop end as an anonymous drop.
  private async handleReadLoopEnd(wt: WebTransport, err: Error | null): Promise<void> {
    if (this.stopping) return;
    const closeInfo = await Promise.race([
      wt.closed.then(
        (info) => info ?? {},
        (e) => e ?? {},
      ),
      new Promise<null>((r) => setTimeout(() => r(null), 100)),
    ]);
    if (this.stopping) return; // the wt.closed handler acted first
    if (closeInfo !== null) {
      const info = closeInfo as { closeCode?: number; reason?: string; message?: string };
      this.handleClose(info.closeCode, info.reason || info.message);
      return;
    }
    this.fail(err ?? new Error('WebTransport session closed by server'));
  }

  private handleClose(closeCode?: number, reason?: string): void {
    if (this.stopping) return;
    const msg = reason ? `WebTransport session closed: ${reason}` : 'WebTransport session closed by server';
    const err = new Error(msg) as any;
    if (closeCode !== undefined) {
      err.closeCode = closeCode;
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

  private handleDecoded(decoded: DecodedFrame): void {
    this.decodedFrames++;
    this.decodedSinceStats++;
    this.lastDecodeLatencyMs = decoded.decodeEndMs - decoded.decodeStartMs;
    // Worker path: draw + close in place so the frame never crosses a boundary.
    // Main-thread path: hand it to the callback, which draws and closes it.
    if (this.renderSink) {
      this.renderSink.draw(decoded.frame);
    } else {
      this.cb.onDecodedFrame(decoded);
    }
  }

  private publishStats(): void {
    const now = performance.now();
    const dt = (now - this.lastStatsAt) / 1000;
    const decoderFps = dt > 0 ? this.decodedSinceStats / dt : 0;
    this.decodedSinceStats = 0;
    this.lastStatsAt = now;
    const reasm = this.reassembler?.getStats();
    const reorder: ReorderStats | undefined = this.reorder?.getStats();
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
    this.abort.abort();

    if (this.decoder) await this.decoder.close();
    try {
      this.wt?.close();
    } catch {
      // already closed by the server — fine
    }

    this.decoder = null;
    this.reassembler = null;
    this.reorder = null;
    this.wt = null;

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
