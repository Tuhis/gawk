import { log } from '../lib/logger';
import type { CaptureConfig } from './types';

type ConfigVariant = {
  label: string;
  hardwareAcceleration?: HardwareAcceleration;
  latencyMode?: LatencyMode;
};

// The WebCodecs spec only has 'prefer-hardware' | 'prefer-software' | 'no-preference'
// — no way to *require* hardware. But: if we ask for 'prefer-hardware' and
// isConfigSupported returns supported=true, Chrome commits to using HW (it
// returns false when it can't). So probing with prefer-hardware FIRST gives
// us a reasonable answer. If that fails, we drop the hint and the browser
// falls back to whatever it can do — usually software.
const CONFIG_VARIANTS: ConfigVariant[] = [
  { label: 'prefer-hw + realtime', hardwareAcceleration: 'prefer-hardware', latencyMode: 'realtime' },
  { label: 'prefer-hw',            hardwareAcceleration: 'prefer-hardware' },
  { label: 'realtime',             latencyMode: 'realtime' },
  { label: 'default' },
];

export type Acceleration = 'hardware' | 'software' | 'unknown';

// Time-based keyframe cadence (docs/08): a keyframe is forced when the frame
// timestamp is at least the interval past the last keyframe's. Frame-count
// cadence would stretch the GOP to 24s at the ladder's 5 fps rung. Pure —
// unit-tested in encoder-keyframe.test.ts.
export class KeyframeCadence {
  private intervalUs: number;
  private lastKeyframeTsUs: number | null = null;

  constructor(intervalMs: number) {
    this.intervalUs = intervalMs * 1000;
  }

  shouldKeyframe(timestampUs: number): boolean {
    if (this.lastKeyframeTsUs === null || timestampUs - this.lastKeyframeTsUs >= this.intervalUs) {
      this.lastKeyframeTsUs = timestampUs;
      return true;
    }
    return false;
  }
}

function classifyAcceleration(
  variant: ConfigVariant,
  resolved: HardwareAcceleration | undefined,
): Acceleration {
  // If the resolved (post-isConfigSupported) config says prefer-software, the
  // browser told us explicitly it went software — trust that over our request.
  if (resolved === 'prefer-software') return 'software';
  if (resolved === 'prefer-hardware') return 'hardware';
  // No resolved answer. Fall back to what we asked for: if we asked for HW
  // and got 'supported: true', Chrome does commit to HW.
  if (variant.hardwareAcceleration === 'prefer-hardware') return 'hardware';
  return 'unknown';
}

export interface EncoderConfigured {
  codec: string;
  variantLabel: string;
  acceleration: Acceleration;
  width: number;
  height: number;
  framerate: number;
  bitrate: number;
}

export interface EncodedFrame {
  chunk: EncodedVideoChunk;
  meta: EncodedVideoChunkMetadata | undefined;
  captureTimestampUs: number;
  encodeStartMs: number;
  encodeEndMs: number;
}

export interface EncoderCallbacks {
  onEncoded: (frame: EncodedFrame) => void;
  onError: (err: Error) => void;
}

export class Encoder {
  private encoder: VideoEncoder;
  private config: CaptureConfig;
  private cadence: KeyframeCadence;
  private captureStartMap = new Map<number, number>();
  private chosenCodec: string | null = null;
  private disposed = false;

  constructor(config: CaptureConfig, callbacks: EncoderCallbacks) {
    this.config = config;
    this.cadence = new KeyframeCadence(config.keyframeIntervalMs);
    this.encoder = new VideoEncoder({
      output: (chunk, meta) => {
        if (this.disposed) return;
        const captureTsUs = chunk.timestamp;
        const encodeStartMs = this.captureStartMap.get(captureTsUs) ?? performance.now();
        this.captureStartMap.delete(captureTsUs);
        callbacks.onEncoded({
          chunk,
          meta,
          captureTimestampUs: captureTsUs,
          encodeStartMs,
          encodeEndMs: performance.now(),
        });
      },
      error: (e) => {
        if (this.disposed) return;
        callbacks.onError(e instanceof Error ? e : new Error(String(e)));
      },
    });
  }

  async configure(): Promise<EncoderConfigured> {
    const attempts: string[] = [];
    for (const codec of this.config.codecPreferences) {
      for (const variant of CONFIG_VARIANTS) {
        const encoderConfig: VideoEncoderConfig = {
          codec,
          width: this.config.width,
          height: this.config.height,
          bitrate: this.config.bitrate,
          framerate: this.config.framerate,
          ...(variant.latencyMode ? { latencyMode: variant.latencyMode } : {}),
          ...(variant.hardwareAcceleration ? { hardwareAcceleration: variant.hardwareAcceleration } : {}),
        };
        try {
          const support = await VideoEncoder.isConfigSupported(encoderConfig);
          if (support.supported) {
            const finalConfig = support.config ?? encoderConfig;
            this.encoder.configure(finalConfig);
            this.chosenCodec = codec;
            const acceleration = classifyAcceleration(variant, finalConfig.hardwareAcceleration);
            log.info(
              `Encoder accepted ${codec} with variant "${variant.label}" (${acceleration}); resolved config:`,
              finalConfig,
            );
            return {
              codec,
              variantLabel: variant.label,
              acceleration,
              width: this.config.width,
              height: this.config.height,
              framerate: this.config.framerate,
              bitrate: this.config.bitrate,
            };
          }
          attempts.push(`${codec}/${variant.label}: unsupported`);
        } catch (e) {
          attempts.push(`${codec}/${variant.label}: ${e instanceof Error ? e.message : String(e)}`);
        }
      }
    }
    log.error('All encoder configs rejected. Attempts:', attempts);
    throw new Error(
      `No codec / config combination supported at ${this.config.width}x${this.config.height}@${this.config.framerate}. See console for full list.`,
    );
  }

  get codec(): string | null {
    return this.chosenCodec;
  }

  encode(frame: VideoFrame): boolean {
    if (this.encoder.state !== 'configured') return false;
    if (this.encoder.encodeQueueSize > 2) return false;
    const keyFrame = this.cadence.shouldKeyframe(frame.timestamp);
    this.captureStartMap.set(frame.timestamp, performance.now());
    this.encoder.encode(frame, { keyFrame });
    return true;
  }

  get queueSize(): number {
    return this.encoder.encodeQueueSize;
  }

  // Immediate teardown for the mid-stream ladder-change path: no flush —
  // in-flight frames are deliberately dropped (drops over stalls), and any
  // late callbacks from the browser are suppressed so a dying encoder can't
  // fail the pipeline that already replaced it.
  dispose(): void {
    this.disposed = true;
    if (this.encoder.state !== 'closed') this.encoder.close();
    this.captureStartMap.clear();
  }

  async close(): Promise<void> {
    try {
      if (this.encoder.state === 'configured') await this.encoder.flush();
    } catch {
      // flush can throw during shutdown races — ignore
    }
    if (this.encoder.state !== 'closed') this.encoder.close();
    this.captureStartMap.clear();
  }
}

export async function probeHardwareSupport(
  codecs: string[],
  width: number,
  height: number,
  framerate: number,
): Promise<boolean> {
  if (typeof VideoEncoder === 'undefined' || typeof VideoEncoder.isConfigSupported !== 'function') {
    return false;
  }
  for (const codec of codecs) {
    try {
      const support = await VideoEncoder.isConfigSupported({
        codec,
        width,
        height,
        bitrate: 1_000_000,
        framerate,
        hardwareAcceleration: 'prefer-hardware',
      });
      if (support.supported && support.config?.hardwareAcceleration !== 'prefer-software') {
        return true;
      }
    } catch {
      // ignore
    }
  }
  return false;
}

