#!/usr/bin/env node
// R80 NS0 — offline measurements (docs/81 §12): build parity against native,
// algorithmic delay, CPU per frame in V8, denoising quality and the talk gate
// (energy vs RNNoise speech probability) on real speech mixed with real noise.
//
//   node docs/assets/81/harness/offline.mjs --wasm DIR --corpus DIR [--native BIN] [--out FILE]
//
// --wasm: build-wasm.sh's output; --corpus: fetch-corpus.sh's; --native: a
// native-ref.c binary built against the same main sources (optional).

import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs';
import { tmpdir, cpus } from 'node:os';
import { join } from 'node:path';
import { f32, FRAME, instantiate } from './rnnoise.mjs';

const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const WASM = arg('wasm');
const CORPUS = arg('corpus');
const NATIVE = arg('native', null);
const OUT = arg('out', new URL('../results/offline.json', import.meta.url).pathname);

const FRAME_MS = 10;
const SPEECH = ['speech-a', 'speech-b', 'speech-c'];
const NOISES = ['noise-keyboard', 'noise-restaurant', 'noise-street', 'white'];
const SNRS = [20, 10, 5, 0];
// Active speech at -26 dBFS: a microphone after the browser's auto gain.
const SPEECH_DBFS = -26;
const VARIANTS = ['v02-avx', 'mumble-avx', 'main-avx', 'main-little-avx'];

const load = (n) => f32(readFileSync(join(CORPUS, `${n}.f32`)));
const modules = new Map();
const mod = (v) => {
  if (!modules.has(v)) modules.set(v, new WebAssembly.Module(readFileSync(join(WASM, `${v}.wasm`))));
  return modules.get(v);
};
const frames = (x) => Math.floor(x.length / FRAME);
const db = (x) => 10 * Math.log10(x + 1e-20);

function frameEnergy(x) {
  const n = frames(x);
  const e = new Float64Array(n);
  for (let f = 0; f < n; f++) {
    let s = 0;
    for (let i = 0; i < FRAME; i++) s += x[f * FRAME + i] ** 2;
    e[f] = s / FRAME;
  }
  return e;
}

// Ground truth from the clean reading: a frame is speech when it is within
// 25 dB of the reading's loud frames (95th percentile), then bridged over
// gaps shorter than 50 ms so stops inside a word stay speech.
function labels(clean) {
  const e = Array.from(frameEnergy(clean), db);
  const s = [...e].sort((a, b) => a - b);
  const thr = s[Math.floor(0.95 * s.length)] - 25;
  const l = e.map((v) => (v > thr ? 1 : 0));
  let last = -1e9;
  for (let f = 0; f < l.length; f++) {
    if (l[f]) {
      if (f - last > 1 && f - last <= 5) for (let g = last + 1; g < f; g++) l[g] = 1;
      last = f;
    }
  }
  return l;
}

function scaleTo(x, dbfs, mask) {
  const e = frameEnergy(x);
  let s = 0, n = 0;
  for (let f = 0; f < e.length; f++) if (!mask || mask[f]) { s += e[f]; n++; }
  const g = Math.sqrt(10 ** (dbfs / 10) / (s / n));
  return x.map((v) => v * g);
}

function white(n) {
  let seed = 12345;
  const out = new Float32Array(n);
  for (let i = 0; i < n; i++) {
    seed = (seed * 1103515245 + 12345) >>> 0;
    out[i] = seed / 2 ** 31 - 1;
  }
  return out;
}

function run(variant, x) {
  const d = instantiate(mod(variant)).create();
  const n = frames(x);
  const out = new Float32Array(n * FRAME);
  const prob = new Float32Array(n);
  const o = new Float32Array(FRAME);
  const t0 = performance.now();
  for (let f = 0; f < n; f++) {
    prob[f] = d.process(x.subarray(f * FRAME, (f + 1) * FRAME), o);
    out.set(o, f * FRAME);
  }
  const msPerFrame = (performance.now() - t0) / n;
  d.destroy();
  return { out, prob, msPerFrame };
}

// ---- Gates ------------------------------------------------------------------

// talkGate.ts today: RMS ≥ 0.01 opens, held 300 ms after the last loud block.
function energyGate(x, thresholdRms = 0.01, holdMs = 300) {
  const e = frameEnergy(x);
  const g = new Uint8Array(e.length);
  let last = -1e9;
  for (let f = 0; f < e.length; f++) {
    if (Math.sqrt(e[f]) >= thresholdRms) last = f;
    g[f] = f - last < holdMs / FRAME_MS ? 1 : 0;
  }
  return g;
}

// Mumble's shape (AudioInput.cpp): open above `on`, stay open above `off`
// while open, then hold `holdMs` after the level falls below. With a
// `floorRms`, a frame only counts when the denoised output is also at least
// that loud: the probability decides, the floor vetoes what RNNoise has
// already silenced.
function probGate(prob, on, off, holdMs, outRms = null, floorRms = 0) {
  const g = new Uint8Array(prob.length);
  let open = false;
  let held = 1e9;
  for (let f = 0; f < prob.length; f++) {
    const p = outRms && outRms[f] < floorRms ? 0 : prob[f];
    const speech = p >= on || (open && p >= off);
    open = speech;
    held = speech ? 0 : held + 1;
    g[f] = speech || held < holdMs / FRAME_MS ? 1 : 0;
  }
  return g;
}

// ---- Metrics ----------------------------------------------------------------

function shift(l, d) {
  const out = new Uint8Array(l.length);
  for (let f = d; f < l.length; f++) out[f] = l[f - d];
  return out;
}

// gate and label in the same (output) time base.
function gateMetrics(gate, label) {
  const n = Math.min(gate.length, label.length);
  const far = new Uint8Array(n); // ≥ 500 ms from any speech
  let lastS = -1e9;
  for (let f = 0; f < n; f++) { if (label[f]) lastS = f; far[f] = f - lastS > 50 ? 1 : 0; }
  lastS = 1e9;
  for (let f = n - 1; f >= 0; f--) { if (label[f]) lastS = f; if (lastS - f <= 50) far[f] = 0; }
  let sp = 0, spSent = 0, farN = 0, farSent = 0;
  for (let f = 0; f < n; f++) {
    if (label[f]) { sp++; spSent += gate[f]; }
    if (far[f]) { farN++; farSent += gate[f]; }
  }
  // Onsets: a speech run after ≥ 300 ms of no speech. Clipped = frames of the
  // run's first 200 ms before the gate opened.
  const clip = [];
  for (let f = 30; f < n; f++) {
    if (!label[f]) continue;
    let quiet = true;
    for (let g = f - 30; g < f; g++) if (label[g]) { quiet = false; break; }
    if (!quiet) continue;
    let c = 0;
    while (c < 20 && f + c < n && !gate[f + c]) c++;
    clip.push(c * FRAME_MS);
  }
  // Spurts that contain no speech at all: noise the gate let through.
  let falseSpurts = 0;
  for (let f = 0; f < n; ) {
    if (!gate[f]) { f++; continue; }
    let any = false;
    while (f < n && gate[f]) { if (label[f]) any = true; f++; }
    if (!any) falseSpurts++;
  }
  clip.sort((a, b) => a - b);
  return {
    speechSent: sp ? spSent / sp : null,
    noiseSent: farN ? farSent / farN : null,
    falseSpurtsPerMin: falseSpurts / ((n * FRAME_MS) / 60000),
    onsets: clip.length,
    onsetClipP50Ms: clip[Math.floor(clip.length / 2)] ?? null,
    onsetClipP90Ms: clip[Math.floor(clip.length * 0.9)] ?? null,
    onsetsClipped50ms: clip.filter((c) => c >= 50).length / (clip.length || 1),
  };
}

// Scale-invariant SDR of `est` against `ref` (both already aligned).
function siSdr(est, ref) {
  let dot = 0, rr = 0;
  for (let i = 0; i < ref.length; i++) { dot += est[i] * ref[i]; rr += ref[i] * ref[i]; }
  const a = dot / rr;
  let s = 0, e = 0;
  for (let i = 0; i < ref.length; i++) { const t = a * ref[i]; s += t * t; e += (est[i] - t) ** 2; }
  return db(s / e);
}

function bestLag(x, y, maxLag) {
  let best = -Infinity, lag = 0;
  const end = Math.min(x.length, y.length) - maxLag;
  for (let L = 0; L <= maxLag; L++) {
    let c = 0;
    for (let i = 48000; i < end; i += 3) c += x[i] * y[i + L];
    if (c > best) { best = c; lag = L; }
  }
  return lag;
}

// ---- Run --------------------------------------------------------------------

const result = {
  measuredAt: new Date().toISOString(),
  host: { cpu: cpus()[0]?.model, node: process.version, v8: process.versions.v8 },
  emcc: readFileSync(join(WASM, 'EMCC_VERSION'), 'utf8').trim(),
  speechDbfs: SPEECH_DBFS,
};

// 1. CPU per frame, every variant, 60 s of speech-a at -26 dBFS (3 reps).
const clean = {};
const lab = {};
for (const s of SPEECH) {
  const raw = load(s);
  lab[s] = labels(raw);
  clean[s] = scaleTo(raw, SPEECH_DBFS, lab[s]);
}
result.cpu = {};
for (const v of ['v02-avx', 'v02-avx2', 'mumble-avx', 'main-scalar', 'main-avx', 'main-avx2', 'main-little-scalar', 'main-little-avx', 'main-little-avx2']) {
  const reps = [0, 1, 2].map(() => run(v, clean['speech-a']).msPerFrame);
  result.cpu[v] = { msPerFrameMin: Math.min(...reps), wasmBytes: readFileSync(join(WASM, `${v}.wasm`)).length };
}

// 2. Algorithmic delay (output vs input, samples) and the probability's
// alignment (which frame lag best matches the labels).
result.delay = {};
for (const v of VARIANTS) {
  const r = run(v, clean['speech-a']);
  const lagSamples = bestLag(clean['speech-a'], r.out, 2400);
  let bestF = 0, bestAgree = -1;
  for (let d = 0; d <= 5; d++) {
    const l = shift(lab['speech-a'], d);
    let a = 0;
    for (let f = 0; f < l.length; f++) a += (r.prob[f] >= 0.5) === (l[f] === 1) ? 1 : 0;
    if (a > bestAgree) { bestAgree = a; bestF = d; }
  }
  result.delay[v] = { lagSamples, lagMs: lagSamples / 48, probLagFrames: bestF };
}

// 3. Native parity: the same input through native-ref and main-avx2.
if (NATIVE) {
  const dir = mkdtempSync(join(tmpdir(), 'ns0-'));
  const x = clean['speech-a'];
  const out = execFileSync(NATIVE, [join(dir, 'vad.f32')], { input: Buffer.from(x.buffer), maxBuffer: 1 << 28 });
  const nOut = new Float32Array(out.buffer.slice(out.byteOffset, out.byteOffset + out.byteLength));
  const nVad = f32(readFileSync(join(dir, 'vad.f32')));
  result.parity = {};
  for (const v of ['main-scalar', 'main-avx', 'main-avx2']) {
    const r = run(v, x);
    let err = 0, sig = 0, maxP = 0;
    for (let i = 0; i < nOut.length; i++) { err += (r.out[i] - nOut[i]) ** 2; sig += nOut[i] ** 2; }
    for (let f = 0; f < nVad.length; f++) maxP = Math.max(maxP, Math.abs(r.prob[f] - nVad[f]));
    result.parity[v] = { snrVsNativeDb: db(sig / err), maxProbDiff: maxP };
  }
}

// 4. Quality and gates over the mix matrix.
const noiseCache = {};
const noiseOf = (n, len) => (noiseCache[n] ??= n === 'white' ? white(len) : load(n));
const ON = [0.5, 0.6, 0.7, 0.8, 0.9, 0.95];
const OFF_DELTA = [0, 0.2, 0.4];
const HOLD = [200, 300];
// Level floors on the denoised output, dBFS.
const FLOORS = [-60, -55, -50, -45];

// Every gate, for one input `x` and its RNNoise run `r`, against labels in
// the input's and the output's time base.
function gates(x, r, lIn, lOut) {
  const g = {
    'energy-raw': gateMetrics(energyGate(x), lIn),
    'energy-rnn': gateMetrics(energyGate(r.out), lOut),
  };
  const outRms = Array.from(frameEnergy(r.out), Math.sqrt);
  for (const on of ON) for (const dOff of OFF_DELTA) for (const h of HOLD) {
    const off = Math.max(0, on - dOff);
    // gated as production gates it: each output block with its own call's
    // probability (0.2 and Mumble's pin run one frame ahead, §12.2)
    g[`prob-${on}-${off.toFixed(2)}-${h}`] = gateMetrics(probGate(r.prob, on, off, h), lOut);
  }
  for (const on of ON) for (const fl of FLOORS) {
    const off = Math.max(0, on - 0.2);
    g[`probfloor-${on}-${off.toFixed(2)}-300-${fl}`] = gateMetrics(probGate(r.prob, on, off, 300, outRms, 10 ** (fl / 20)), lOut);
  }
  return g;
}
result.mix = [];
for (const v of VARIANTS) {
  const lagF = result.delay[v].lagSamples / FRAME;
  for (const nz of NOISES) {
    for (const snr of SNRS) {
      for (const s of SPEECH) {
        const c = clean[s];
        const nraw = noiseOf(nz, c.length).subarray(0, c.length);
        const noise = scaleTo(nraw, SPEECH_DBFS - snr);
        const x = c.map((v2, i) => v2 + noise[i]);
        const r = run(v, x);
        const L = result.delay[v].lagSamples;
        const refC = c.subarray(0, c.length - L);
        const est = r.out.subarray(L, L + refC.length);
        const row = {
          variant: v, noise: nz, snr, speech: s,
          siSdrIn: siSdr(x.subarray(0, refC.length), refC),
          siSdrOut: siSdr(est, refC),
          gates: gates(x, r, lab[s], shift(lab[s], lagF)),
        };
        result.mix.push(row);
      }
      process.stderr.write(`${v} ${nz} ${snr} dB\n`);
    }
  }
}

// 5. Noise only: what the gate sends while nobody talks, the noise at its
// level at 10 dB SNR (-36 dBFS) and at 0 dB (-26 dBFS, a loud keyboard
// next to the mic); and how much RNNoise attenuates it.
result.noiseOnly = [];
for (const v of VARIANTS) {
  for (const nz of NOISES) {
    for (const level of [SPEECH_DBFS - 10, SPEECH_DBFS]) {
      const n = scaleTo(noiseOf(nz, 2880000).subarray(0, 2880000), level);
      const r = run(v, n);
      const zero = new Uint8Array(frames(n));
      result.noiseOnly.push({
        variant: v, noise: nz, levelDbfs: level,
        attenuationDb: db(frameEnergy(n).reduce((a, b) => a + b)) - db(frameEnergy(r.out).reduce((a, b) => a + b)),
        gates: gates(n, r, zero, zero),
      });
    }
  }
}

writeFileSync(OUT, JSON.stringify(result));
process.stderr.write(`wrote ${OUT}\n`);
