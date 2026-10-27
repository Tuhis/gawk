// The voice capture AudioWorklet (R79 docs/80 D21; R80 docs/81 D5–D7, D14).
// Every browser's microphone reaches the encoder through it: the processor
// down-mixes each render quantum to mono, resamples it to 48 kHz whatever
// rate the room's context runs at, cuts it into 10 ms blocks and, while
// RNNoise is on, denoises each block before posting it with its speech
// probability. The main thread only gates and encodes.
//
// The processor is a source string for the same reason as audioSink.ts's
// PROCESSOR_SOURCE: addModule takes a URL. Its tests run the string in a
// fake worklet scope, with the real wasm.

// One block per Opus frame: 10 ms, at 48 kHz.
export const VOICE_BLOCK_MS = 10;
export const VOICE_CAPTURE_RATE = 48_000;
export const VOICE_BLOCK_FRAMES = 480;

// RNNoise's own delay (docs/81 D8), for the settings to say.
export const RNNOISE_DELAY_MS = 20;
// The watchdog (D14): RNNoise over this share of real time, measured over
// WATCHDOG_WINDOW_MS of audio, is bypassed.
export const WATCHDOG_LIMIT = 0.4;
export const WATCHDOG_WINDOW_MS = 2000;
// The resampler's kernel half-width, in input samples at or below 48 kHz
// (D6): 64 taps, flat to 20 kHz from 44.1 kHz, 0.7 ms of delay there.
export const RESAMPLER_HALF_TAPS = 32;
// Above this many polyphase phases (an odd rate with a tiny common divisor
// with 48 kHz) the table is not built and linear interpolation is used.
export const RESAMPLER_MAX_PHASES = 2048;

// What `new AudioData(...)` takes for one block: f32-planar mono.
export function blockAudioDataInit(pcm: Float32Array, sampleRate: number, timestampUs: number): AudioDataInit {
  return {
    format: 'f32-planar',
    sampleRate,
    numberOfFrames: pcm.length,
    numberOfChannels: 1,
    timestamp: timestampUs,
    data: pcm as Float32Array<ArrayBuffer>,
  };
}

// Worklet → main thread.
export type CaptureWorkletEvent =
  // One 10 ms, 48 kHz block (the buffer transferred), its index since the
  // node started, and RNNoise's speech probability for it, or -1 when the
  // block was not denoised.
  | { t: 'block'; seq: number; pcm: Float32Array; prob: number }
  // RNNoise is running: the module instantiated and a state exists.
  | { t: 'ready' }
  // RNNoise could not start; the worklet carries on without it.
  | { t: 'error'; reason: string }
  // The watchdog tripped; the worklet carries on without RNNoise.
  | { t: 'slow'; load: number };

// Main thread → worklet.
export type CaptureWorkletCommand = { t: 'denoise'; on: boolean } | { t: 'close' };

// The compiled module travels in processorOptions, never through the port:
// Chromium ≤ 154 delivers a WebAssembly.Module posted to a worklet port as a
// messageerror (156 delivers it), while processorOptions carries it on every
// engine (docs/81 D5, §12.7). A module that lands after the node exists gets
// a new node.
export interface CaptureProcessorOptions {
  denoise: boolean;
  module: WebAssembly.Module | null;
}

export const CAPTURE_PROCESSOR_NAME = 'gawk-voice-capture';

// The processor. Silent output: the node is pulled through a muted gain so the
// engine renders it, and it must never be heard.
//
//   resampling (D6): a polyphase windowed-sinc (Blackman) from the context
//     rate to 48 kHz, its history and phase carried across quanta, so
//     nothing is lost or repeated at a quantum boundary; at 48 kHz the
//     samples pass through untouched. Not linear interpolation: that rolls
//     the highs off (-6.6 dB at 20 kHz from 44.1 kHz), and NS4 measured it
//     sending a third more keyboard through the gate at a 44.1 kHz context
//     (docs/81 §12.7).
//   RNNoise (D3, D7): one instance per node, instantiated synchronously
//     (the module imports nothing), one state while denoising; 480 samples
//     per call at int16 scale (x32768 in, /32768 out).
//   watchdog (D14): Date.now() deltas around each call, summed over 2 s of
//     audio; the only clock every worklet scope has, and with millisecond
//     ticks the sum is unbiased over many calls.
export const CAPTURE_PROCESSOR_SOURCE = `
const OUT_RATE = ${VOICE_CAPTURE_RATE};
const FRAME = ${VOICE_BLOCK_FRAMES};
const SCALE = 32768;
const WATCHDOG_FRAMES = ${WATCHDOG_WINDOW_MS / VOICE_BLOCK_MS};
const WATCHDOG_LIMIT = ${WATCHDOG_LIMIT};
const HALF_TAPS = ${RESAMPLER_HALF_TAPS};
const MAX_PHASES = ${RESAMPLER_MAX_PHASES};

function gcd(a, b) {
  while (b) [a, b] = [b, a % b];
  return a;
}

// inRate → OUT_RATE. Output k sits at input position k·M/L (L/M the reduced
// ratio): input n = floor(k·M/L) and phase (k·M) mod L pick a row of the
// kernel table, applied to inputs n-half+1 .. n+half. Inputs before the first
// are zeros.
class Resampler {
  constructor(inRate, push) {
    this.push = push;
    const g = gcd(inRate, OUT_RATE);
    this.L = OUT_RATE / g;
    this.M = inRate / g;
    this.linear = this.L > MAX_PHASES;
    if (this.linear) {
      this.ratio = inRate / OUT_RATE;
      this.pos = 0;
      this.prev = 0;
      return;
    }
    // Downsampling narrows the passband to the output's Nyquist.
    const scale = Math.min(1, OUT_RATE / inRate);
    const half = Math.ceil(HALF_TAPS / scale);
    const taps = 2 * half;
    this.half = half;
    this.taps = taps;
    this.table = new Float32Array(this.L * taps);
    for (let p = 0; p < this.L; p++) {
      const frac = p / this.L;
      let sum = 0;
      for (let j = 0; j < taps; j++) {
        const d = j - (half - 1) - frac;
        const x = scale * d;
        const sinc = x === 0 ? 1 : Math.sin(Math.PI * x) / (Math.PI * x);
        const w = Math.abs(d) >= half ? 0 : 0.42 + 0.5 * Math.cos((Math.PI * d) / half) + 0.08 * Math.cos((2 * Math.PI * d) / half);
        const h = sinc * w;
        this.table[p * taps + j] = h;
        sum += h;
      }
      // Unity gain at DC in every phase.
      for (let j = 0; j < taps; j++) this.table[p * taps + j] /= sum;
    }
    this.hist = new Float32Array(taps);
    this.work = null;
    // The next output's centre input n, relative to the first sample of
    // this.hist, and its phase.
    this.n = half - 1;
    this.phase = 0;
  }

  write(x) {
    const len = x.length;
    if (this.linear) {
      let p = this.pos;
      while (p < len - 1) {
        const i = Math.floor(p);
        const a = i < 0 ? this.prev : x[i];
        this.push(a + (p - i) * (x[i + 1] - a));
        p += this.ratio;
      }
      this.pos = p - len;
      this.prev = x[len - 1];
      return;
    }
    const taps = this.taps;
    const total = taps + len;
    if (!this.work || this.work.length !== total) this.work = new Float32Array(total);
    const w = this.work;
    w.set(this.hist, 0);
    w.set(x, taps);
    const t = this.table;
    let n = this.n;
    let phase = this.phase;
    // Output needs w[n-half+1 .. n+half].
    while (n + this.half < total) {
      const row = phase * taps;
      const base = n - this.half + 1;
      let acc = 0;
      for (let j = 0; j < taps; j++) acc += t[row + j] * w[base + j];
      this.push(acc);
      phase += this.M;
      while (phase >= this.L) {
        phase -= this.L;
        n++;
      }
    }
    this.hist.set(w.subarray(total - taps));
    this.n = n - len;
    this.phase = phase;
  }
}

class GawkVoiceCapture extends AudioWorkletProcessor {
  constructor(options) {
    super();
    this.resampler = sampleRate === OUT_RATE ? null : new Resampler(sampleRate, (v) => this.push(v));
    this.mono = null;
    this.frame = new Float32Array(FRAME);
    this.fill = 0;
    this.seq = 0;
    const o = (options && options.processorOptions) || {};
    this.denoise = !!o.denoise;
    this.module = o.module || null;
    this.wasm = null;
    this.state = 0;
    this.busyMs = 0;
    this.watched = 0;
    this.closed = false;
    this.port.onmessage = (e) => this.command(e.data);
    // Never a silent hang: anything the port cannot deliver is reported.
    this.port.onmessageerror = () =>
      this.port.postMessage({ t: 'error', reason: 'a message could not reach the audio worklet' });
    if (this.denoise) this.startDenoise();
  }

  command(m) {
    if (!m || this.closed) return;
    if (m.t === 'denoise') {
      this.denoise = !!m.on;
      if (this.denoise) this.startDenoise();
      else this.stopDenoise();
    } else if (m.t === 'close') {
      this.stopDenoise();
      this.closed = true;
    }
  }

  startDenoise() {
    if (this.state || !this.module) return;
    try {
      if (!this.wasm) {
        const e = new WebAssembly.Instance(this.module, {}).exports;
        e._initialize();
        const inP = e.malloc(FRAME * 4);
        const outP = e.malloc(FRAME * 4);
        // One view for good: rnnoise/build.sh links with
        // ALLOW_MEMORY_GROWTH=0, so malloc never detaches the buffer.
        // Allowing growth would mean re-taking this view after allocations.
        this.wasm = { e, inP, outP, heap: new Float32Array(e.memory.buffer), inO: inP >> 2, outO: outP >> 2 };
      }
      this.state = this.wasm.e.rnnoise_create(0);
      if (!this.state) throw new Error('rnnoise_create returned null');
    } catch (err) {
      this.denoise = false;
      this.state = 0;
      this.port.postMessage({ t: 'error', reason: String((err && err.message) || err) });
      return;
    }
    this.busyMs = 0;
    this.watched = 0;
    this.port.postMessage({ t: 'ready' });
  }

  stopDenoise() {
    if (this.state) this.wasm.e.rnnoise_destroy(this.state);
    this.state = 0;
  }

  process(inputs) {
    if (this.closed) return false;
    const input = inputs[0];
    if (!input || input.length === 0 || input[0].length === 0) return true;
    const n = input[0].length;
    if (!this.mono || this.mono.length !== n) this.mono = new Float32Array(n);
    const mono = this.mono;
    mono.set(input[0]);
    if (input.length > 1) {
      for (let c = 1; c < input.length; c++) {
        const ch = input[c];
        for (let i = 0; i < n; i++) mono[i] += ch[i];
      }
      for (let i = 0; i < n; i++) mono[i] /= input.length;
    }
    if (this.resampler) this.resampler.write(mono);
    else for (let i = 0; i < n; i++) this.push(mono[i]);
    return true;
  }

  push(x) {
    this.frame[this.fill++] = x;
    if (this.fill === FRAME) this.emit();
  }

  emit() {
    const pcm = this.frame;
    this.frame = new Float32Array(FRAME);
    this.fill = 0;
    let prob = -1;
    if (this.state) {
      const w = this.wasm;
      const t0 = Date.now();
      for (let i = 0; i < FRAME; i++) w.heap[w.inO + i] = pcm[i] * SCALE;
      prob = w.e.rnnoise_process_frame(this.state, w.outP, w.inP);
      for (let i = 0; i < FRAME; i++) pcm[i] = w.heap[w.outO + i] / SCALE;
      this.busyMs += Date.now() - t0;
      if (++this.watched >= WATCHDOG_FRAMES) {
        const load = this.busyMs / (this.watched * ${VOICE_BLOCK_MS});
        this.busyMs = 0;
        this.watched = 0;
        if (load > WATCHDOG_LIMIT) {
          this.denoise = false;
          this.stopDenoise();
          this.port.postMessage({ t: 'slow', load });
        }
      }
    }
    this.port.postMessage({ t: 'block', seq: this.seq++, pcm, prob }, [pcm.buffer]);
  }
}
registerProcessor('${CAPTURE_PROCESSOR_NAME}', GawkVoiceCapture);
`;
