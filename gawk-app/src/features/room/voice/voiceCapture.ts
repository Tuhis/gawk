// The microphone → Opus path of room voice (R79, docs/80 D21).
//
//   getUserMedia (echo cancellation, noise suppression, auto gain, mono)
//     → frames: the broadcaster audio lane's MediaStreamTrackProcessor pump
//       where the main thread has MSTP (Chromium), else a new AudioWorklet
//       capture node whose PCM voiceFraming.ts cuts into 10 ms AudioData
//       (Firefox; and Safari, whose MSTP exists only in workers — see below)
//     → one TalkGate decision per frame (voice activity / push-to-talk / mute)
//     → AudioEncoder (Opus, 40 kbit/s, 10 ms frames where the browser takes
//       them, else 20 ms) only while the gate says talk
//     → onPacket, and onSpurtEnd once the encoder has drained a spurt.
//
// Safari: docs/80 D21 planned the worker MSTP there. Moving the encoder into
// a worker for one 40 kbit/s mono stream buys nothing the worklet path does
// not already give, and costs a second transport of every packet across the
// worker boundary, so Safari takes the worklet path too. MV0 measures both.
//
// Listen-only never constructs one of these: no getUserMedia, no prompt, no
// mic indicator.
//
// Every failure releases what start() acquired (the track first: a leaked
// track is a lit mic indicator) and surfaces as a 'capture' VoiceError.

import { log } from '../../../lib/logger';
import { VoiceError } from './voiceSession';
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_PROCESSOR_SOURCE,
  PcmBlocker,
  blockAudioDataInit,
} from './voiceFraming';
import { blockRms } from './talkGate';

export const VOICE_BITRATE_BPS = 40_000;
export const VOICE_SAMPLE_RATE = 48_000;
// Opus frame durations, µs: 10 ms is the target (docs/80 OD31); 20 ms is the
// fallback for a browser that refuses it.
export const VOICE_FRAME_US = 10_000;
export const VOICE_FALLBACK_FRAME_US = 20_000;
// One VoiceFrame seq unit (wire.ts: seq counts 10 ms).
export const VOICE_SEQ_UNIT_US = 10_000;

export const VOICE_AUDIO_CONSTRAINTS: MediaTrackConstraints = {
  echoCancellation: true,
  noiseSuppression: true,
  autoGainControl: true,
  channelCount: 1,
};

export interface VoiceEncoderConfig {
  codec: 'opus';
  sampleRate: number;
  numberOfChannels: number;
  bitrate: number;
  opus: { frameDuration: number; complexity: number; usedtx: boolean };
}

export function voiceEncoderConfig(sampleRate: number, numberOfChannels: number, frameUs: number): VoiceEncoderConfig {
  return {
    codec: 'opus',
    sampleRate,
    numberOfChannels,
    bitrate: VOICE_BITRATE_BPS,
    // DTX off: the gate already sends nothing between spurts, and inside one
    // a constant 10 ms cadence keeps the seq arithmetic exact.
    opus: { frameDuration: frameUs, complexity: 8, usedtx: false },
  };
}

// The 10 ms config where the browser takes it, else 20 ms, else null.
export async function chooseFrameDuration(
  isConfigSupported: (c: VoiceEncoderConfig) => Promise<{ supported?: boolean }>,
): Promise<number | null> {
  for (const frameUs of [VOICE_FRAME_US, VOICE_FALLBACK_FRAME_US]) {
    try {
      const r = await isConfigSupported(voiceEncoderConfig(VOICE_SAMPLE_RATE, 1, frameUs));
      if (r.supported) return frameUs;
    } catch {
      // a config the browser cannot even parse is unsupported
    }
  }
  return null;
}

// Seq units a packet covers: its duration in 10 ms, at least one.
export function seqUnitsFor(durationUs: number | null | undefined, fallbackUs: number): number {
  const d = typeof durationUs === 'number' && durationUs > 0 ? durationUs : fallbackUs;
  return Math.max(1, Math.round(d / VOICE_SEQ_UNIT_US));
}

// The slices of the WebCodecs / media APIs the capture touches, injectable so
// it unit-tests without a browser.
export interface VoiceAudioData {
  readonly timestamp: number;
  readonly sampleRate: number;
  readonly numberOfChannels: number;
  readonly numberOfFrames: number;
  copyTo(dst: Float32Array, opts: { planeIndex: number; format: 'f32-planar' }): void;
  close(): void;
}

export interface VoiceEncoderLike {
  encode(data: VoiceAudioData): void;
  flush(): Promise<void>;
  close(): void;
}

export interface VoiceCaptureDeps {
  getUserMedia: (c: MediaStreamConstraints) => Promise<MediaStream>;
  isConfigSupported: (c: VoiceEncoderConfig) => Promise<{ supported?: boolean }>;
  createEncoder: (
    config: VoiceEncoderConfig,
    cb: { output: (data: Uint8Array, durationUs: number | null) => void; error: (e: Error) => void },
  ) => VoiceEncoderLike;
  // Main-thread MediaStreamTrackProcessor, or null where it does not exist.
  createProcessor: ((track: MediaStreamTrack) => ReadableStream<VoiceAudioData>) | null;
  createAudioData: (init: AudioDataInit) => VoiceAudioData;
  now: () => number;
}

export interface VoiceCaptureCallbacks {
  // One frame of captured PCM, its RMS: whether to send it (TalkGate).
  shouldSend: (rms: number, nowMs: number) => boolean;
  onPacket: (payload: Uint8Array, seqUnits: number) => void;
  // The spurt's last packet has gone out (the encoder drained).
  onSpurtEnd: () => void;
  // The capture died after start (encoder error, track ended). Fired once;
  // everything is already released.
  onError: (err: VoiceError) => void;
}

// The AudioContext the worklet path runs in: the room's shared one.
export interface CaptureContext {
  context: AudioContext;
}

export interface VoiceCaptureLike {
  start(ctx: () => CaptureContext | null): Promise<void>;
  stop(): void;
}

export function voiceCaptureSupported(): boolean {
  const g = globalThis as { MediaStreamTrackProcessor?: unknown; AudioWorkletNode?: unknown };
  return (
    typeof navigator !== 'undefined' &&
    typeof navigator.mediaDevices?.getUserMedia === 'function' &&
    typeof AudioEncoder === 'function' &&
    (typeof g.MediaStreamTrackProcessor === 'function' ||
      (typeof g.AudioWorkletNode === 'function' && typeof AudioData === 'function'))
  );
}

export function realCaptureDeps(): VoiceCaptureDeps {
  const g = globalThis as {
    MediaStreamTrackProcessor?: new (init: { track: MediaStreamTrack }) => { readable: ReadableStream<AudioData> };
  };
  const Mstp = g.MediaStreamTrackProcessor;
  return {
    getUserMedia: (c) => navigator.mediaDevices.getUserMedia(c),
    isConfigSupported: (c) => AudioEncoder.isConfigSupported(c as AudioEncoderConfig),
    createEncoder: (config, cb) => {
      const enc = new AudioEncoder({
        output: (chunk) => {
          const payload = new Uint8Array(chunk.byteLength);
          chunk.copyTo(payload);
          cb.output(payload, chunk.duration ?? null);
        },
        error: (e) => cb.error(e instanceof Error ? e : new Error(String(e))),
      });
      enc.configure(config as AudioEncoderConfig);
      return {
        encode: (d) => enc.encode(d as unknown as AudioData),
        flush: () => enc.flush(),
        close: () => {
          if (enc.state !== 'closed') enc.close();
        },
      };
    },
    createProcessor: Mstp
      ? (track) => new Mstp({ track }).readable as unknown as ReadableStream<VoiceAudioData>
      : null,
    createAudioData: (init) => new AudioData(init) as unknown as VoiceAudioData,
    now: () => performance.now(),
  };
}

// Registered once per context: a second registration of the name throws.
const registered = new WeakMap<BaseAudioContext, Promise<void>>();

function registerCaptureWorklet(ctx: AudioContext): Promise<void> {
  let p = registered.get(ctx);
  if (!p) {
    const url = URL.createObjectURL(new Blob([CAPTURE_PROCESSOR_SOURCE], { type: 'application/javascript' }));
    p = ctx.audioWorklet.addModule(url).finally(() => URL.revokeObjectURL(url));
    p.catch(() => registered.delete(ctx));
    registered.set(ctx, p);
  }
  return p;
}

export class VoiceCapture implements VoiceCaptureLike {
  private readonly deps: VoiceCaptureDeps;
  private readonly cb: VoiceCaptureCallbacks;
  private stream: MediaStream | null = null;
  private encoder: VoiceEncoderLike | null = null;
  private frameUs = VOICE_FRAME_US;
  private reader: ReadableStreamDefaultReader<VoiceAudioData> | null = null;
  private nodes: AudioNode[] = [];
  private port: MessagePort | null = null;
  private sending = false;
  private stopped = false;
  private failed = false;

  constructor(cb: VoiceCaptureCallbacks, deps: VoiceCaptureDeps = realCaptureDeps()) {
    this.cb = cb;
    this.deps = deps;
  }

  async start(getContext: () => CaptureContext | null): Promise<void> {
    try {
      const frameUs = await chooseFrameDuration(this.deps.isConfigSupported);
      if (frameUs === null) throw new VoiceError('capture', 'This browser can’t encode voice (no Opus encoder).');
      this.frameUs = frameUs;
      let stream: MediaStream;
      try {
        stream = await this.deps.getUserMedia({ audio: VOICE_AUDIO_CONSTRAINTS, video: false });
      } catch (e) {
        const name = (e as { name?: string } | null)?.name;
        throw new VoiceError(
          'capture',
          name === 'NotAllowedError'
            ? 'Microphone access was refused.'
            : name === 'NotFoundError'
              ? 'No microphone was found.'
              : `The microphone could not be opened${e instanceof Error && e.message ? `: ${e.message}` : '.'}`,
        );
      }
      this.stream = stream;
      if (this.stopped) throw new VoiceError('capture', 'voice capture stopped while starting');
      const track = stream.getAudioTracks()[0];
      if (!track) throw new VoiceError('capture', 'The microphone delivered no audio track.');
      track.addEventListener('ended', () => this.fail(new VoiceError('capture', 'The microphone stopped.')));
      if (this.deps.createProcessor) {
        this.pumpProcessor(this.deps.createProcessor(track));
      } else {
        await this.startWorklet(stream, getContext());
      }
      if (this.stopped) throw new VoiceError('capture', 'voice capture stopped while starting');
    } catch (e) {
      this.release();
      throw e instanceof VoiceError ? e : new VoiceError('capture', e instanceof Error ? e.message : String(e));
    }
  }

  stop(): void {
    this.stopped = true;
    this.release();
  }

  // ---- Frame sources ---------------------------------------------------------

  private pumpProcessor(readable: ReadableStream<VoiceAudioData>): void {
    const reader = readable.getReader();
    this.reader = reader;
    void (async () => {
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done || this.stopped) {
            value?.close();
            break;
          }
          if (value) this.handleAudioData(value);
        }
      } catch (e) {
        if (!this.stopped) log.warn('voice capture pump ended:', e);
      }
    })();
  }

  private async startWorklet(stream: MediaStream, out: CaptureContext | null): Promise<void> {
    if (!out) throw new VoiceError('capture', 'Web Audio is unavailable here.');
    const ctx = out.context;
    await registerCaptureWorklet(ctx);
    if (this.stopped) return;
    const source = ctx.createMediaStreamSource(stream);
    const node = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME, {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      outputChannelCount: [1],
    });
    // Pulled through a muted gain so the engine renders the node; the mic is
    // never played back.
    const sink = ctx.createGain();
    sink.gain.value = 0;
    source.connect(node);
    node.connect(sink);
    sink.connect(ctx.destination);
    this.nodes = [source, node, sink];
    const blocker = new PcmBlocker(ctx.sampleRate);
    this.port = node.port;
    node.port.onmessage = (e: MessageEvent<Float32Array>) => {
      if (this.stopped || !(e.data instanceof Float32Array)) return;
      for (const block of blocker.push(e.data)) this.handleBlock(block.pcm, ctx.sampleRate, block.timestampUs);
    };
  }

  // The worklet path: one 10 ms block of mono PCM.
  handleBlock(pcm: Float32Array, sampleRate: number, timestampUs: number): void {
    if (this.stopped) return;
    const send = this.gate(blockRms(pcm));
    if (!send) return;
    let data: VoiceAudioData;
    try {
      data = this.deps.createAudioData(blockAudioDataInit(pcm, sampleRate, timestampUs));
    } catch (e) {
      this.fail(new VoiceError('capture', `voice capture: ${e instanceof Error ? e.message : String(e)}`));
      return;
    }
    this.encode(data);
  }

  // The MSTP path: one AudioData as the track delivered it.
  private handleAudioData(data: VoiceAudioData): void {
    if (this.stopped) {
      data.close();
      return;
    }
    const plane = new Float32Array(data.numberOfFrames);
    try {
      data.copyTo(plane, { planeIndex: 0, format: 'f32-planar' });
    } catch {
      // A format the copy refuses: let the gate see silence rather than die.
      plane.fill(0);
    }
    if (!this.gate(blockRms(plane))) {
      data.close();
      return;
    }
    this.encode(data);
  }

  // ---- Gate and encoder -------------------------------------------------------

  private gate(rms: number): boolean {
    const send = this.cb.shouldSend(rms, this.deps.now());
    if (this.sending && !send) this.endSpurt();
    this.sending = send;
    return send;
  }

  private encode(data: VoiceAudioData): void {
    try {
      // Configured from the first frame in hand: its rate and channel count
      // are the truth, whatever the constraints asked for.
      this.encoder ??= this.deps.createEncoder(
        voiceEncoderConfig(data.sampleRate, data.numberOfChannels, this.frameUs),
        {
          output: (payload, durationUs) => {
            if (!this.stopped && payload.length > 0) this.cb.onPacket(payload, seqUnitsFor(durationUs, this.frameUs));
          },
          error: (e) => this.fail(new VoiceError('capture', `voice encoder: ${e.message}`)),
        },
      );
      this.encoder.encode(data);
    } catch (e) {
      this.fail(new VoiceError('capture', `voice encoder: ${e instanceof Error ? e.message : String(e)}`));
    } finally {
      data.close();
    }
  }

  // The spurt is over: drain the encoder so its last packets go out before
  // the terminator. A new spurt's encodes queue behind the flush, so they
  // cannot overtake it.
  private endSpurt(): void {
    const enc = this.encoder;
    if (!enc) {
      this.cb.onSpurtEnd();
      return;
    }
    enc.flush().then(
      () => {
        if (!this.stopped) this.cb.onSpurtEnd();
      },
      () => {
        // a closed encoder: stop() or fail() already said everything
      },
    );
  }

  private fail(err: VoiceError): void {
    if (this.stopped || this.failed) return;
    this.failed = true;
    this.stopped = true;
    this.release();
    this.cb.onError(err);
  }

  // The one teardown, for every ending.
  private release(): void {
    const stream = this.stream;
    this.stream = null;
    for (const t of stream?.getTracks() ?? []) {
      try {
        t.stop();
      } catch {
        // already stopped
      }
    }
    const reader = this.reader;
    this.reader = null;
    void reader?.cancel().catch(() => {});
    if (this.port) this.port.onmessage = null;
    this.port = null;
    for (const n of this.nodes) {
      try {
        n.disconnect();
      } catch {
        // already disconnected
      }
    }
    this.nodes = [];
    const enc = this.encoder;
    this.encoder = null;
    try {
      enc?.close();
    } catch {
      // already closed
    }
  }
}

