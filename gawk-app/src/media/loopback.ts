import { log } from '../lib/logger';
import { startCapture, stopCapture, type CaptureHandle } from './capture';
import { Encoder, probeHardwareSupport, type EncodedFrame, type EncoderConfigured } from './encoder';
import { Decoder, type DecodedFrame } from './decoder';
import type { CaptureConfig, PipelineStats } from './types';
import { EMPTY_STATS } from './types';

export interface LoopbackCallbacks {
  onSourceStream: (stream: MediaStream) => void;
  onDecodedFrame: (decoded: DecodedFrame) => void;
  onStats: (stats: PipelineStats) => void;
  onEncoderConfigured: (info: EncoderConfigured) => void;
  onCapturePathChosen: (path: string) => void;
  onError: (err: Error) => void;
  onEnded: () => void;
}

// Encoders typically want even dimensions; H.264 additionally wants
// macroblock alignment. Rounding down to even keeps H.264 in the running
// even when getDisplayMedia hands us weird sizes on some DPI/monitor combos.
function roundDownToEven(n: number): number {
  return n - (n % 2);
}

export class LoopbackPipeline {
  private config: CaptureConfig;
  private cb: LoopbackCallbacks;
  private capture: CaptureHandle | null = null;
  private encoder: Encoder | null = null;
  private decoder: Decoder | null = null;
  // All decoder operations chain off this promise so `configure` completes
  // before any `decode`, and decodes run in the order chunks arrived.
  private decoderChain: Promise<void> = Promise.resolve();
  private stopping = false;

  private stats: PipelineStats = { ...EMPTY_STATS };
  private lastStatsAt = 0;
  private encodedSinceStats = 0;
  private decodedSinceStats = 0;
  private statsTimer: number | null = null;

  constructor(config: CaptureConfig, callbacks: LoopbackCallbacks) {
    this.config = config;
    this.cb = callbacks;
  }

  async start(): Promise<void> {
    this.capture = await startCapture(this.config);
    log.info('Capture path:', this.capture.capturePath);
    this.cb.onCapturePathChosen(this.capture.capturePath);
    this.cb.onSourceStream(this.capture.stream);

    const settings = this.capture.track.getSettings();
    log.info('Track getSettings:', settings);

    this.decoder = new Decoder({
      onDecoded: (decoded) => this.handleDecoded(decoded),
      onError: (e) => this.fail(e),
    });

    this.capture.track.addEventListener('ended', () => {
      log.info('Capture track ended (user stopped sharing).');
      void this.stop();
    });

    this.lastStatsAt = performance.now();
    this.statsTimer = window.setInterval(() => this.publishStats(), 500);

    // Encoder is configured from the FIRST frame's actual dimensions rather
    // than track.getSettings() — Chrome sometimes disagrees with itself,
    // reporting one shape in getSettings() while delivering frames to MSTP
    // at a different shape, which caused the encoder to silently distort.
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
        const width = roundDownToEven(frame.displayWidth);
        const height = roundDownToEven(frame.displayHeight);
        let framerate = settings.frameRate ?? this.config.framerate;

        const proceedInit = async () => {
          if ((width > 1920 || height > 1080) && framerate > 30) {
            const hwSupported = await probeHardwareSupport(
              this.config.codecPreferences,
              width,
              height,
              framerate,
            );
            if (!hwSupported) {
              log.info(
                `HW encoding not supported for loopback target ${width}x${height}@${framerate}fps. Capping to 30fps.`,
              );
              framerate = 30;
            }
          }

          const negotiatedConfig: CaptureConfig = {
            ...this.config,
            width,
            height,
            framerate,
          };
          log.info(
            `Configuring encoder for ${negotiatedConfig.width}x${negotiatedConfig.height}, codec choices:`,
            negotiatedConfig.codecPreferences,
          );
          const enc = new Encoder(negotiatedConfig, {
            onEncoded: (encoded) => this.handleEncoded(encoded),
            onError: (e) => this.fail(e),
          });
          try {
            const chosen = await enc.configure();
            if (this.stopping) {
              firstFrame.close();
              return;
            }
            this.encoder = enc;
            this.cb.onEncoderConfigured(chosen);
            const accepted = enc.encode(firstFrame);
            if (!accepted) this.stats.droppedFrames++;
            firstFrame.close();
          } catch (e) {
            firstFrame.close();
            this.fail(e instanceof Error ? e : new Error(String(e)));
          }
        };
        const firstFrame = frame;
        void proceedInit();
        return;
      }

      if (!this.encoder) {
        // Encoder is still configuring — drop this frame.
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
    this.stats.encodedFrames++;
    this.encodedSinceStats++;
    if (encoded.chunk.type === 'key') this.stats.keyframes++;
    this.stats.lastEncodeLatencyMs = encoded.encodeEndMs - encoded.encodeStartMs;
    this.stats.encoderQueueDepth = this.encoder?.queueSize ?? 0;

    if (!this.decoder) return;
    const dec = this.decoder;
    const chunk = encoded.chunk;

    // On the first encoded chunk the encoder gives us a decoderConfig that
    // already includes codec + codedSize + (for H.264) the AVCC extradata.
    // Chain it into decoderChain so no decode runs before configure resolves.
    if (this.stats.encodedFrames === 1) {
      const cfg = encoded.meta?.decoderConfig;
      if (!cfg) {
        this.fail(new Error('Encoder produced first chunk without decoderConfig'));
        return;
      }
      this.decoderChain = this.decoderChain.then(() => dec.configure(cfg));
    }
    // Chain every decode after the previous op. This preserves chunk order
    // (P-frames won't reach the decoder before the keyframe they depend on)
    // and guarantees configure completes first.
    this.decoderChain = this.decoderChain.then(() => {
      if (!this.stopping) dec.decode(chunk);
    });
    this.decoderChain.catch((e) => this.fail(e instanceof Error ? e : new Error(String(e))));
  }

  private handleDecoded(decoded: DecodedFrame): void {
    this.stats.decodedFrames++;
    this.decodedSinceStats++;
    this.stats.lastDecodeLatencyMs = decoded.decodeEndMs - decoded.decodeStartMs;
    this.stats.decoderQueueDepth = this.decoder?.queueSize ?? 0;
    // End-to-end within-tab latency uses the capture timestamp origin.
    const nowUs = performance.now() * 1000;
    this.stats.lastEndToEndLatencyMs = (nowUs - decoded.captureTimestampUs) / 1000;
    this.cb.onDecodedFrame(decoded);
  }

  private publishStats(): void {
    const now = performance.now();
    const dt = (now - this.lastStatsAt) / 1000;
    if (dt > 0) {
      this.stats.encoderFps = this.encodedSinceStats / dt;
      this.stats.decoderFps = this.decodedSinceStats / dt;
    }
    this.encodedSinceStats = 0;
    this.decodedSinceStats = 0;
    this.lastStatsAt = now;
    this.cb.onStats({ ...this.stats });
  }

  private fail(err: Error): void {
    log.error('Pipeline error:', err);
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

    if (this.encoder) await this.encoder.close();
    if (this.decoder) await this.decoder.close();
    if (this.capture) stopCapture(this.capture);

    this.encoder = null;
    this.decoder = null;
    this.capture = null;

    this.cb.onEnded();
  }
}
