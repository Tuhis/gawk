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
      onError: (e) => this.fail(e),
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
    const dec = this.decoder;
    if (!dec) return;
    // Copy the extradata: it aliases the datagram buffer and the decoder
    // config may be consumed after we've moved on.
    const decoderConfig: VideoDecoderConfig = {
      codec: config.codec,
      ...(config.extradata.length > 0 ? { description: config.extradata.slice() } : {}),
      optimizeForLatency: true,
    };
    log.info('Applying decoder config:', config.codec, `(${config.extradata.length}B extradata)`);
    this.configsApplied++;
    this.waitingForKeyframe = true;
    this.chainDecoderOp(() => dec.configure(decoderConfig));
    this.cb.onConfig(config);
  }

  private decodeFrame(keyframe: boolean, timestampUs: bigint, data: Uint8Array): void {
    const dec = this.decoder;
    if (!dec || this.stopping) return;
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
      this.fail(e instanceof Error ? e : new Error(String(e)));
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
