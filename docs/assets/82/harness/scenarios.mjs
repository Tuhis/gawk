// R81 VX0: the signals the spike plays (docs/82 §12). Every scenario is the
// speaker's own reading (`clean`, at -26 dBFS active level, as NS0) plus an
// interferer, 48 kHz mono; labels come from `clean` with NS0's rule.
//
//   hum:   0–40 s the reading, 40–70 s the interferer alone, 70–90 s the
//          reading again (does the gate keep the hum out, and does the
//          speaker come back unclipped after 30 s of it?)
//   bg:    a second reading under the speaker the whole 90 s (a TV, someone
//          across the room); 0–40 s and 60–80 s the speaker, 40–60 s and
//          80–90 s the background alone
//   mix:   NS0's matrix (three readings × keyboard, restaurant, street,
//          white at 10 and 5 dB SNR, 60 s): regressions in speech sent and
//          onsets clipped
//   drop:  the speaker gets quieter: two minutes of readings at -26 dBFS,
//          then speech-a 6, 10, 14 or 18 dB softer for a minute (leaning
//          back, starting to whisper): how long until the floor follows?
//   kb:    docs/81 §1's keyboard minute (-36 dBFS) with nobody talking, alone
//          and after 30 s of the speaker (once a level is learned)
//   level: speech-a over keyboard at 10 dB SNR with the speaker at -40,
//          -26 and -16 dBFS: does a relative floor follow a quiet or loud
//          speaker where a fixed one cannot?

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

export const RATE = 48000;
export const FRAME = 480;
export const SPEECH_DBFS = -26;

export function f32(buf) {
  return new Float32Array(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
}

export function frameEnergy(x) {
  const n = Math.floor(x.length / FRAME);
  const e = new Float64Array(n);
  for (let f = 0; f < n; f++) {
    let s = 0;
    for (let i = 0; i < FRAME; i++) s += x[f * FRAME + i] ** 2;
    e[f] = s / FRAME;
  }
  return e;
}

const db = (x) => 10 * Math.log10(x + 1e-20);

// NS0's labels (offline.mjs): within 25 dB of the reading's 95th-percentile
// frame, gaps under 50 ms bridged.
export function labels(clean) {
  const e = Array.from(frameEnergy(clean), db);
  const s = e.filter((v) => v > -150).sort((a, b) => a - b);
  const thr = s[Math.floor(0.95 * s.length)] - 25;
  const l = e.map((v) => (v > thr ? 1 : 0));
  let last = -1e9;
  for (let f = 0; f < l.length; f++) {
    if (l[f]) {
      if (f - last > 1 && f - last <= 5) for (let g = last + 1; g < f; g++) l[g] = 1;
      last = f;
    }
  }
  return Uint8Array.from(l);
}

// Scale to `dbfs`: over the labelled frames for speech (active level), over
// everything for noise.
export function scaleTo(x, dbfs, mask = null) {
  const e = frameEnergy(x);
  let s = 0;
  let n = 0;
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

export function loader(corpus) {
  const cache = new Map();
  return (name) => {
    if (!cache.has(name)) cache.set(name, name === 'white' ? white(60 * RATE) : f32(readFileSync(join(corpus, `${name}.f32`))));
    return cache.get(name);
  };
}

const sec = (s) => s * RATE;

// The first `len` samples, zero-padded: a corpus file can end a few
// samples short of 60 s.
function fit(x, len) {
  const out = new Float32Array(len);
  out.set(x.subarray(0, len));
  return out;
}

function speechAt(load, name, dbfs) {
  const raw = load(name);
  return scaleTo(raw, dbfs, labels(raw));
}

// Place `src[from, from+len)` at `at` in `dst`.
function put(dst, src, at, from, len) {
  for (let i = 0; i < len && from + i < src.length && at + i < dst.length; i++) dst[at + i] += src[from + i];
}

export const HUMS = ['hum-woman', 'hum-chant', 'hum-mains', 'hum-aircon'];
export const REL_DB = [10, 15, 20];

// Every scenario: { id, kind, x (input), clean, region: [fromFrame, toFrame)
// pairs where only the interferer plays }.
export function* scenarios(load, { kinds = ['hum', 'bg', 'mix', 'level', 'drop', 'kb'] } = {}) {
  if (kinds.includes('hum')) {
    const sp = speechAt(load, 'speech-a', SPEECH_DBFS);
    for (const h of HUMS) for (const r of REL_DB) {
      const clean = new Float32Array(sec(90));
      put(clean, sp, 0, 0, sec(40));
      put(clean, sp, sec(70), sec(40), sec(20));
      const x = Float32Array.from(clean);
      put(x, scaleTo(load(h), SPEECH_DBFS - r), sec(40), 0, sec(30));
      yield { id: `hum/${h}/-${r}`, kind: 'hum', x, clean, region: [[4050, 7000]] };
    }
  }
  if (kinds.includes('bg')) {
    const sp = speechAt(load, 'speech-a', SPEECH_DBFS);
    for (const r of REL_DB) {
      const clean = new Float32Array(sec(90));
      put(clean, sp, 0, 0, sec(40));
      put(clean, sp, sec(60), sec(40), sec(20));
      const x = Float32Array.from(clean);
      put(x, speechAt(load, 'speech-b', SPEECH_DBFS - r), 0, 0, sec(60));
      put(x, speechAt(load, 'speech-c', SPEECH_DBFS - r), sec(60), 0, sec(30));
      yield { id: `bg/speech-b+c/-${r}`, kind: 'bg', x, clean, region: [[4050, 6000], [8050, 9000]] };
    }
  }
  if (kinds.includes('mix')) {
    for (const s of ['speech-a', 'speech-b', 'speech-c']) {
      const clean = fit(speechAt(load, s, SPEECH_DBFS), sec(60));
      for (const n of ['noise-keyboard', 'noise-restaurant', 'noise-street', 'white']) for (const snr of [10, 5]) {
        const nz = scaleTo(fit(load(n), sec(60)), SPEECH_DBFS - snr);
        yield { id: `mix/${s}/${n}/${snr}`, kind: 'mix', x: clean.map((v, i) => v + nz[i]), clean, region: [] };
      }
    }
  }
  if (kinds.includes('drop')) {
    for (const d of [6, 10, 14, 18]) {
      const clean = new Float32Array(sec(180));
      put(clean, speechAt(load, 'speech-b', SPEECH_DBFS), 0, 0, sec(60));
      put(clean, speechAt(load, 'speech-c', SPEECH_DBFS), sec(60), 0, sec(60));
      put(clean, speechAt(load, 'speech-a', SPEECH_DBFS - d), sec(120), 0, sec(60));
      // Scored on the soft minute only (region = its frames, as speech).
      yield { id: `drop/-${d}`, kind: 'drop', x: Float32Array.from(clean), clean, region: [], scoreFrom: 12000 };
    }
  }
  if (kinds.includes('kb')) {
    const kb = scaleTo(fit(load('noise-keyboard'), sec(60)), -36);
    const alone = new Float32Array(sec(60));
    put(alone, kb, 0, 0, sec(60));
    yield { id: 'kb/alone', kind: 'kb', x: alone, clean: new Float32Array(sec(60)), region: [[0, 6000]] };
    const clean = new Float32Array(sec(90));
    put(clean, speechAt(load, 'speech-a', SPEECH_DBFS), 0, 0, sec(30));
    const x = Float32Array.from(clean);
    put(x, kb, sec(30), 0, sec(60));
    yield { id: 'kb/after-speech', kind: 'kb', x, clean, region: [[3050, 9000]] };
  }
  if (kinds.includes('level')) {
    for (const lv of [-40, -26, -16]) {
      const clean = fit(speechAt(load, 'speech-a', lv), sec(60));
      const nz = scaleTo(fit(load('noise-keyboard'), sec(60)), lv - 10);
      yield { id: `level/${lv}`, kind: 'level', x: clean.map((v, i) => v + nz[i]), clean, region: [] };
    }
  }
}
