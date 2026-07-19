// R15 (docs/20 Decision 7): the viewer's audio decode lane. Runs wherever the
// pipeline runs — inside the viewer worker on the offloaded path, on the main
// thread in the fallback — and emits planar PCM ready for the AudioWorklet
// sink.
//
// Why planar PCM out (and not a transferred AudioData): the sink must reach
// an AudioWorklet, which needs Float32 planar channels. copyTo() has to happen
// somewhere; doing it here means the copy lands in the worker (the thread we
// spent R8/R10 clearing is the *main* one) and only plain ArrayBuffers cross
// the boundary — no reliance on AudioData being transferable. Same "no
// structured clone of media" property the design asked for.

import { log } from '../lib/logger';
import type { AudioPacket } from './reassembler';
import type { AudioConfigMessage } from './wire';

// One decoded, boundary-ready audio chunk.
export interface DecodedAudioChunk {
  timestampUs: number;
  sampleRate: number;
  // Planar: one Float32Array per channel. Their buffers are transferable.
  channels: Float32Array[];
  frameCount: number;
}

export interface AudioDecodeCallbacks {
  onChunk: (chunk: DecodedAudioChunk) => void;
  // The lane died (unsupported codec, decode failure). Video keeps playing —
  // audio is strictly additive (docs/20 Decision 7).
  onError: (err: Error) => void;
}

export interface AudioDecodeStats {
  packetsDecoded: number;
  decodeErrors: number;
  codec: string | null;
  sampleRate: number | null;
  channels: number | null;
}

// Whether this scope can decode audio at all. A viewer without it plays
// video-only with an annotation — never a pipeline-placement change.
export function audioDecodeSupported(): boolean {
  return typeof AudioDecoder === 'function';
}

export class AudioDecodeLane {
  private cb: AudioDecodeCallbacks;
  private decoder: AudioDecoder | null = null;
  private configKey: string | null = null;
  private stopped = false;
  private stats: AudioDecodeStats = {
    packetsDecoded: 0,
    decodeErrors: 0,
    codec: null,
    sampleRate: null,
    channels: null,
  };

  constructor(callbacks: AudioDecodeCallbacks) {
    this.cb = callbacks;
  }

  // Applies an AudioConfig. Deduplicated by content: the broadcaster re-sends
  // it at 1 Hz (docs/20 Decision 5), and reconfiguring mid-stream would drop
  // the decoder's state for nothing.
  configure(config: AudioConfigMessage): void {
    if (this.stopped) return;
    const key = `${config.codec}:${config.sampleRate}:${config.channels}:${Array.from(config.description).join(',')}`;
    if (key === this.configKey) return;
    this.configKey = key;
    this.stats.codec = config.codec;
    this.stats.sampleRate = config.sampleRate;
    this.stats.channels = config.channels;

    try {
      this.decoder?.close();
    } catch {
      // already closed — fine
    }
    try {
      const decoder = new AudioDecoder({
        output: (data) => this.handleDecoded(data),
        error: (e) => this.fail(e instanceof Error ? e : new Error(String(e))),
      });
      decoder.configure({
        codec: config.codec,
        sampleRate: config.sampleRate,
        numberOfChannels: config.channels,
        ...(config.description.length > 0 ? { description: config.description.slice() } : {}),
      });
      this.decoder = decoder;
    } catch (e) {
      this.decoder = null;
      this.fail(e instanceof Error ? e : new Error(String(e)));
    }
  }

  // Feeds one demuxed packet. Packets arriving before the config are dropped:
  // the config is join-primed by the relay and re-sent at 1 Hz, so the wait is
  // bounded by a second in the worst case.
  push(packet: AudioPacket): void {
    const decoder = this.decoder;
    if (this.stopped || !decoder || decoder.state !== 'configured') return;
    try {
      decoder.decode(
        new EncodedAudioChunk({
          type: 'key', // every Opus packet is independently decodable
          timestamp: Number(packet.timestampUs),
          data: packet.payload,
        }),
      );
    } catch (e) {
      this.stats.decodeErrors++;
      this.fail(e instanceof Error ? e : new Error(String(e)));
    }
  }

  private handleDecoded(data: AudioData): void {
    if (this.stopped) {
      data.close();
      return;
    }
    try {
      const channelCount = data.numberOfChannels;
      const frameCount = data.numberOfFrames;
      const channels: Float32Array[] = [];
      for (let i = 0; i < channelCount; i++) {
        const plane = new Float32Array(frameCount);
        data.copyTo(plane, { planeIndex: i, format: 'f32-planar' });
        channels.push(plane);
      }
      this.stats.packetsDecoded++;
      this.cb.onChunk({
        timestampUs: data.timestamp,
        sampleRate: data.sampleRate,
        channels,
        frameCount,
      });
    } catch (e) {
      this.stats.decodeErrors++;
      log.warn('Audio copyTo failed; dropping packet:', e);
    } finally {
      data.close();
    }
  }

  private fail(err: Error): void {
    if (this.stopped) return;
    this.stop();
    this.cb.onError(err);
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    try {
      if (this.decoder && this.decoder.state !== 'closed') this.decoder.close();
    } catch {
      // already closed — fine
    }
    this.decoder = null;
  }

  getStats(): AudioDecodeStats {
    return { ...this.stats };
  }
}
