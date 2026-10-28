#!/usr/bin/env node
// R81 VX0 — the learned-level floor against docs/81 D9's fixed one
// (docs/82 §12). Runs every scenario (scenarios.mjs) through the shipping
// RNNoise wasm and scores each gate (gates.mjs). With --captures, also runs
// the scenarios Chromium recorded through its own processing (capture.mjs),
// auto gain on and off, as the microphone actually reaches the worklet.
//
//   node docs/assets/82/harness/vx0.mjs --corpus DIR [--captures DIR] [--out FILE]

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { fixedGate, trackedGate } from './gates.mjs';
import { f32, FRAME, frameEnergy, labels, loader, scenarios } from './scenarios.mjs';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const REPO = resolve(HERE, '../../../..');
const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const CORPUS = arg('corpus');
const CAPTURES = arg('captures', null);
const OUT = arg('out', join(HERE, '..', 'results', 'vx0.json'));

// The wasm gawk-app ships (RNNoise 0.2, docs/81 D1).
const MODULE = new WebAssembly.Module(readFileSync(join(REPO, 'gawk-app/rnnoise/rnnoise.wasm')));
// RNNoise's output runs 960 samples behind its input (docs/81 D8).
const LAG_FRAMES = 2;

function denoise(x) {
  const e = new WebAssembly.Instance(MODULE, {}).exports;
  e._initialize();
  const st = e.rnnoise_create(0);
  const inP = e.malloc(FRAME * 4);
  const outP = e.malloc(FRAME * 4);
  const heap = new Float32Array(e.memory.buffer);
  const n = Math.floor(x.length / FRAME);
  const prob = new Float32Array(n);
  const outRms = new Float32Array(n);
  for (let f = 0; f < n; f++) {
    for (let i = 0; i < FRAME; i++) heap[(inP >> 2) + i] = x[f * FRAME + i] * 32768;
    prob[f] = e.rnnoise_process_frame(st, outP, inP);
    let s = 0;
    for (let i = 0; i < FRAME; i++) s += (heap[(outP >> 2) + i] / 32768) ** 2;
    outRms[f] = Math.sqrt(s / FRAME);
  }
  e.rnnoise_destroy(st);
  return { prob, outRms };
}

function shift(l, d) {
  const out = new Uint8Array(l.length);
  for (let f = d; f < l.length; f++) out[f] = l[f - d];
  return out;
}

// NS0's metrics (offline.mjs gateMetrics), plus the interferer: the share of
// the interferer-only region the gate sent, and its spurts per minute.
function metrics(gate, label, region) {
  const n = Math.min(gate.length, label.length);
  let sp = 0;
  let spSent = 0;
  for (let f = 0; f < n; f++) if (label[f]) { sp++; spSent += gate[f]; }
  let onsets = 0;
  let clipped = 0;
  for (let f = 30; f < n; f++) {
    if (!label[f]) continue;
    let quiet = true;
    for (let g = f - 30; g < f; g++) if (label[g]) { quiet = false; break; }
    if (!quiet) continue;
    let c = 0;
    while (c < 20 && f + c < n && !gate[f + c]) c++;
    onsets++;
    if (c >= 5) clipped++;
  }
  let rN = 0;
  let rSent = 0;
  let spurts = 0;
  for (const [a, b] of region) {
    for (let f = a; f < Math.min(b, n); f++) {
      rN++;
      rSent += gate[f];
      if (gate[f] && (f === a || !gate[f - 1])) spurts++;
    }
  }
  return {
    speechSent: sp ? spSent / sp : null,
    onsets,
    clipped,
    interfererSent: rN ? rSent / rN : null,
    interfererSpurtsPerMin: rN ? spurts / ((rN * 10) / 60000) : null,
  };
}

const GATES = { 'fixed--50': { kind: 'fixed', floorDb: -50 } };
for (const fl of [-45, -40, -35]) GATES[`fixed-${fl}`] = { kind: 'fixed', floorDb: fl };
for (const margin of [10, 12, 15, 18, 20, 25]) for (const cap of [-35, -30]) for (const hyst of [0, 6]) {
  GATES[`tracked-m${margin}-c${cap}-h${hyst}`] = { kind: 'tracked', margin, cap, hyst };
}
// How fast the learned level may follow a speaker who gets quieter: the
// histogram's half-life and the quantile (a lower quantile sits lower in the
// speaker's own distribution, so its margin is smaller for the same floor).
for (const halfLifeS of [60, 30, 15]) for (const [q, margin] of [[0.85, 12], [0.7, 10], [0.5, 7]]) {
  GATES[`tracked-m${margin}-q${q}-hl${halfLifeS}`] = { kind: 'tracked', margin, cap: -30, hyst: 0, tracker: { q, halfLifeS } };
}
// The slider's ends (docs/82 D4's curve), at the default margin's neighbours.
for (const [on, margin] of [[0.6, 25], [0.6, 30], [0.99, 10], [0.99, 12]]) {
  GATES[`tracked-on${on}-m${margin}-c-30-h6`] = { kind: 'tracked', on, off: on - 0.2, margin, cap: -30, hyst: 6 };
}

function score(x, clean, region, scoreFrom = 0) {
  const { prob, outRms } = denoise(x);
  const lab = shift(labels(clean), LAG_FRAMES);
  // Score the speaker only from scoreFrom (the drop scenarios' soft minute).
  for (let f = 0; f < Math.min(scoreFrom, lab.length); f++) lab[f] = 0;
  const out = {};
  for (const [name, g] of Object.entries(GATES)) {
    const opts = { on: g.on ?? 0.9, off: g.off ?? 0.7, holdMs: 300 };
    if (g.kind === 'fixed') {
      out[name] = metrics(fixedGate(prob, outRms, { ...opts, floorDb: g.floorDb }), lab, region);
    } else {
      const r = trackedGate(prob, outRms, { ...opts, margin: g.margin, cap: g.cap, hyst: g.hyst, tracker: g.tracker });
      out[name] = { ...metrics(r.gate, lab, region), learnedDb: r.finalLevel };
    }
  }
  // The confident blocks' levels inside and outside the interferer region:
  // how far apart the speaker and the hum are after RNNoise.
  const conf = { speech: [], interferer: [] };
  const inRegion = new Uint8Array(prob.length);
  for (const [a, b] of region) for (let f = a; f < Math.min(b, prob.length); f++) inRegion[f] = 1;
  for (let f = 0; f < prob.length; f++) {
    if (prob[f] < 0.95 || outRms[f] <= 1e-5) continue;
    const d = 20 * Math.log10(outRms[f]);
    if (inRegion[f]) conf.interferer.push(d);
    else if (lab[f]) conf.speech.push(d);
  }
  const pct = (a, q) => (a.length ? [...a].sort((u, v) => u - v)[Math.floor(q * (a.length - 1))] : null);
  out.confident = {
    speechBlocks: conf.speech.length, speechP50: pct(conf.speech, 0.5), speechP85: pct(conf.speech, 0.85),
    interfererBlocks: conf.interferer.length, interfererP50: pct(conf.interferer, 0.5), interfererP90: pct(conf.interferer, 0.9),
  };
  return out;
}

// Align a capture to its input: the lag (samples) maximising correlation.
function align(cap, x) {
  let best = -Infinity;
  let lag = 0;
  for (let L = 0; L <= 9600; L += 8) {
    let c = 0;
    for (let i = 5 * 48000; i < 35 * 48000; i += 7) c += x[i] * (cap[i + L] ?? 0);
    if (c > best) { best = c; lag = L; }
  }
  const out = new Float32Array(x.length);
  out.set(cap.subarray(lag, lag + x.length));
  return { aligned: out, lag };
}

const load = loader(CORPUS);
const result = { measuredAt: new Date().toISOString(), node: process.version, gates: GATES, offline: [], chromium: [] };
for (const s of scenarios(load)) {
  result.offline.push({ id: s.id, kind: s.kind, gates: score(s.x, s.clean, s.region, s.scoreFrom) });
  process.stderr.write(`offline ${s.id}\n`);
  if (!CAPTURES) continue;
  for (const agc of ['on', 'off']) {
    const p = join(CAPTURES, `${s.id.replaceAll('/', '_')}.agc-${agc}.f32`);
    if (!existsSync(p)) continue;
    const { aligned, lag } = align(f32(readFileSync(p)), s.x);
    const inDb = 10 * Math.log10(frameEnergy(s.x).reduce((a, b) => a + b) / frameEnergy(s.x).length);
    const capDb = 10 * Math.log10(frameEnergy(aligned).reduce((a, b) => a + b) / frameEnergy(aligned).length);
    result.chromium.push({ id: s.id, kind: s.kind, agc, lag, gainDb: capDb - inDb, gates: score(aligned, s.clean, s.region) });
    process.stderr.write(`chromium ${s.id} agc ${agc}\n`);
  }
}
writeFileSync(OUT, JSON.stringify(result));
process.stderr.write(`wrote ${OUT}\n`);
