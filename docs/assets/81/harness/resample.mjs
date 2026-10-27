#!/usr/bin/env node
// R80 NS0 — does a linear 44.1 → 48 kHz resampler in front of RNNoise cost
// anything? (docs/81 §12). macOS routinely runs the AudioContext at 44.1 kHz
// whatever was asked (docs/gotchas.md), so the capture worklet sees 44.1 kHz
// PCM and RNNoise only works at 48 kHz.
//
// speech + noise at 10 dB SNR, 48 kHz, through RNNoise three ways:
//   direct      the 48 kHz mix (the reference)
//   linear      ffmpeg to 44.1 kHz (what the context delivers), then the
//               worklet's linear interpolation back to 48 kHz
//   unresampled the 44.1 kHz samples handed to RNNoise as if 48 kHz (the bug
//               this guards against)
// Reported: noise attenuation over a noise-only minute, SI-SDR against the
// clean speech, and agreement of the speech probability with direct's.
//
//   node docs/assets/81/harness/resample.mjs --wasm DIR --corpus DIR [--out FILE]

import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { f32, FRAME, instantiate } from './rnnoise.mjs';

const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const WASM = arg('wasm');
const CORPUS = arg('corpus');
const OUT = arg('out', new URL('../results/resample.json', import.meta.url).pathname);
const VARIANT = arg('variant', 'v02-avx');

const ld = (n) => f32(readFileSync(join(CORPUS, `${n}.f32`)));
const db = (x) => 10 * Math.log10(x + 1e-20);
const ms = (x) => x.reduce((a, v) => a + v * v, 0) / x.length;
const mod = new WebAssembly.Module(readFileSync(join(WASM, `${VARIANT}.wasm`)));

function rn(x) {
  const d = instantiate(mod).create();
  const n = Math.floor(x.length / FRAME);
  const out = new Float32Array(n * FRAME), prob = new Float32Array(n), o = new Float32Array(FRAME);
  for (let f = 0; f < n; f++) { prob[f] = d.process(x.subarray(f * FRAME, (f + 1) * FRAME), o); out.set(o, f * FRAME); }
  d.destroy();
  return { out, prob };
}

function to441(x) {
  const b = execFileSync('ffmpeg', ['-nostdin', '-loglevel', 'error', '-f', 'f32le', '-ar', '48000', '-ac', '1', '-i', 'pipe:0',
    '-ar', '44100', '-f', 'f32le', 'pipe:1'], { input: Buffer.from(x.buffer), maxBuffer: 1 << 28 });
  return new Float32Array(b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength));
}

// The worklet's resampler: linear interpolation at a fixed in/out ratio,
// state carried across calls (here one call).
function linearUp(x, inRate, outRate) {
  const step = inRate / outRate;
  const n = Math.floor((x.length - 1) / step);
  const y = new Float32Array(n);
  for (let i = 0; i < n; i++) {
    const p = i * step, k = Math.floor(p), t = p - k;
    y[i] = x[k] + (x[k + 1] - x[k]) * t;
  }
  return y;
}

function siSdr(est, ref) {
  let dot = 0, rr = 0;
  for (let i = 0; i < ref.length; i++) { dot += est[i] * ref[i]; rr += ref[i] * ref[i]; }
  const a = dot / rr;
  let s = 0, e = 0;
  for (let i = 0; i < ref.length; i++) { const t = a * ref[i]; s += t * t; e += (est[i] - t) ** 2; }
  return db(s / e);
}

// Best lag of y against ref (the resampler adds a fraction of a sample).
function bestLag(ref, y, around, span) {
  let best = -Infinity, lag = around;
  for (let L = around - span; L <= around + span; L++) {
    let c = 0;
    for (let i = 48000; i < ref.length - 4000; i += 3) c += ref[i] * y[i + L];
    if (c > best) { best = c; lag = L; }
  }
  return lag;
}

const res = { variant: VARIANT, rows: [] };
for (const noise of ['noise-keyboard', 'noise-restaurant', 'noise-street']) {
  const sp = ld('speech-a');
  const nz = ld(noise).subarray(0, sp.length);
  // active speech ≈ -26 dBFS, noise 10 dB below
  const gs = Math.sqrt(10 ** (-2.6) / (ms(sp.filter((v) => Math.abs(v) > 0.01)) || 1));
  const gn = Math.sqrt(10 ** (-3.6) / ms(nz));
  const clean = sp.map((v) => v * gs);
  const noiseOnly = nz.map((v) => v * gn);
  const mix = clean.map((v, i) => v + noiseOnly[i]);
  const paths = {
    direct: (x) => x,
    linear: (x) => linearUp(to441(x), 44100, 48000),
    unresampled: (x) => to441(x),
  };
  const ref = rn(mix);
  for (const [name, f] of Object.entries(paths)) {
    const r = name === 'direct' ? ref : rn(f(mix));
    const nr = rn(f(noiseOnly));
    const L = bestLag(clean, r.out, 960, 48);
    const n = Math.min(clean.length - 2000, r.out.length - L);
    const nf = Math.min(r.prob.length, ref.prob.length);
    let agree = 0;
    for (let k = 0; k < nf; k++) agree += (r.prob[k] >= 0.6) === (ref.prob[k] >= 0.6) ? 1 : 0;
    res.rows.push({
      noise, path: name,
      lagSamples: L,
      noiseAttenuationDb: db(ms(noiseOnly)) - db(ms(nr.out)),
      siSdrDb: siSdr(r.out.subarray(L, L + n), clean.subarray(0, n)),
      probAgreementWithDirect: agree / nf,
      meanProb: r.prob.reduce((a, v) => a + v, 0) / r.prob.length,
    });
  }
}
writeFileSync(OUT, JSON.stringify(res, null, 2) + '\n');
for (const r of res.rows) console.log(r.noise, r.path, r.lagSamples, r.noiseAttenuationDb.toFixed(1), r.siSdrDb.toFixed(2), r.probAgreementWithDirect.toFixed(3), r.meanProb.toFixed(3));
