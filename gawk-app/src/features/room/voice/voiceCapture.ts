// The microphone → Opus path of room voice (R79, docs/80 D21; R80, docs/81).
//
//   getUserMedia (echo cancellation and auto gain on; the browser's noise
//   suppression only in the Browser mode, docs/81 D11; mono)
//     → the gawk-voice-capture AudioWorklet on the room's context, on every
//       browser (docs/81 D5): mono, resampled to 48 kHz, 10 ms blocks, and
//       denoised by RNNoise with its speech probability while that is on
//       (voiceFraming.ts)
//     → one TalkGate decision per block (speech probability or level /
//       push-to-talk / mute)
//     → AudioEncoder (Opus, 40 kbit/s, 10 ms frames where the browser takes
//       them, else 20 ms) only while the gate says talk
//     → onPacket, and onSpurtEnd once the encoder has drained a spurt.
//
// Noise suppression (docs/81 D11, D13): RNNoise and Off run on the same
// track and switch inside the worklet; a switch to or from Browser re-opens
// the microphone inside the running capture (old track stopped first), and
// only when the microphone permission is already granted, so it never
// prompts. Otherwise the choice waits for the next join. Where the browser
// has no noiseSuppression constraint (WebKit) Browser and Off are the same
// mode and nothing re-opens. RNNoise never blocks voice: no SIMD, a failed
// download or compile, an instantiate error or the watchdog falls back to
// Browser (granted) or Off, for the rest of the capture, with the reason.
//
// Listen-only never constructs one of these: no getUserMedia, no prompt, no
// mic indicator, no wasm download.
//
// Every failure releases what start() acquired (the track first: a leaked
// track is a lit mic indicator) and surfaces as a 'capture' VoiceError.

import { log } from '../../../lib/logger';
import { VoiceError } from './voiceSession';
import {
  CAPTURE_PROCESSOR_NAME,
  CAPTURE_PROCESSOR_SOURCE,
  VOICE_BLOCK_MS,
  VOICE_CAPTURE_RATE,
  blockAudioDataInit,
  type CaptureProcessorOptions,
  type CaptureWorkletCommand,
  type CaptureWorkletEvent,
} from './voiceFraming';
import { blockRms } from './talkGate';
import { compileRnnoise, wasmSimdSupported } from './rnnoise';
import type { NoiseMode } from './voicePrefs';

export const VOICE_BITRATE_BPS = 40_000;
export const VOICE_SAMPLE_RATE = VOICE_CAPTURE_RATE;
// Opus frame durations, µs: 10 ms is the target (docs/80 OD31); 20 ms is the
// fallback for a browser that refuses it.
export const VOICE_FRAME_US = 10_000;
export const VOICE_FALLBACK_FRAME_US = 20_000;
// One VoiceFrame seq unit (wire.ts: seq counts 10 ms).
export const VOICE_SEQ_UNIT_US = 10_000;

// docs/81 D11: echo cancellation always; the browser's own suppression only
// in the Browser mode (before RNNoise it leaves 16 dB more keyboard, NS0
// §12.4). docs/82 D5: auto gain only in the Browser mode too — in a pause it
// lifts a hum or a TV to the speaker's own level, where no level floor can
// tell them apart (VX0 §12); the gain after the gate (D6) does its job.
// RNNoise and Off keep one set of constraints, so switching between them
// stays the worklet's alone.
export function voiceAudioConstraints(mode: NoiseMode): MediaTrackConstraints {
  return {
    echoCancellation: true,
    noiseSuppression: mode === 'browser',
    autoGainControl: mode === 'browser',
    channelCount: 1,
  };
}

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
  createAudioData: (init: AudioDataInit) => VoiceAudioData;
  // docs/81 D13: synchronous, before the first getUserMedia.
  wasmSimd: () => boolean;
  // Fetch and compile rnnoise.wasm; rejects with a reason to show.
  loadRnnoise: () => Promise<WebAssembly.Module>;
  // Whether the microphone permission is already 'granted' (a throw or any
  // other answer is not): a re-open must never prompt (D11).
  micPermissionGranted: () => Promise<boolean>;
  // Whether the browser has a noiseSuppression constraint at all (WebKit
  // does not: Browser and Off are the same there, D12, D15).
  suppressionConstraint: () => boolean;
}

// What the capture runs, for the Audio section (docs/81 D13, D15).
export interface NoiseStatus {
  // The mode the user chose (saved, docs/81 D17).
  requested: NoiseMode;
  // The mode the microphone runs in now.
  running: NoiseMode;
  // RNNoise is running but its module is not in the worklet yet: the level
  // gate decides meanwhile.
  loading: boolean;
  // Why RNNoise is not running although it was chosen, or null.
  fallback: string | null;
  // The choice could not apply without a permission prompt: it applies at
  // the next join.
  pending: boolean;
  // Whether Off is a mode of its own here (the browser has a
  // noiseSuppression constraint).
  offAvailable: boolean;
}

export interface VoiceCaptureCallbacks {
  // One 10 ms block: its RMS and RNNoise's speech probability (null when the
  // block was not denoised): whether to send it (TalkGate).
  shouldSend: (rms: number, prob: number | null, nowMs: number) => boolean;
  onPacket: (payload: Uint8Array, seqUnits: number) => void;
  // The spurt's last packet has gone out (the encoder drained).
  onSpurtEnd: () => void;
  // The capture died after start (encoder error, track ended). Fired once;
  // everything is already released.
  onError: (err: VoiceError) => void;
  // The noise suppression changed (a switch, a fallback, RNNoise loaded).
  onNoise: (status: NoiseStatus) => void;
  // docs/82 D6: the gain for blocks the gate passed, dB (asked after
  // shouldSend, so the gate always judges the block as captured).
  gainDb?: () => number;
}

// The AudioContext the worklet runs in: the room's shared one.
export interface CaptureContext {
  context: AudioContext;
}

export interface VoiceCaptureLike {
  start(ctx: () => CaptureContext | null): Promise<void>;
  // A mode chosen mid-call (docs/81 D11).
  setNoise(mode: NoiseMode): Promise<void>;
  stop(): void;
}

export function voiceCaptureSupported(): boolean {
  const g = globalThis as { AudioWorkletNode?: unknown; AudioData?: unknown };
  return (
    typeof navigator !== 'undefined' &&
    typeof navigator.mediaDevices?.getUserMedia === 'function' &&
    typeof AudioEncoder === 'function' &&
    typeof g.AudioWorkletNode === 'function' &&
    typeof g.AudioData === 'function'
  );
}

// docs/81 D11: the same reading as features/pop/permission.ts — anything but
// 'granted', a throw included, is not granted.
export async function queryMicPermission(
  permissions: { query(d: { name: string }): Promise<{ state?: string }> } | undefined = typeof navigator !==
  'undefined'
    ? (navigator as Navigator & { permissions?: { query(d: { name: string }): Promise<PermissionStatus> } })
        .permissions
    : undefined,
): Promise<boolean> {
  if (!permissions || typeof permissions.query !== 'function') return false;
  try {
    return (await permissions.query({ name: 'microphone' }))?.state === 'granted';
  } catch {
    return false;
  }
}

export function realCaptureDeps(): VoiceCaptureDeps {
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
    createAudioData: (init) => new AudioData(init) as unknown as VoiceAudioData,
    wasmSimd: wasmSimdSupported,
    loadRnnoise: () => compileRnnoise(),
    micPermissionGranted: () => queryMicPermission(),
    suppressionConstraint: () => {
      try {
        return navigator.mediaDevices.getSupportedConstraints?.().noiseSuppression === true;
      } catch {
        return false;
      }
    },
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

function micOpenError(e: unknown, what = 'opened'): VoiceError {
  const name = (e as { name?: string } | null)?.name;
  return new VoiceError(
    'capture',
    name === 'NotAllowedError'
      ? 'Microphone access was refused.'
      : name === 'NotFoundError'
        ? 'No microphone was found.'
        : `The microphone could not be ${what}${e instanceof Error && e.message ? `: ${e.message}` : '.'}`,
  );
}

function reasonOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export class VoiceCapture implements VoiceCaptureLike {
  private readonly deps: VoiceCaptureDeps;
  private readonly cb: VoiceCaptureCallbacks;
  private stream: MediaStream | null = null;
  private track: MediaStreamTrack | null = null;
  private encoder: VoiceEncoderLike | null = null;
  private frameUs = VOICE_FRAME_US;
  private context: AudioContext | null = null;
  private source: AudioNode | null = null;
  private node: AudioWorkletNode | null = null;
  private sink: AudioNode | null = null;
  private port: MessagePort | null = null;
  // Blocks handed to the gate: the 48 kHz clock the AudioData timestamps
  // count (10 ms each), across re-opens and node rebuilds.
  private blocks = 0;
  private sending = false;
  // The gain the last sent block ended at, linear (docs/82 D6).
  private gainLin: number | null = null;
  private stopped = false;
  private failed = false;

  // Noise suppression state (docs/81 D11, D13).
  private requested: NoiseMode;
  private running: NoiseMode;
  private offAvailable = true;
  private rnnoiseReady = false;
  private fallback: string | null = null;
  private pending = false;
  private module: WebAssembly.Module | null = null;
  // Whether the current node was built with the module.
  private nodeHasModule = false;
  private moduleRequested = false;
  // Resolves once start() has the microphone and the worklet up; rejects if
  // it never does. Mode changes wait for it.
  private opened: Promise<void>;
  private resolveOpened!: () => void;
  private rejectOpened!: (e: unknown) => void;
  // Mode changes and fallbacks run one at a time: a re-open is async.
  private serial: Promise<void> = Promise.resolve();

  constructor(cb: VoiceCaptureCallbacks, deps: VoiceCaptureDeps = realCaptureDeps(), noise: NoiseMode = 'rnnoise') {
    this.cb = cb;
    this.deps = deps;
    this.requested = noise;
    this.running = noise;
    this.opened = new Promise<void>((res, rej) => {
      this.resolveOpened = res;
      this.rejectOpened = rej;
    });
    this.opened.catch(() => {});
  }

  async start(getContext: () => CaptureContext | null): Promise<void> {
    try {
      const frameUs = await chooseFrameDuration(this.deps.isConfigSupported);
      if (frameUs === null) throw new VoiceError('capture', 'This browser can’t encode voice (no Opus encoder).');
      this.frameUs = frameUs;
      this.offAvailable = this.deps.suppressionConstraint();
      this.running = this.effective(this.requested);
      if (this.running === 'rnnoise' && !this.deps.wasmSimd()) {
        // Before the prompt, so the microphone opens as Browser (D13).
        this.running = 'browser';
        this.fallback = 'this browser has no WebAssembly SIMD';
      }
      // The download starts with the prompt, not after it (D4).
      if (this.running === 'rnnoise') this.requestModule();
      let stream: MediaStream;
      try {
        stream = await this.deps.getUserMedia({ audio: voiceAudioConstraints(this.running), video: false });
      } catch (e) {
        throw micOpenError(e);
      }
      this.stream = stream;
      if (this.stopped) throw new VoiceError('capture', 'voice capture stopped while starting');
      const track = stream.getAudioTracks()[0];
      if (!track) throw new VoiceError('capture', 'The microphone delivered no audio track.');
      this.watchTrack(track);
      const out = getContext();
      if (!out) throw new VoiceError('capture', 'Web Audio is unavailable here.');
      this.context = out.context;
      await registerCaptureWorklet(out.context);
      if (this.stopped) throw new VoiceError('capture', 'voice capture stopped while starting');
      this.buildGraph(stream);
      this.emitNoise();
      this.resolveOpened();
    } catch (e) {
      this.rejectOpened(e);
      this.release();
      throw e instanceof VoiceError ? e : new VoiceError('capture', reasonOf(e));
    }
  }

  stop(): void {
    this.stopped = true;
    this.release();
  }

  // ---- Noise suppression --------------------------------------------------------

  setNoise(mode: NoiseMode): Promise<void> {
    this.requested = mode;
    return this.enqueue(() => this.applyNoise(mode));
  }

  noiseStatus(): NoiseStatus {
    return {
      requested: this.requested,
      running: this.running,
      loading: this.running === 'rnnoise' && !this.rnnoiseReady,
      fallback: this.fallback,
      pending: this.pending,
      offAvailable: this.offAvailable,
    };
  }

  // Off is Browser where the browser has no suppression to turn off.
  private effective(mode: NoiseMode): NoiseMode {
    return mode === 'off' && !this.offAvailable ? 'browser' : mode;
  }

  // Whether going from one mode to another changes the constraints, which
  // only a re-open can do (Chromium ignores applyConstraints for it, §12.6).
  private needsReopen(from: NoiseMode, to: NoiseMode): boolean {
    return this.offAvailable && (from === 'browser') !== (to === 'browser');
  }

  private enqueue(fn: () => Promise<void>): Promise<void> {
    const next = this.serial.then(async () => {
      try {
        await this.opened;
      } catch {
        return;
      }
      if (this.stopped) return;
      await fn();
    });
    this.serial = next.catch(() => {});
    return next;
  }

  private async applyNoise(mode: NoiseMode): Promise<void> {
    if (mode !== this.requested) return; // a later choice supersedes this one
    const target = this.effective(mode);
    this.pending = false;
    if (target === 'rnnoise' && this.fallback !== null) {
      // RNNoise failed this capture: it gets its next chance at the next
      // join (D13).
      this.pending = this.running !== 'rnnoise';
      this.emitNoise();
      return;
    }
    if (target === this.running) {
      this.emitNoise();
      return;
    }
    if (this.needsReopen(this.running, target)) {
      if (!(await this.deps.micPermissionGranted())) {
        if (this.stopped || mode !== this.requested) return;
        this.pending = true;
        this.emitNoise();
        return;
      }
      if (this.stopped || mode !== this.requested) return;
      await this.reopen(target);
      return;
    }
    this.switchInWorklet(target);
  }

  // RNNoise ↔ Off, or any switch where nothing re-opens: the same track.
  private switchInWorklet(target: NoiseMode): void {
    this.running = target;
    this.rnnoiseReady = false;
    if (target === 'rnnoise') this.requestModule();
    this.post({ t: 'denoise', on: target === 'rnnoise' });
    this.ensureModuleInNode();
    this.emitNoise();
  }

  // docs/81 D11, §10: stop the old track first (Chromium hands a second track
  // on an open microphone the first one's processing), then getUserMedia with
  // the new constraints, then splice the new source into the running worklet.
  // The encoder, the worklet and the block clock all carry on. A re-open that
  // fails ends the capture as a lost microphone does.
  private async reopen(target: NoiseMode): Promise<void> {
    const ctx = this.context;
    if (!ctx || !this.node) return;
    this.unwatchTrack();
    this.stopTracks(this.stream);
    this.stream = null;
    this.disconnect(this.source);
    this.source = null;
    let stream: MediaStream;
    try {
      stream = await this.deps.getUserMedia({ audio: voiceAudioConstraints(target), video: false });
    } catch (e) {
      this.fail(micOpenError(e, 're-opened'));
      return;
    }
    if (this.stopped) {
      this.stopTracks(stream);
      return;
    }
    this.stream = stream;
    const track = stream.getAudioTracks()[0];
    if (!track) {
      this.fail(new VoiceError('capture', 'The microphone delivered no audio track.'));
      return;
    }
    this.watchTrack(track);
    // The node as it is now: the module landing or a processor error during
    // the await may have swapped it (review round 1).
    const node = this.node;
    if (!node) return;
    try {
      const source = ctx.createMediaStreamSource(stream);
      source.connect(node);
      this.source = source;
    } catch (e) {
      this.fail(new VoiceError('capture', `voice capture: ${reasonOf(e)}`));
      return;
    }
    this.switchInWorklet(target);
  }

  private requestModule(): void {
    if (this.moduleRequested) return;
    this.moduleRequested = true;
    this.deps.loadRnnoise().then(
      (m) => {
        if (this.stopped) return;
        this.module = m;
        this.ensureModuleInNode();
      },
      (e: unknown) => {
        if (!this.stopped) void this.fallBack(reasonOf(e));
      },
    );
  }

  // docs/81 D13: RNNoise failed after the microphone opened. Browser by a
  // re-open where that cannot prompt, else Off on the open track; Browser on
  // WebKit, where the two are the same. Once per capture.
  private fallBack(reason: string): Promise<void> {
    return this.enqueue(async () => {
      if (this.fallback !== null) return;
      log.warn(`voice: RNNoise unavailable (${reason}); falling back`);
      this.fallback = reason;
      this.rnnoiseReady = false;
      this.post({ t: 'denoise', on: false });
      if (this.running !== 'rnnoise') {
        this.emitNoise();
        return;
      }
      if (this.offAvailable && (await this.deps.micPermissionGranted())) {
        if (this.stopped) return;
        await this.reopen('browser');
        return;
      }
      this.switchInWorklet(this.offAvailable ? 'off' : 'browser');
    });
  }

  private emitNoise(): void {
    if (!this.stopped) this.cb.onNoise(this.noiseStatus());
  }

  // ---- The worklet ---------------------------------------------------------------

  private buildGraph(stream: MediaStream): void {
    const ctx = this.context!;
    const source = ctx.createMediaStreamSource(stream);
    const node = this.createNode(ctx, this.running === 'rnnoise');
    // Pulled through a muted gain so the engine renders the node; the mic is
    // never played back.
    const sink = ctx.createGain();
    sink.gain.value = 0;
    source.connect(node);
    node.connect(sink);
    sink.connect(ctx.destination);
    this.source = source;
    this.sink = sink;
  }

  // The module, when compiled, rides processorOptions (D5): Chromium ≤ 154
  // cannot post one through the port.
  private createNode(ctx: AudioContext, denoise: boolean): AudioWorkletNode {
    const opts: CaptureProcessorOptions = { denoise, module: this.module };
    this.nodeHasModule = this.module !== null;
    const node = new AudioWorkletNode(ctx, CAPTURE_PROCESSOR_NAME, {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      outputChannelCount: [1],
      processorOptions: opts,
    });
    this.node = node;
    this.port = node.port;
    node.port.onmessage = (e: MessageEvent<CaptureWorkletEvent>) => this.onWorklet(e.data);
    // A processor that throws is dead for good: build a plain one in its
    // place and fall back (D13).
    node.onprocessorerror = () => this.replaceNode();
    return node;
  }

  // A new node in the old one's place between source and sink. The encoder
  // and the block clock carry on; the old node frees its RNNoise state.
  private swapNode(denoise: boolean): boolean {
    const ctx = this.context;
    const old = this.node;
    if (this.stopped || !ctx || !old) return false;
    try {
      old.port.postMessage({ t: 'close' } satisfies CaptureWorkletCommand);
    } catch {
      // a dead processor's port
    }
    old.port.onmessage = null;
    old.onprocessorerror = null;
    this.disconnect(old);
    // disconnect() drops only the old node's own outputs: the source would
    // otherwise keep the old processor, and its wasm memory, alive.
    try {
      this.source?.disconnect(old);
    } catch {
      // not connected
    }
    this.rnnoiseReady = false;
    const node = this.createNode(ctx, denoise);
    try {
      this.source?.connect(node);
      if (this.sink) node.connect(this.sink);
    } catch (e) {
      this.fail(new VoiceError('capture', `voice capture: ${reasonOf(e)}`));
      return false;
    }
    return true;
  }

  // RNNoise is running and its module has landed, but the node predates it.
  private ensureModuleInNode(): void {
    if (this.running === 'rnnoise' && this.module && !this.nodeHasModule) this.swapNode(true);
  }

  private replaceNode(): void {
    if (this.stopped || !this.node) return;
    log.warn('voice capture worklet failed; rebuilding it without RNNoise');
    if (!this.swapNode(false)) return;
    if (this.running === 'rnnoise') void this.fallBack('the audio worklet failed');
  }

  private onWorklet(m: CaptureWorkletEvent | null | undefined): void {
    if (this.stopped || !m) return;
    switch (m.t) {
      case 'block':
        if (m.pcm instanceof Float32Array) this.handleBlock(m.pcm, m.prob >= 0 ? m.prob : null);
        return;
      case 'ready':
        if (this.running === 'rnnoise' && !this.rnnoiseReady) {
          this.rnnoiseReady = true;
          this.emitNoise();
        }
        return;
      case 'error':
        void this.fallBack(`it failed to start (${m.reason})`);
        return;
      case 'slow':
        void this.fallBack(`it is too slow on this device (${Math.round(m.load * 100)} % of real time)`);
        return;
    }
  }

  private post(cmd: CaptureWorkletCommand): void {
    try {
      this.port?.postMessage(cmd);
    } catch (e) {
      log.warn('voice capture worklet message failed:', e);
    }
  }

  // One 10 ms, 48 kHz block of mono PCM, denoised when prob is not null.
  handleBlock(pcm: Float32Array, prob: number | null): void {
    if (this.stopped) return;
    const timestampUs = this.blocks * VOICE_BLOCK_MS * 1000;
    const send = this.gate(blockRms(pcm), prob);
    this.blocks++;
    if (!send) return;
    this.applyGain(pcm);
    let data: VoiceAudioData;
    try {
      data = this.deps.createAudioData(blockAudioDataInit(pcm, VOICE_CAPTURE_RATE, timestampUs));
    } catch (e) {
      this.fail(new VoiceError('capture', `voice capture: ${reasonOf(e)}`));
      return;
    }
    this.encode(data);
  }

  // docs/82 D6, in place: ramped across the block from the last block's gain
  // so a change never steps, and clipped to ±1.
  private applyGain(pcm: Float32Array): void {
    const target = 10 ** ((this.cb.gainDb?.() ?? 0) / 20);
    const from = this.gainLin ?? target;
    this.gainLin = target;
    if (from === 1 && target === 1) return;
    const step = (target - from) / pcm.length;
    for (let i = 0; i < pcm.length; i++) {
      const v = pcm[i] * (from + step * i);
      pcm[i] = v > 1 ? 1 : v < -1 ? -1 : v;
    }
  }

  // ---- Gate and encoder -------------------------------------------------------

  private gate(rms: number, prob: number | null): boolean {
    // Audio time, not arrival time: blocks reach the main thread in bursts
    // behind React and GC, and the gate's hold must count audio as NS0's
    // probGate counts blocks (review round 1).
    const send = this.cb.shouldSend(rms, prob, this.blocks * VOICE_BLOCK_MS);
    if (this.sending && !send) this.endSpurt();
    this.sending = send;
    return send;
  }

  private encode(data: VoiceAudioData): void {
    try {
      // Configured from the first block in hand: always 48 kHz mono now
      // (docs/81 D6), and never reconfigured.
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
      this.fail(new VoiceError('capture', `voice encoder: ${reasonOf(e)}`));
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

  // ---- Teardown -------------------------------------------------------------------

  private readonly onTrackEnded = () => this.fail(new VoiceError('capture', 'The microphone stopped.'));

  private watchTrack(track: MediaStreamTrack): void {
    this.track = track;
    track.addEventListener('ended', this.onTrackEnded);
  }

  // Off before a deliberate stop(), so a re-open is not a lost microphone.
  private unwatchTrack(): void {
    this.track?.removeEventListener('ended', this.onTrackEnded);
    this.track = null;
  }

  private stopTracks(stream: MediaStream | null): void {
    for (const t of stream?.getTracks() ?? []) {
      try {
        t.stop();
      } catch {
        // already stopped
      }
    }
  }

  private disconnect(n: AudioNode | null): void {
    try {
      n?.disconnect();
    } catch {
      // already disconnected
    }
  }

  // The one teardown, for every ending.
  private release(): void {
    this.unwatchTrack();
    const stream = this.stream;
    this.stream = null;
    this.stopTracks(stream);
    this.post({ t: 'close' });
    if (this.port) this.port.onmessage = null;
    this.port = null;
    if (this.node) this.node.onprocessorerror = null;
    for (const n of [this.source, this.node, this.sink]) this.disconnect(n);
    this.source = null;
    this.node = null;
    this.sink = null;
    const enc = this.encoder;
    this.encoder = null;
    try {
      enc?.close();
    } catch {
      // already closed
    }
  }
}
