// R22 audio, iOS path (docs/27 finding 4): re-encode the decoded R15 audio lane
// to AAC so the iPhone's native-fullscreen player can have sound.
//
// Why this exists at all: the muxed presentation carries Opus verbatim wherever
// the runtime accepts it (Chrome does — the CI proof), but iOS 18.7 / Safari
// 26.5.2 answered `isTypeSupported('audio/mp4; codecs="opus"')` with **false**
// through ManagedMediaSource, so on the one device this whole feature exists for
// there is no Opus track to be had. AAC-LC is the codec Apple's own HLS mandates,
// and it is the one audio format an iPhone is guaranteed to demux from fMP4.
//
// Where it sits: the viewer worker already decodes Opus to planar PCM for the
// main-thread AudioWorklet sink (docs/20 Decision 7). This taps that same
// decoded stream — so the transcode is decode-once, encode-once, and it does not
// touch the inline audio path at all. If the encoder is unavailable or fails, the
// tap goes quiet and the presentation stays video-only; audio keeps playing
// inline exactly as before.
//
// DOM-free and injectable: `AudioEncoder`/`AudioData` come in through the
// constructor so this unit-tests in node with fakes.

import { log } from '../lib/logger';

// The decoded chunk shape this consumes (structurally DecodedAudioChunk from
// audio-decode.ts, which is the producer).
export interface TranscodeInput {
  timestampUs: number;
  sampleRate: number;
  channels: Float32Array[];
  frameCount: number;
}

export interface TranscodedAudio {
  timestampUs: number;
  data: Uint8Array;
  // Present on the first output only (and after a reconfigure): the
  // AudioSpecificConfig the muxer must put in `esds`. Taken from the encoder
  // rather than synthesized — it is the encoder that decides the profile.
  description: Uint8Array | null;
}

export interface AacTranscoderStats {
  state: 'idle' | 'active' | 'unsupported' | 'error';
  packetsIn: number;
  packetsOut: number;
  errors: number;
  codec: string | null;
  detail: string | null;
}

// AAC-LC. `mp4a.40.2` is the RFC 6381 name (object type 0x40, audio object type
// 2) and the one Safari/HLS is specified around.
export const AAC_CODEC = 'mp4a.40.2';
// 128 kbps stereo — matches the R15 Opus lane's bitrate, so the transcode is not
// the quality bottleneck.
export const AAC_BITRATE = 128_000;

// Minimal structural types: lib.dom's WebCodecs audio types exist, but the
// injectable seam has to name them without pulling the real globals into tests.
export interface AudioEncoderLike {
  configure(config: {
    codec: string;
    sampleRate: number;
    numberOfChannels: number;
    bitrate: number;
  }): void;
  encode(data: AudioDataLike): void;
  close(): void;
  readonly state?: string;
}

export interface AudioDataLike {
  close(): void;
}

export interface EncodedChunkLike {
  timestamp: number;
  byteLength: number;
  copyTo(dest: Uint8Array): void;
}

export interface TranscoderDeps {
  createEncoder(callbacks: {
    output: (chunk: EncodedChunkLike, metadata?: { decoderConfig?: { description?: unknown } }) => void;
    error: (err: Error) => void;
  }): AudioEncoderLike;
  createAudioData(init: {
    format: 'f32-planar';
    sampleRate: number;
    numberOfFrames: number;
    numberOfChannels: number;
    timestamp: number;
    data: Float32Array;
  }): AudioDataLike;
}

// The real WebCodecs wiring. Returns null where this scope has no AudioEncoder
// (which is the honest answer, not an error).
export function defaultTranscoderDeps(): TranscoderDeps | null {
  const g = globalThis as {
    AudioEncoder?: new (cb: Parameters<TranscoderDeps['createEncoder']>[0]) => AudioEncoderLike;
    AudioData?: new (init: unknown) => AudioDataLike;
  };
  if (typeof g.AudioEncoder !== 'function' || typeof g.AudioData !== 'function') return null;
  const Encoder = g.AudioEncoder;
  const Data = g.AudioData;
  return {
    createEncoder: (cb) => new Encoder(cb),
    createAudioData: (init) => new Data(init),
  };
}

export class AacTranscoder {
  private deps: TranscoderDeps | null;
  private encoder: AudioEncoderLike | null = null;
  private onOutput: (out: TranscodedAudio) => void;
  private description: Uint8Array | null = null;
  private descriptionSent = false;
  private configured: { sampleRate: number; channels: number } | null = null;
  private stats: AacTranscoderStats = {
    state: 'idle',
    packetsIn: 0,
    packetsOut: 0,
    errors: 0,
    codec: null,
    detail: null,
  };

  constructor(
    onOutput: (out: TranscodedAudio) => void,
    deps: TranscoderDeps | null = defaultTranscoderDeps(),
  ) {
    this.onOutput = onOutput;
    this.deps = deps;
    if (!deps) {
      this.stats.state = 'unsupported';
      this.stats.detail = 'no AudioEncoder in this scope';
    }
  }

  getStats(): AacTranscoderStats {
    return { ...this.stats };
  }

  // Feed one decoded chunk. The encoder is configured lazily from the first
  // chunk's real format — never from the wire config, per the project rule about
  // trusting the frames in hand over metadata.
  push(chunk: TranscodeInput): void {
    if (this.stats.state === 'unsupported' || this.stats.state === 'error') return;
    const deps = this.deps;
    if (!deps) return;
    const channels = chunk.channels.length;
    if (channels < 1 || chunk.frameCount <= 0 || chunk.sampleRate <= 0) return;

    if (!this.ensureEncoder(deps, chunk.sampleRate, channels)) return;

    // f32-planar wants one contiguous buffer, channel after channel.
    const interleavedPlanar = new Float32Array(chunk.frameCount * channels);
    for (let c = 0; c < channels; c++) {
      interleavedPlanar.set(chunk.channels[c].subarray(0, chunk.frameCount), c * chunk.frameCount);
    }
    let data: AudioDataLike;
    try {
      data = deps.createAudioData({
        format: 'f32-planar',
        sampleRate: chunk.sampleRate,
        numberOfFrames: chunk.frameCount,
        numberOfChannels: channels,
        timestamp: chunk.timestampUs,
        data: interleavedPlanar,
      });
    } catch (e) {
      this.fail('AudioData construction failed', e);
      return;
    }
    try {
      this.encoder!.encode(data);
      this.stats.packetsIn++;
    } catch (e) {
      this.fail('encode failed', e);
    } finally {
      // AudioData copies on construction, so the frame is done either way.
      try {
        data.close();
      } catch {
        // best-effort
      }
    }
  }

  close(): void {
    try {
      this.encoder?.close();
    } catch {
      // already closed
    }
    this.encoder = null;
    this.configured = null;
  }

  private ensureEncoder(deps: TranscoderDeps, sampleRate: number, channels: number): boolean {
    const cur = this.configured;
    if (cur && cur.sampleRate === sampleRate && cur.channels === channels) return true;
    // A format change means a new AudioSpecificConfig: rebuild, and let the next
    // output carry the new description to the muxer.
    this.close();
    this.description = null;
    this.descriptionSent = false;
    try {
      const encoder = deps.createEncoder({
        output: (chunk, metadata) => this.handleOutput(chunk, metadata),
        error: (e) => this.fail('encoder error', e),
      });
      encoder.configure({
        codec: AAC_CODEC,
        sampleRate,
        numberOfChannels: channels,
        bitrate: AAC_BITRATE,
      });
      this.encoder = encoder;
      this.configured = { sampleRate, channels };
      this.stats.state = 'active';
      this.stats.codec = AAC_CODEC;
      this.stats.detail = null;
      return true;
    } catch (e) {
      // configure() throwing is how a runtime says "no AAC encoder here" —
      // distinct from a mid-stream failure, and equally non-fatal.
      this.stats.state = 'unsupported';
      this.stats.detail = e instanceof Error ? e.message : String(e);
      log.warn('AAC transcode unavailable:', e);
      return false;
    }
  }

  private handleOutput(
    chunk: EncodedChunkLike,
    metadata?: { decoderConfig?: { description?: unknown } },
  ): void {
    const desc = metadata?.decoderConfig?.description;
    // Normalize here, at the boundary where the encoder's answer enters our
    // world: downstream (the muxer's `esds`) the contract stays "description IS
    // the AudioSpecificConfig", so its golden vectors don't move.
    if (desc && !this.description) {
      this.description = extractAudioSpecificConfig(toBytes(desc));
    }
    // Without an AudioSpecificConfig the muxer cannot build a usable esds, so
    // hold output until the encoder has provided one. WebCodecs delivers it with
    // the first chunk's metadata, so this drops nothing in practice.
    if (!this.description) return;
    const data = new Uint8Array(chunk.byteLength);
    try {
      chunk.copyTo(data);
    } catch (e) {
      this.fail('copyTo failed', e);
      return;
    }
    const description = this.descriptionSent ? null : this.description;
    this.descriptionSent = true;
    this.stats.packetsOut++;
    this.onOutput({ timestampUs: chunk.timestamp, data, description });
  }

  private fail(what: string, e: unknown): void {
    this.stats.errors++;
    this.stats.state = 'error';
    this.stats.detail = `${what}: ${e instanceof Error ? e.message : String(e)}`;
    log.warn(`AAC transcode ${what}:`, e);
    this.close();
  }
}

// ISO 14496-1 descriptor tags, as they appear inside an `esds`.
const TAG_ES_DESCRIPTOR = 0x03;
const TAG_DECODER_CONFIG = 0x04;
const TAG_DECODER_SPECIFIC_INFO = 0x05;

// R22 audio (docs/27 finding 6), measured on iPhone by the device probe: WebCodecs
// says an AAC `decoderConfig.description` is the AudioSpecificConfig, and Chrome
// hands back exactly that (`11 90` for AAC-LC 48 kHz stereo) — but **Safari hands
// back the entire `esds` payload**, a complete ES_Descriptor with the ASC buried
// three levels down. Taken at face value, the muxer nests that whole descriptor
// inside its own DecoderSpecificInfo, and WebKit rejects the resulting init
// segment with MEDIA_ERR_SRC_NOT_SUPPORTED and closes the MediaSource — which is
// how iPhone native fullscreen ended up permanently video-only (the presenter's
// sticky audio drop then made it silent for the rest of the session).
//
// So: unwrap a descriptor when we are handed one, and pass a bare ASC through.
// The discriminator is the leading tag byte — an AudioSpecificConfig's first
// byte carries the 5-bit audioObjectType in its high bits, so a valid one can
// never be 0x03/0x04 (that would mean AOT 0, which is "NULL" and never encoded).
// A shape we don't understand is returned unchanged rather than guessed at; the
// muxer's own guard refuses to build an unusable init segment from it.
export function extractAudioSpecificConfig(desc: Uint8Array): Uint8Array {
  if (desc.length === 0) return desc;
  if (desc[0] !== TAG_ES_DESCRIPTOR && desc[0] !== TAG_DECODER_CONFIG) return desc;
  const found = findDescriptor(desc, 0, desc.length, TAG_DECODER_SPECIFIC_INFO);
  return found ?? desc;
}

// Walk a descriptor list, recursing into the two containers that can hold a
// DecoderSpecificInfo. Sizes use the 7-bits-per-byte continuation encoding, and
// Apple writes the 4-byte long form even for a 2-byte payload.
function findDescriptor(
  buf: Uint8Array,
  start: number,
  end: number,
  want: number,
): Uint8Array | null {
  let off = start;
  while (off < end) {
    const tag = buf[off++];
    let size = 0;
    for (let i = 0; i < 4 && off < end; i++) {
      const b = buf[off++];
      size = (size << 7) | (b & 0x7f);
      if ((b & 0x80) === 0) break;
    }
    const bodyEnd = off + size;
    // A size that runs past the buffer is a malformed descriptor, not something
    // to read off the end of.
    if (size < 0 || bodyEnd > end) return null;
    if (tag === want) return buf.slice(off, bodyEnd);
    if (tag === TAG_ES_DESCRIPTOR) {
      // ES_ID (2) + flags/priority (1); no dependency/URL/OCR fields are used by
      // any AAC encoder we've seen, and the flags byte says so.
      const nested = findDescriptor(buf, off + 3, bodyEnd, want);
      if (nested) return nested;
    } else if (tag === TAG_DECODER_CONFIG) {
      // OTI (1) + streamType byte (1) + bufferSizeDB (3) + max/avg bitrate (4+4).
      const nested = findDescriptor(buf, off + 13, bodyEnd, want);
      if (nested) return nested;
    }
    off = bodyEnd;
  }
  return null;
}

function toBytes(desc: unknown): Uint8Array {
  if (desc instanceof Uint8Array) return desc.slice();
  if (desc instanceof ArrayBuffer) return new Uint8Array(desc.slice(0));
  if (ArrayBuffer.isView(desc)) {
    const v = desc as ArrayBufferView;
    return new Uint8Array(v.buffer.slice(v.byteOffset, v.byteOffset + v.byteLength));
  }
  return new Uint8Array(0);
}
