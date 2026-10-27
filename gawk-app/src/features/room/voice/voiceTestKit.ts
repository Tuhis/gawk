// Test-only (R80, docs/81 NS2/NS3): the capture worklet's source run in a
// fake AudioWorkletGlobalScope with the real rnnoise.wasm, and deterministic
// synthetic speech and noise to feed it. No third-party audio enters the
// repository: the "speech" is a glottal pulse train through formant
// resonators in syllable envelopes, which RNNoise 0.2 calls speech, and the
// keyboard is decaying broadband clicks, which it does not. Imported by tests
// only, so it never reaches the bundle.

/// <reference types="node" />
import { readFileSync } from 'node:fs';
import { CAPTURE_PROCESSOR_SOURCE, type CaptureWorkletCommand, type CaptureWorkletEvent } from './voiceFraming';

export const RATE = 48_000;
export const FRAME = 480;
export const QUANTUM = 128;

let cachedModule: WebAssembly.Module | null = null;

// The committed wasm, compiled once per test file.
export function rnnoiseModule(): WebAssembly.Module {
  cachedModule ??= new WebAssembly.Module(readFileSync(new URL('../../../../rnnoise/rnnoise.wasm', import.meta.url)));
  return cachedModule;
}

interface FakeProcessor {
  port: { postMessage: (m: CaptureWorkletEvent, transfer?: Transferable[]) => void; onmessage: ((e: { data: unknown }) => void) | null };
  process(inputs: Float32Array[][]): boolean;
}

export interface WorkletRun {
  processor: FakeProcessor;
  events: CaptureWorkletEvent[];
  send(cmd: CaptureWorkletCommand): void;
  // Feeds mono samples in render quanta of `quantum` (the last may be short).
  feed(samples: Float32Array, quantum?: number): void;
  blocks(): Extract<CaptureWorkletEvent, { t: 'block' }>[];
}

// Evaluates CAPTURE_PROCESSOR_SOURCE at a context rate and builds one
// processor, as `new AudioWorkletNode(...)` would.
export function runWorklet(contextRate: number, denoise: boolean): WorkletRun {
  const events: CaptureWorkletEvent[] = [];
  class AudioWorkletProcessor {
    port = {
      postMessage: (m: CaptureWorkletEvent) => void events.push(m),
      onmessage: null as ((e: { data: unknown }) => void) | null,
    };
  }
  let Cls: (new (o: unknown) => FakeProcessor) | null = null;
  new Function('AudioWorkletProcessor', 'registerProcessor', 'sampleRate', CAPTURE_PROCESSOR_SOURCE)(
    AudioWorkletProcessor,
    (_name: string, c: new (o: unknown) => FakeProcessor) => {
      Cls = c;
    },
    contextRate,
  );
  const processor = new Cls!({ processorOptions: { denoise } });
  return {
    processor,
    events,
    send: (cmd) => processor.port.onmessage?.({ data: cmd }),
    feed(samples, quantum = QUANTUM) {
      for (let off = 0; off < samples.length; off += quantum) {
        processor.process([[samples.subarray(off, Math.min(samples.length, off + quantum))]]);
      }
    },
    blocks: () => events.filter((e): e is Extract<CaptureWorkletEvent, { t: 'block' }> => e.t === 'block'),
  };
}

// ---- Signals --------------------------------------------------------------------

export function rng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s * 1103515245 + 12345) >>> 0;
    return s / 2 ** 32;
  };
}

// Vowel formants (Hz): /a/ /i/ /ɑ/ /u/ /e/.
const FORMANTS = [
  [730, 1090, 2440],
  [270, 2290, 3010],
  [570, 840, 2410],
  [300, 870, 2240],
  [530, 1840, 2480],
];

// A "reading": talk runs of syllables separated by pauses of silence, at
// 48 kHz, starting and ending with a pause.
export function syntheticSpeech(seconds: number, seed: number): Float32Array {
  const r = rng(seed);
  const n = Math.floor(seconds * RATE);
  const out = new Float32Array(n);
  const res = [0, 1, 2].map(() => ({ y1: 0, y2: 0 }));
  let i = Math.floor((0.5 + 0.5 * r()) * RATE);
  let phase = 0;
  while (i < n) {
    const talkEnd = Math.min(n - Math.floor(0.5 * RATE), i + Math.floor((0.8 + 1.4 * r()) * RATE));
    while (i < talkEnd) {
      const len = Math.floor((0.15 + 0.15 * r()) * RATE);
      const f = FORMANTS[Math.floor(r() * FORMANTS.length)];
      const f0Base = 100 + 40 * r();
      for (let k = 0; k < len && i < n; k++, i++) {
        const t = k / len;
        const env = Math.sqrt(Math.sin(Math.PI * t));
        phase += (f0Base + 15 * t) / RATE;
        let src = 0.02 * (r() - 0.5);
        if (phase >= 1) {
          phase -= 1;
          src += 1;
        }
        let y = 0;
        for (let j = 0; j < 3; j++) {
          const rad = Math.exp((-Math.PI * (80 + 40 * j)) / RATE);
          const th = (2 * Math.PI * f[j]) / RATE;
          const s = res[j];
          const v = src + 2 * rad * Math.cos(th) * s.y1 - rad * rad * s.y2;
          s.y2 = s.y1;
          s.y1 = v;
          y += v / (j + 1);
        }
        out[i] = y * env;
      }
    }
    for (const s of res) s.y1 = s.y2 = 0;
    i += Math.floor((0.4 + 0.8 * r()) * RATE);
  }
  return out;
}

// Typing: decaying broadband clicks every 80–330 ms.
export function syntheticKeyboard(seconds: number, seed: number): Float32Array {
  const r = rng(seed);
  const n = Math.floor(seconds * RATE);
  const out = new Float32Array(n);
  for (let i = Math.floor(r() * 0.1 * RATE); i < n; i += Math.floor((0.08 + 0.25 * r()) * RATE)) {
    const len = 300 + Math.floor(r() * 400);
    const a = 0.5 + r();
    for (let j = 0; j < len && i + j < n; j++) out[i + j] += a * (r() * 2 - 1) * Math.exp(-j / 60);
  }
  return out;
}

export function syntheticWhite(seconds: number, seed: number): Float32Array {
  const r = rng(seed);
  return Float32Array.from({ length: Math.floor(seconds * RATE) }, () => r() * 2 - 1);
}

// A fan: white noise through a one-pole low-pass.
export function syntheticFan(seconds: number, seed: number): Float32Array {
  const w = syntheticWhite(seconds, seed);
  let y = 0;
  return w.map((x) => (y = 0.97 * y + 0.03 * x));
}

// Mains hum with harmonics, over a little noise.
export function syntheticHum(seconds: number, seed: number): Float32Array {
  const w = syntheticWhite(seconds, seed);
  return w.map((x, i) => {
    const t = i / RATE;
    return Math.sin(2 * Math.PI * 50 * t) + 0.5 * Math.sin(2 * Math.PI * 100 * t) + 0.3 * Math.sin(2 * Math.PI * 150 * t) + 0.05 * x;
  });
}

// ---- NS0's rules (docs/assets/81/harness/offline.mjs) ---------------------------

export function frameEnergy(x: Float32Array): Float64Array {
  const n = Math.floor(x.length / FRAME);
  const e = new Float64Array(n);
  for (let f = 0; f < n; f++) {
    let s = 0;
    for (let i = 0; i < FRAME; i++) s += x[f * FRAME + i] ** 2;
    e[f] = s / FRAME;
  }
  return e;
}

const db = (x: number) => 10 * Math.log10(x + 1e-20);

// Ground truth from the clean speech: a frame is speech within 25 dB of the
// loud frames (95th percentile), bridged over gaps shorter than 50 ms.
export function speechLabels(clean: Float32Array): Uint8Array {
  const e = Array.from(frameEnergy(clean), db);
  const sorted = [...e].sort((a, b) => a - b);
  const thr = sorted[Math.floor(0.95 * sorted.length)] - 25;
  const l = Uint8Array.from(e, (v) => (v > thr ? 1 : 0));
  let last = -1e9;
  for (let f = 0; f < l.length; f++) {
    if (!l[f]) continue;
    if (f - last > 1 && f - last <= 5) for (let g = last + 1; g < f; g++) l[g] = 1;
    last = f;
  }
  return l;
}

// Scales x so its mean power over the masked frames is `dbfs`.
export function scaleTo(x: Float32Array, dbfs: number, mask?: Uint8Array): Float32Array {
  const e = frameEnergy(x);
  let s = 0;
  let n = 0;
  for (let f = 0; f < e.length; f++) {
    if (!mask || mask[f]) {
      s += e[f];
      n++;
    }
  }
  const g = Math.sqrt(10 ** (dbfs / 10) / (s / n));
  return x.map((v) => v * g);
}

export function shift(l: Uint8Array, d: number): Uint8Array {
  const out = new Uint8Array(l.length);
  for (let f = d; f < l.length; f++) out[f] = l[f - d];
  return out;
}

export interface GateMetrics {
  speechFrames: number;
  speechSent: number;
  noiseFrames: number;
  noiseSent: number;
  onsets: number;
  onsetsClipped50ms: number;
}

// gate and label in the same (output) time base. Noise frames are those
// >= 500 ms from any speech; an onset is a speech run after >= 300 ms without
// speech, clipped by the frames of its first 200 ms before the gate opened.
export function gateMetrics(gate: Uint8Array, label: Uint8Array): GateMetrics {
  const n = Math.min(gate.length, label.length);
  const far = new Uint8Array(n);
  let lastS = -1e9;
  for (let f = 0; f < n; f++) {
    if (label[f]) lastS = f;
    far[f] = f - lastS > 50 ? 1 : 0;
  }
  lastS = 1e9;
  for (let f = n - 1; f >= 0; f--) {
    if (label[f]) lastS = f;
    if (lastS - f <= 50) far[f] = 0;
  }
  const m: GateMetrics = { speechFrames: 0, speechSent: 0, noiseFrames: 0, noiseSent: 0, onsets: 0, onsetsClipped50ms: 0 };
  for (let f = 0; f < n; f++) {
    if (label[f]) {
      m.speechFrames++;
      m.speechSent += gate[f];
    }
    if (far[f]) {
      m.noiseFrames++;
      m.noiseSent += gate[f];
    }
  }
  for (let f = 30; f < n; f++) {
    if (!label[f]) continue;
    let quiet = true;
    for (let g = f - 30; g < f; g++) {
      if (label[g]) {
        quiet = false;
        break;
      }
    }
    if (!quiet) continue;
    let c = 0;
    while (c < 20 && f + c < n && !gate[f + c]) c++;
    m.onsets++;
    if (c * 10 >= 50) m.onsetsClipped50ms++;
  }
  return m;
}
