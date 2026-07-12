// Broadcaster pipeline: capture → encode → packetize → /publish datagrams.
// The capture/encode half mirrors media/loopback.ts (first-frame encoder
// negotiation and all); the decode half is replaced by the network.

import { log } from '../lib/logger';
import { startCapture, stopCapture, type CaptureHandle } from '../media/capture';
import { Encoder, type EncodedFrame, type EncoderConfigured } from '../media/encoder';
import type { CaptureConfig } from '../media/types';
import { connectWebTransport, DatagramSender, type ConnectOptions } from './connection';
import { packetizeDecoderConfig, packetizeFrame } from './packetizer';
import { parseBroadcastAnnounce } from './wire';

export interface BroadcastStats {
  encodedFrames: number;
  keyframes: number;
  droppedFrames: number;
  datagramsSent: number;
  bytesSent: number;
  configsSent: number;
  encoderQueueDepth: number;
  encoderFps: number;
  lastEncodeLatencyMs: number;
}

const EMPTY_BROADCAST_STATS: BroadcastStats = {
  encodedFrames: 0,
  keyframes: 0,
  droppedFrames: 0,
  datagramsSent: 0,
  bytesSent: 0,
  configsSent: 0,
  encoderQueueDepth: 0,
  encoderFps: 0,
  lastEncodeLatencyMs: 0,
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

  private nextFrameId = 0;
  // Latest DecoderConfig datagram; re-sent immediately before every
  // keyframe so a viewer that missed it can always recover at the next
  // keyframe (the relay additionally caches and re-emits it).
  private configDatagram: Uint8Array<ArrayBuffer> | null = null;

  private stats: BroadcastStats = { ...EMPTY_BROADCAST_STATS };
  private lastStatsAt = 0;
  private encodedSinceStats = 0;
  private statsTimer: number | null = null;

  constructor(
    config: CaptureConfig,
    serverUrl: string,
    connectOpts: ConnectOptions,
    callbacks: BroadcastCallbacks,
    broadcastId?: string,
  ) {
    this.config = config;
    this.serverUrl = serverUrl;
    this.connectOpts = connectOpts;
    this.cb = callbacks;
    this.broadcastId = broadcastId;
  }

  async start(): Promise<void> {
    // Connect before prompting for screen capture: if the publisher slot is
    // taken (409) or the server is unreachable, fail without the share
    // picker ever appearing.
    const path = this.broadcastId ? `/publish/${this.broadcastId}` : '/publish';
    const url = new URL(path, this.serverUrl).toString();
    try {
      this.wt = await connectWebTransport(url, this.connectOpts);
    } catch (e) {
      throw new BroadcastStartError('connect', e);
    }
    this.sender = new DatagramSender(this.wt);
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

  private async startMedia(): Promise<void> {
    this.capture = await startCapture(this.config);
    log.info('Capture path:', this.capture.capturePath);
    this.cb.onCapturePathChosen(this.capture.capturePath);
    this.cb.onSourceStream(this.capture.stream);

    const settings = this.capture.track.getSettings();

    this.capture.track.addEventListener('ended', () => {
      log.info('Capture track ended (user stopped sharing).');
      void this.stop();
    });

    this.lastStatsAt = performance.now();
    this.statsTimer = window.setInterval(() => this.publishStats(), 500);

    // Encoder is configured from the FIRST frame's actual dimensions, not
    // track.getSettings() — see docs/01-loopback-test.md.
    let encoderInitStarted = false;

    await this.capture.startFrames((frame) => {
      if (this.stopping) {
        frame.close();
        return;
      }

      if (!this.encoder && !encoderInitStarted) {
        encoderInitStarted = true;
        log.info(
          `First captured frame: display=${frame.displayWidth}x${frame.displayHeight}, coded=${frame.codedWidth}x${frame.codedHeight}`,
        );
        const negotiatedConfig: CaptureConfig = {
          ...this.config,
          width: roundDownToEven(frame.displayWidth),
          height: roundDownToEven(frame.displayHeight),
          framerate: settings.frameRate ?? this.config.framerate,
        };
        const enc = new Encoder(negotiatedConfig, {
          onEncoded: (encoded) => this.handleEncoded(encoded),
          onError: (e) => this.fail(e),
        });
        const firstFrame = frame;
        void enc
          .configure()
          .then((chosen) => {
            if (this.stopping) {
              firstFrame.close();
              return;
            }
            this.encoder = enc;
            this.cb.onEncoderConfigured(chosen);
            const accepted = enc.encode(firstFrame);
            if (!accepted) this.stats.droppedFrames++;
            firstFrame.close();
          })
          .catch((e) => {
            firstFrame.close();
            this.fail(e instanceof Error ? e : new Error(String(e)));
          });
        return;
      }

      if (!this.encoder) {
        frame.close();
        this.stats.droppedFrames++;
        return;
      }

      const accepted = this.encoder.encode(frame);
      if (!accepted) this.stats.droppedFrames++;
      frame.close();
    });
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
    const keyframe = chunk.type === 'key';

    let datagrams: Uint8Array<ArrayBuffer>[];
    try {
      datagrams = packetizeFrame(
        {
          frameId: this.nextFrameId++,
          keyframe,
          timestampUs: BigInt(Math.round(chunk.timestamp)),
        },
        data,
      );
    } catch (e) {
      this.fail(e instanceof Error ? e : new Error(String(e)));
      return;
    }
    // Config precedes every keyframe so a viewer can always sync up at the
    // next keyframe no matter what it missed.
    if (keyframe && this.configDatagram) {
      datagrams = [this.configDatagram, ...datagrams];
      this.stats.configsSent++;
    }

    let bytes = 0;
    for (const d of datagrams) bytes += d.length;
    this.sender
      .send(datagrams)
      .then(() => {
        this.stats.datagramsSent += datagrams.length;
        this.stats.bytesSent += bytes;
      })
      .catch((e) => {
        if (!this.stopping) this.fail(e instanceof Error ? e : new Error(String(e)));
      });
  }

  private handleSessionGone(err: Error | null): void {
    if (this.stopping) return;
    this.fail(err ?? new Error('WebTransport session closed by server'));
  }

  private publishStats(): void {
    const now = performance.now();
    const dt = (now - this.lastStatsAt) / 1000;
    if (dt > 0) this.stats.encoderFps = this.encodedSinceStats / dt;
    this.encodedSinceStats = 0;
    this.lastStatsAt = now;
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
