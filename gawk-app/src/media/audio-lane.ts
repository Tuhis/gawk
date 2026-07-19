// R15 (docs/20 Decisions 1, 3, 5, 6): the broadcaster's audio lane — audio
// MediaStreamTrackProcessor → timestamp anchor → AudioEncoder (Opus) → one
// AudioFrame datagram per packet, with the AudioConfig re-sent at 1 Hz
// piggybacked on the packet flow (audio has no keyframe to anchor re-emits
// to). The lane is strictly subordinate to the broadcast: an audio failure
// tears down the lane only, never the video pipeline.
//
// Split like BroadcastWorkerCore: AudioLaneCore is DOM-free with an
// injectable encoder factory (unit-tested with fakes); startAudioLane wires
// the real MediaStreamTrackProcessor + AudioEncoder around it.

import { log } from '../lib/logger';
import { encodeAudioConfig, encodeAudioFrame, nextFrameId } from '../transport/wire';

// Opus, 48 kHz stereo, 20 ms frames (the WebCodecs defaults), DTX off — a
// constant packet rate keeps gap detection and the buffer clock trivial
// (docs/20 Decision 1). Bitrate is a named constant, not a setting, in v1.
export const AUDIO_BITRATE_BPS = 128_000;
// Config re-send cadence (docs/20 Decision 5): lossy-tolerant by repetition.
export const AUDIO_CONFIG_RESEND_MS = 1000;
// Decision 3: if the anchored timestamp drifts more than this from the wall
// clock, re-anchor and let the viewer buffer absorb the step (expected
// ~never within a session; cheap insurance).
export const AUDIO_ANCHOR_DRIFT_LIMIT_US = 50_000;

// Maps the audio media clock onto the broadcaster's performance.now() µs
// timeline — the same clock video capture stamps (docs/20 Decision 3). The
// media clock provides drift-free 20 ms spacing; the anchor pins it to the
// shared wall clock.
export class AudioTimestampAnchor {
  private anchorUs: number | null = null;
  reanchors = 0;

  stamp(mediaTsUs: number, nowUs: number): number {
    if (this.anchorUs === null) this.anchorUs = nowUs - mediaTsUs;
    let stamped = this.anchorUs + mediaTsUs;
    if (Math.abs(stamped - nowUs) > AUDIO_ANCHOR_DRIFT_LIMIT_US) {
      this.anchorUs = nowUs - mediaTsUs;
      stamped = nowUs;
      this.reanchors++;
    }
    return stamped;
  }
}

// Turns encoded Opus packets into wire datagrams: seq stamping (own uint32
// space, wrap-aware successor) and the 1 Hz AudioConfig piggyback. Pure —
// time comes in as an argument.
export class AudioPacketizer {
  private seq: number;
  private configDatagram: Uint8Array<ArrayBuffer> | null = null;
  private lastConfigSentAtMs: number | null = null;
  configsSent = 0;

  constructor(initialSeq = 0) {
    this.seq = initialSeq >>> 0;
  }

  setConfig(config: { codec: string; sampleRate: number; channels: number; description: Uint8Array }): void {
    this.configDatagram = encodeAudioConfig(config);
    // A config change must reach viewers promptly: reset the cadence.
    this.lastConfigSentAtMs = null;
  }

  // Returns the datagrams for one packet: [config?] + the AudioFrame. The
  // config rides along on the first packet and then at most once per
  // AUDIO_CONFIG_RESEND_MS — no separate timer, the 50/s packet flow is the
  // scheduler.
  packetize(payload: Uint8Array, timestampUs: number, nowMs: number): Uint8Array<ArrayBuffer>[] {
    const out: Uint8Array<ArrayBuffer>[] = [];
    if (
      this.configDatagram &&
      (this.lastConfigSentAtMs === null || nowMs - this.lastConfigSentAtMs >= AUDIO_CONFIG_RESEND_MS)
    ) {
      out.push(this.configDatagram);
      this.lastConfigSentAtMs = nowMs;
      this.configsSent++;
    }
    out.push(encodeAudioFrame({ seq: this.seq, timestampUs: BigInt(Math.round(timestampUs)) }, payload));
    this.seq = nextFrameId(this.seq);
    return out;
  }
}

// The narrow slices of AudioData / EncodedAudioChunk / AudioEncoder the core
// touches — injectable so the core unit-tests without WebCodecs in scope.
export interface AudioDataLike {
  timestamp: number; // µs on the track's media clock
  sampleRate: number;
  numberOfChannels: number;
  close(): void;
}

export interface EncodedAudioChunkLike {
  timestamp: number; // µs, echoes the AudioData timestamp
  byteLength: number;
  copyTo(dst: AllowSharedBufferSource): void;
}

export interface AudioEncoderLike {
  encode(data: AudioDataLike): void;
  close(): void;
}

export interface AudioEncoderInit {
  codec: string;
  sampleRate: number;
  numberOfChannels: number;
  bitrate: number;
}

export type AudioEncoderFactory = (
  config: AudioEncoderInit,
  callbacks: {
    output: (chunk: EncodedAudioChunkLike, description?: Uint8Array) => void;
    error: (err: Error) => void;
  },
) => AudioEncoderLike;

export interface AudioLaneCallbacks {
  // Hands datagrams to the transport. Resolution = counted as sent;
  // rejection is swallowed (the session is dying — wt.closed owns that).
  send(datagrams: Uint8Array<ArrayBuffer>[]): Promise<void>;
  // The lane is dead (encoder error, config failure). Fired at most once;
  // the broadcast continues video-only.
  onError(err: Error): void;
}

export interface AudioLaneStats {
  encodedPackets: number;
  packetsSent: number;
  bytesSent: number;
  configsSent: number;
  sampleRate: number | null;
  channels: number | null;
  codec: string;
  bitrateBps: number;
}

const realAudioEncoderFactory: AudioEncoderFactory = (config, callbacks) => {
  const encoder = new AudioEncoder({
    output: (chunk, meta) => {
      const desc = meta?.decoderConfig?.description;
      callbacks.output(chunk, desc ? toUint8(desc) : undefined);
    },
    error: (e) => callbacks.error(e instanceof Error ? e : new Error(String(e))),
  });
  encoder.configure(config);
  return {
    encode: (data) => encoder.encode(data as AudioData),
    close: () => {
      if (encoder.state !== 'closed') encoder.close();
    },
  };
};

function toUint8(src: AllowSharedBufferSource): Uint8Array {
  if (src instanceof ArrayBuffer || src instanceof SharedArrayBuffer) return new Uint8Array(src);
  const view = src as ArrayBufferView;
  return new Uint8Array(view.buffer as ArrayBuffer, view.byteOffset, view.byteLength);
}

// DOM-free lane core. Feed it AudioData-likes; it configures the encoder
// from the first one (trust the data in hand, never track.getSettings() —
// the project's capture principle, third medium), stamps encoded packets
// onto the shared clock, and sends them.
export class AudioLaneCore {
  private cb: AudioLaneCallbacks;
  private createEncoder: AudioEncoderFactory;
  private now: () => number;
  private encoder: AudioEncoderLike | null = null;
  private packetizer = new AudioPacketizer();
  private anchor = new AudioTimestampAnchor();
  private stopped = false;

  private stats: AudioLaneStats = {
    encodedPackets: 0,
    packetsSent: 0,
    bytesSent: 0,
    configsSent: 0,
    sampleRate: null,
    channels: null,
    codec: 'opus',
    bitrateBps: AUDIO_BITRATE_BPS,
  };

  constructor(
    callbacks: AudioLaneCallbacks,
    createEncoder: AudioEncoderFactory = realAudioEncoderFactory,
    now: () => number = () => performance.now(),
  ) {
    this.cb = callbacks;
    this.createEncoder = createEncoder;
    this.now = now;
  }

  pushAudioData(data: AudioDataLike): void {
    if (this.stopped) {
      data.close();
      return;
    }
    if (!this.encoder) {
      try {
        this.configureFrom(data);
      } catch (e) {
        data.close();
        this.fail(e instanceof Error ? e : new Error(String(e)));
        return;
      }
    }
    try {
      this.encoder!.encode(data);
    } catch (e) {
      data.close();
      this.fail(e instanceof Error ? e : new Error(String(e)));
      return;
    }
    data.close();
  }

  private configureFrom(data: AudioDataLike): void {
    this.stats.sampleRate = data.sampleRate;
    this.stats.channels = data.numberOfChannels;
    this.packetizer.setConfig({
      codec: 'opus',
      sampleRate: data.sampleRate,
      channels: data.numberOfChannels,
      description: new Uint8Array(0),
    });
    this.encoder = this.createEncoder(
      {
        codec: 'opus',
        sampleRate: data.sampleRate,
        numberOfChannels: data.numberOfChannels,
        bitrate: AUDIO_BITRATE_BPS,
      },
      {
        output: (chunk, description) => this.handleEncoded(chunk, description),
        error: (err) => this.fail(err),
      },
    );
  }

  private handleEncoded(chunk: EncodedAudioChunkLike, description?: Uint8Array): void {
    if (this.stopped) return;
    // Opus from WebCodecs normally needs no description; if the encoder
    // emits one, viewers must get it (docs/20 Decision 5).
    if (description && description.length > 0 && this.stats.sampleRate !== null) {
      this.packetizer.setConfig({
        codec: 'opus',
        sampleRate: this.stats.sampleRate,
        channels: this.stats.channels ?? 2,
        description,
      });
    }
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);
    const nowMs = this.now();
    const stampedUs = this.anchor.stamp(chunk.timestamp, nowMs * 1000);
    let datagrams: Uint8Array<ArrayBuffer>[];
    try {
      datagrams = this.packetizer.packetize(payload, stampedUs, nowMs);
    } catch (e) {
      // An unencodable packet (empty/oversize) breaks the wire invariant —
      // treat it as a lane failure, not a silent drop streak.
      this.fail(e instanceof Error ? e : new Error(String(e)));
      return;
    }
    this.stats.encodedPackets++;
    this.stats.configsSent = this.packetizer.configsSent;
    let bytes = 0;
    for (const d of datagrams) bytes += d.length;
    this.cb
      .send(datagrams)
      .then(() => {
        this.stats.packetsSent++;
        this.stats.bytesSent += bytes;
      })
      .catch(() => {
        // Session dying/resuming — wt.closed owns that story (R17). Audio
        // packets are droppable by design; the lane keeps going.
      });
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
      this.encoder?.close();
    } catch {
      // already closed — fine
    }
    this.encoder = null;
  }

  getStats(): AudioLaneStats {
    return { ...this.stats };
  }
}

// Whether this scope can run the lane at all. Firefox broadcasters lack both
// halves today — video-only is the designed graceful state.
export function audioLaneSupported(): boolean {
  return (
    typeof AudioEncoder === 'function' &&
    typeof (globalThis as { MediaStreamTrackProcessor?: unknown }).MediaStreamTrackProcessor ===
      'function'
  );
}

export interface RunningAudioLane {
  stop(): void;
  getStats(): AudioLaneStats;
}

// Wires the real MediaStreamTrackProcessor pump around an AudioLaneCore.
// Does NOT own the track — the media source that produced it stops it.
export function startAudioLane(track: MediaStreamTrack, callbacks: AudioLaneCallbacks): RunningAudioLane {
  const core = new AudioLaneCore(callbacks);
  const processor = new MediaStreamTrackProcessor({ track: track as MediaStreamAudioTrack });
  const reader = processor.readable.getReader();
  let stopped = false;

  const pump = async () => {
    try {
      while (!stopped) {
        const { value, done } = await reader.read();
        if (done) break;
        if (value) core.pushAudioData(value);
      }
    } catch (e) {
      // Reader cancellation during teardown lands here; anything else is a
      // dead capture track — the lane just stops producing.
      if (!stopped) log.warn('Audio capture pump ended:', e);
    }
  };
  void pump();

  return {
    stop() {
      stopped = true;
      try {
        void reader.cancel();
      } catch {
        // ignore
      }
      core.stop();
    },
    getStats: () => core.getStats(),
  };
}
