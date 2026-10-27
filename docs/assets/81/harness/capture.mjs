#!/usr/bin/env node
// R80 NS0 — Chromium's own noise suppression vs RNNoise on the same fake
// microphone (docs/81 §12). Reads run-browser.mjs's captures:
//   capture-input.f32  what the fake device played (5 s keyboard, then speech + keyboard at 10 dB)
//   capture-clean.f32  the speech alone, same timeline
//   capture-ns-on.f32  Chromium with noiseSuppression: true
//   capture-ns-off.f32 Chromium with noiseSuppression: false
// and compares, after aligning each capture to the input, the noise left in
// the keyboard-only lead and what the energy gate does with it. No waveform
// score (SI-SDR): Chromium's processing filters the capture even with noise
// suppression off (~0 dB against the clean speech), so it measures the
// filter, not the suppressor; offline.mjs scores quality instead.
//   chromium-ns    the NS-on capture
//   rnnoise        RNNoise (v0.2) on the NS-off capture
//   both           RNNoise on the NS-on capture
//   none           the NS-off capture
//
//   node docs/assets/81/harness/capture.mjs --wasm DIR --capture DIR [--out FILE]

import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { f32, FRAME, instantiate } from './rnnoise.mjs';

const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const WASM = arg('wasm');
const CAP = arg('capture');
const OUT = arg('out', new URL('../results/capture.json', import.meta.url).pathname);
const VARIANT = arg('variant', 'v02-avx');
const SR = 48000;

const ld = (n) => f32(readFileSync(join(CAP, `${n}.f32`)));
const db = (x) => 10 * Math.log10(x + 1e-20);
const energy = (x, a, b) => { let s = 0; for (let i = a; i < b; i++) s += x[i] * x[i]; return s / (b - a); };

function envelope(x) {
  const n = Math.floor(x.length / FRAME);
  const e = new Float64Array(n);
  for (let f = 0; f < n; f++) e[f] = Math.sqrt(energy(x, f * FRAME, (f + 1) * FRAME));
  return e;
}

// Where `ref` starts inside `y` (samples): coarse on 10 ms envelopes, then
// sample-exact around the coarse answer.
function align(ref, y) {
  const er = envelope(ref), ey = envelope(y);
  let best = -Infinity, lagF = 0;
  for (let L = -300; L <= 300; L++) {
    let c = 0;
    for (let f = 0; f < er.length; f++) { const g = f + L; if (g >= 0 && g < ey.length) c += er[f] * ey[g]; }
    if (c > best) { best = c; lagF = L; }
  }
  best = -Infinity;
  let lag = lagF * FRAME;
  for (let L = lagF * FRAME - FRAME; L <= lagF * FRAME + FRAME; L++) {
    let c = 0;
    for (let i = 6 * SR; i < 20 * SR; i += 2) { const j = i + L; if (j >= 0 && j < y.length) c += ref[i] * y[j]; }
    if (c > best) { best = c; lag = L; }
  }
  return lag;
}

function slice(y, lag, n) {
  const out = new Float32Array(n);
  for (let i = 0; i < n; i++) { const j = i + lag; if (j >= 0 && j < y.length) out[i] = y[j]; }
  return out;
}

function denoise(x) {
  const d = instantiate(new WebAssembly.Module(readFileSync(join(WASM, `${VARIANT}.wasm`)))).create();
  const n = Math.floor(x.length / FRAME);
  const out = new Float32Array(x.length);
  const prob = new Float32Array(n);
  const o = new Float32Array(FRAME);
  for (let f = 0; f < n; f++) { prob[f] = d.process(x.subarray(f * FRAME, (f + 1) * FRAME), o); out.set(o, f * FRAME); }
  d.destroy();
  // undo RNNoise's 20 ms
  return { out: out.subarray(960), prob: prob.subarray(2) };
}

function energyGateSpurts(x, a, b) {
  let spurts = 0, open = false, last = -1e9, sent = 0;
  for (let f = Math.floor(a / FRAME); f < Math.floor(b / FRAME); f++) {
    if (Math.sqrt(energy(x, f * FRAME, (f + 1) * FRAME)) >= 0.01) last = f;
    const g = f - last < 30;
    if (g && !open) spurts++;
    if (g) sent++;
    open = g;
  }
  return { spurts, sentFraction: sent / (Math.floor(b / FRAME) - Math.floor(a / FRAME)) };
}

const input = ld('capture-input');
const N = input.length;
const nsOnRaw = ld('capture-ns-on');
const nsOffRaw = ld('capture-ns-off');
const lagOn = align(input, nsOnRaw);
const lagOff = align(input, nsOffRaw);
const nsOn = slice(nsOnRaw, lagOn, N);
const nsOff = slice(nsOffRaw, lagOff, N);
const rn = denoise(nsOff);
const both = denoise(nsOn);
const conds = {
  none: nsOff,
  'chromium-ns': nsOn,
  rnnoise: slice(rn.out, 0, N),
  both: slice(both.out, 0, N),
};

// The lead (0.5–4.5 s) is keyboard only; speech runs from 5 s.
const LEAD = [0.5 * SR, 4.5 * SR];
const SPEECH = [5 * SR, N - SR];
const res = { variant: VARIANT, lagSamples: { nsOn: lagOn, nsOff: lagOff }, inputLeadDbfs: db(energy(input, ...LEAD)), conditions: {} };
for (const [k, y] of Object.entries(conds)) {
  res.conditions[k] = {
    leadNoiseDbfs: db(energy(y, ...LEAD)),
    leadAttenuationDb: db(energy(input, ...LEAD)) - db(energy(y, ...LEAD)),
    speechLevelChangeDb: db(energy(y, ...SPEECH)) - db(energy(input, ...SPEECH)),
    energyGateLead: energyGateSpurts(y, ...LEAD),
  };
}
res.conditions.input = { energyGateLead: energyGateSpurts(input, ...LEAD) };
// RNNoise's probability over the lead (keyboard only): frames ≥ 0.5 / 0.9.
const lp = rn.prob.subarray(LEAD[0] / FRAME, LEAD[1] / FRAME);
res.rnnoiseLeadProb = { max: Math.max(...lp), over05: lp.filter((p) => p >= 0.5).length / lp.length, over09: lp.filter((p) => p >= 0.9).length / lp.length };
writeFileSync(OUT, JSON.stringify(res, null, 2) + '\n');
console.log(JSON.stringify(res, null, 1));
