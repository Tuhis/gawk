// Viewer pipeline: /subscribe datagrams → reassemble → decode → callback.
// The decode half mirrors media/loopback.ts (ordered decoder chain); the
// encode half is replaced by the network.

import { log } from '../lib/logger';
import { Decoder, type DecodedFrame } from '../media/decoder';
import { connectWebTransport, readDatagrams, type ConnectOptions } from './connection';
import { Reassembler, type ReassemblerStats } from './reassembler';
import type { DecoderConfigMessage } from './wire';

export interface ViewerStats extends ReassemblerStats {
  decodedFrames: number;
  decoderQueueDepth: number;
  decoderFps: number;
  configsApplied: number;
  framesDiscardedAwaitingKey: number;
  lastDecodeLatencyMs: number;
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

  private wt: WebTransport | null = null;
  private decoder: Decoder | null = null;
  private reassembler: Reassembler | null = null;
  private abort = new AbortController();
  private stopping = false;

  // Decoder ops chain so configure completes before any decode and decodes
  // stay in arrival order — same discipline as the loopback pipeline.
  private decoderChain: Promise<void> = Promise.resolve();
  // WebCodecs requires the first chunk after configure() to be a keyframe;
  // set on every (re)configure, cleared by the first keyframe.
  private waitingForKeyframe = true;

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

  private broadcastId: string;

  constructor(
    serverUrl: string,
    broadcastId: string,
    connectOpts: ConnectOptions,
    callbacks: ViewerCallbacks,
  ) {
    this.serverUrl = serverUrl;
    this.broadcastId = broadcastId;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
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

    this.reassembler = new Reassembler({
      onConfig: (config) => this.applyConfig(config),
      onFrame: (frame) => this.decodeFrame(frame.keyframe, frame.timestampUs, frame.data),
    });

    this.lastStatsAt = performance.now();
    this.statsTimer = window.setInterval(() => this.publishStats(), 500);

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

    // Read loop runs for the life of the session. On a joining viewer the
    // relay primes us with the cached config + last keyframe, so the first
    // picture typically appears without waiting for the next keyframe.
    const reassembler = this.reassembler;
    const wt = this.wt;
    void readDatagrams(wt, (dgram) => reassembler.push(dgram), this.abort.signal)
      .then(() => this.handleReadLoopEnd(wt, null))
      .catch((e) => this.handleReadLoopEnd(wt, e instanceof Error ? e : new Error(String(e))));
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

  private applyConfig(config: DecoderConfigMessage): void {
    this.pendingConfig = config;
    this.lastConfigMessage = config;
    this.lastCodec = config.codec;
    this.waitingForKeyframe = true;
    this.cb.onConfig(config);
  }

  private decodeFrame(keyframe: boolean, timestampUs: bigint, data: Uint8Array): void {
    const dec = this.decoder;
    if (!dec || this.stopping) return;

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
    this.chainDecoderOp(() => {
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
    this.cb.onDecodedFrame(decoded);
  }

  private publishStats(): void {
    const now = performance.now();
    const dt = (now - this.lastStatsAt) / 1000;
    const decoderFps = dt > 0 ? this.decodedSinceStats / dt : 0;
    this.decodedSinceStats = 0;
    this.lastStatsAt = now;
    const reasm = this.reassembler?.getStats();
    this.cb.onStats({
      datagramsReceived: reasm?.datagramsReceived ?? 0,
      badDatagrams: reasm?.badDatagrams ?? 0,
      duplicateChunks: reasm?.duplicateChunks ?? 0,
      duplicateConfigs: reasm?.duplicateConfigs ?? 0,
      framesCompleted: reasm?.framesCompleted ?? 0,
      framesDroppedIncomplete: reasm?.framesDroppedIncomplete ?? 0,
      framesDroppedLate: reasm?.framesDroppedLate ?? 0,
      decodedFrames: this.decodedFrames,
      decoderQueueDepth: this.decoder?.queueSize ?? 0,
      decoderFps,
      configsApplied: this.configsApplied,
      framesDiscardedAwaitingKey: this.framesDiscardedAwaitingKey,
      lastDecodeLatencyMs: this.lastDecodeLatencyMs,
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
    this.abort.abort();

    if (this.decoder) await this.decoder.close();
    try {
      this.wt?.close();
    } catch {
      // already closed by the server — fine
    }

    this.decoder = null;
    this.reassembler = null;
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
