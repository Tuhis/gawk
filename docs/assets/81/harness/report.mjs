#!/usr/bin/env node
// R80 NS0 — tables for docs/assets/81/RESULTS.md from results/*.json.
//   node docs/assets/81/harness/report.mjs [--results DIR] [--gate-variant v02-avx2]
// Writes results/tables.md and splices it between the BEGIN/END markers in
// RESULTS.md.

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const DIR = resolve(arg('results', join(HERE, '..', 'results')));
const GV = arg('gate-variant', 'v02-avx');
const DEFAULT_GATE = arg('default-gate', 'probfloor-0.9-0.70-300--50');
const j = (f) => (existsSync(join(DIR, f)) ? JSON.parse(readFileSync(join(DIR, f), 'utf8')) : null);

const off = j('offline.json');
const cap = j('capture.json');
const rs = j('resample.json');
const br = Object.fromEntries(['chromium', 'firefox', 'webkit'].map((b) => [b, j(`browser-${b}.json`)]).filter(([, v]) => v));

const f = (x, d = 1) => (x === null || x === undefined || Number.isNaN(x) ? '–' : Number(x).toFixed(d));
const pct = (x) => (x === null || x === undefined ? '–' : `${(100 * x).toFixed(1)} %`);
const mean = (a) => a.reduce((s, v) => s + v, 0) / a.length;
// Onsets clipped ≥ 50 ms over onsets, pooled across runs (not a mean of
// per-run rates: the readings have 9, 11 and 12 onsets).
const pooledClip = (rows) => {
  const n = rows.reduce((s, x) => s + x.onsets, 0);
  return n ? rows.reduce((s, x) => s + x.onsetsClipped50ms * x.onsets, 0) / n : null;
};
const row = (cells) => `| ${cells.join(' | ')} |`;
const out = [];
const P = (s = '') => out.push(s);

// ---- builds and CPU -----------------------------------------------------------
P('### Builds and CPU per 10 ms frame');
P();
P(`Emscripten: \`${off?.emcc ?? '?'}\`. Node ${off?.host.node} (V8 ${off?.host.v8}) on ${off?.host.cpu}. ` +
  'Browser numbers are main-thread, best of 3 × 3000 frames; the worklet column is 1000 frames timed with `Date.now()` inside the AudioWorklet (1 ms clock, so ±0.001 ms/frame).');
P();
const variants = Object.keys(off?.cpu ?? {});
const bNames = Object.keys(br);
P(row(['Variant', 'wasm bytes', 'Node', ...bNames.map((b) => `${b} ${br[b].version}`), ...bNames.map((b) => `${b} worklet`)]));
P(row(Array(3 + 2 * bNames.length).fill('---')));
for (const v of variants) {
  P(row([`\`${v}\``, off.cpu[v].wasmBytes.toLocaleString('en'), f(off.cpu[v].msPerFrameMin, 3),
    ...bNames.map((b) => f(br[b].cpuMain?.[v]?.msPerFrame, 3)),
    ...bNames.map((b) => {
      const w = br[b].worklet?.filter((x) => x.variant === v && x.kind === 'ready') ?? [];
      return w.length ? f(Math.min(...w.map((x) => x.benchMsPerFrame)), 3) : '–';
    })]));
}
P();

// ---- delay and parity ---------------------------------------------------------
P('### Algorithmic delay and parity with native');
P();
P(row(['Variant', 'Output lag (samples)', 'Output lag (ms)', 'Probability lag (frames, best agreement with labels)']));
P(row(['---', '---', '---', '---']));
for (const [v, d] of Object.entries(off?.delay ?? {})) P(row([`\`${v}\``, d.lagSamples, f(d.lagMs, 2), d.probLagFrames]));
P();
if (off?.parity) {
  P('Against a native x86-64 build of the same main sources (`native-ref.c`, gcc -O2, SSE2 path):');
  P();
  P(row(['wasm variant', 'Output SNR vs native (dB)', 'Max |Δ probability|']));
  P(row(['---', '---', '---']));
  for (const [v, p] of Object.entries(off.parity)) P(row([`\`${v}\``, f(p.snrVsNativeDb, 1), f(p.maxProbDiff, 4)]));
  P();
}

// ---- quality ------------------------------------------------------------------
if (off) {
  P('### Denoising quality (offline, mean of three readings)');
  P();
  P('SI-SDR of the output against the clean reading (higher is better), input in brackets. Speech at −26 dBFS active level.');
  P();
  const vs = [...new Set(off.mix.map((m) => m.variant))];
  const noises = [...new Set(off.mix.map((m) => m.noise))];
  const snrs = [...new Set(off.mix.map((m) => m.snr))];
  P(row(['Noise', 'SNR', ...vs.map((v) => `\`${v}\``)]));
  P(row(Array(2 + vs.length).fill('---')));
  for (const nz of noises) for (const snr of snrs) {
    const cells = vs.map((v) => {
      const rows = off.mix.filter((m) => m.variant === v && m.noise === nz && m.snr === snr);
      return `${f(mean(rows.map((r) => r.siSdrOut)))} (${f(mean(rows.map((r) => r.siSdrIn)))})`;
    });
    P(row([nz.replace('noise-', ''), `${snr} dB`, ...cells]));
  }
  P();
  P('Noise alone, 60 s: attenuation in dB at −36 dBFS (its level at 10 dB SNR).');
  P();
  P(row(['Noise', ...vs.map((v) => `\`${v}\``)]));
  P(row(Array(1 + vs.length).fill('---')));
  for (const nz of noises) P(row([nz.replace('noise-', ''), ...vs.map((v) => f(off.noiseOnly.find((r) => r.variant === v && r.noise === nz && (r.levelDbfs ?? -36) === -36)?.attenuationDb))]));
  P();

  // ---- gates ------------------------------------------------------------------
  P(`### The talk gate (\`${GV}\`)`);
  P();
  P('In the output\'s time base. *Speech sent*: labelled speech frames the gate passed. ' +
    '*Clipped*: speech onsets (after ≥ 300 ms quiet) that lost ≥ 50 ms before the gate opened, pooled (clipped ÷ onsets over every run in the cell); *clip p90*: the 90th-percentile loss, ms. ' +
    '*Noise only*: 60 s of the noise with nobody talking, the share of frames sent and the spurts per minute. ' +
    '`energy-raw` is today\'s gate (RMS ≥ 0.01, 300 ms hold) on the unprocessed signal — pessimistic, because in production it runs after the ' +
    'browser\'s own suppression (§ Chromium below shows how little that removes from a keyboard); `energy-rnn` is the same gate after RNNoise; ' +
    '`prob-ON-OFF-HOLD` opens at probability ≥ ON, stays open while ≥ OFF, then holds HOLD ms; `probfloor-ON-OFF-HOLD-FLOOR` also needs the ' +
    'denoised block to be louder than FLOOR dBFS.');
  P();
  const keys = Object.keys(off.mix[0].gates);
  const at = (v, k, pred) => off.mix.filter((m) => m.variant === v && pred(m)).map((m) => m.gates[k]).filter(Boolean);
  const only = (v, k, nz, lvl) => off.noiseOnly.find((r) => r.variant === v && r.noise === nz && (r.levelDbfs ?? -36) === lvl)?.gates[k];
  const levels = [...new Set(off.noiseOnly.map((r) => r.levelDbfs ?? -36))];
  const featured = ['energy-raw', 'energy-rnn',
    ...keys.filter((k) => /^prob-(0\.6|0\.7|0\.8|0\.9)-0\.\d0-300$/.test(k) && Math.abs(Number(k.split('-')[1]) - Number(k.split('-')[2]) - 0.2) < 1e-9),
    ...keys.filter((k) => /^probfloor-(0\.6|0\.7|0\.8|0\.9)-.*-300-(-50|-55)$/.test(k))];
  P('**Per noise, speech mixed at 10 and 5 dB SNR** (mean of three readings × two SNRs): speech sent · clipped.');
  P();
  P(row(['Gate', ...noises.map((n) => n.replace('noise-', ''))]));
  P(row(Array(1 + noises.length).fill('---')));
  for (const k of featured) {
    P(row([`\`${k}\``, ...noises.map((nz) => {
      const r = at(GV, k, (m) => m.noise === nz && (m.snr === 10 || m.snr === 5));
      return `${pct(mean(r.map((x) => x.speechSent)))} · ${pct(pooledClip(r))}`;
    })]));
  }
  P();
  for (const lvl of levels) {
    P(`**Noise only at ${lvl} dBFS**, nobody talking: sent · spurts/min.`);
    P();
    P(row(['Gate', ...noises.map((n) => n.replace('noise-', ''))]));
    P(row(Array(1 + noises.length).fill('---')));
    for (const k of featured) {
      P(row([`\`${k}\``, ...noises.map((nz) => {
        const g = only(GV, k, nz, lvl);
        return g ? `${pct(g.noiseSent)} · ${f(g.falseSpurtsPerMin)}` : '–';
      })]));
    }
    P();
  }
  P('**Every gate, condensed**: speech mixes at all SNRs averaged; noise only averaged over the keyboard, street and white noise (babble is speech, see the per-noise tables) at each level.');
  P();
  P(row(['Gate', 'Speech sent', 'Clipped', 'Clip p90 ms', ...levels.map((l) => `Noise sent @ ${l}`), ...levels.map((l) => `Spurts/min @ ${l}`)]));
  P(row(Array(4 + 2 * levels.length).fill('---')));
  const nonBabble = noises.filter((n) => n !== 'noise-restaurant');
  for (const k of keys.filter((x) => x.startsWith('prob')).concat(['energy-raw', 'energy-rnn'])) {
    const r = at(GV, k, () => true);
    P(row([`\`${k}\``, pct(mean(r.map((x) => x.speechSent))), pct(pooledClip(r)), f(mean(r.map((x) => x.onsetClipP90Ms)), 0),
      ...levels.map((l) => pct(mean(nonBabble.map((nz) => only(GV, k, nz, l)?.noiseSent ?? 0)))),
      ...levels.map((l) => f(mean(nonBabble.map((nz) => only(GV, k, nz, l)?.falseSpurtsPerMin ?? 0))))]));
  }
  P();
  if (DEFAULT_GATE) {
    P(`**The default gate (\`${DEFAULT_GATE}\`) on each model**: speech at 10/5 dB (sent · clipped), noise only at ${levels.at(-1)} dBFS (sent per noise).`);
    P();
    P(row(['Variant', 'Speech sent · clipped', ...noises.map((n) => n.replace('noise-', ''))]));
    P(row(Array(2 + noises.length).fill('---')));
    for (const v of vs) {
      const r = at(v, DEFAULT_GATE, (m) => m.snr === 10 || m.snr === 5);
      P(row([`\`${v}\``, `${pct(mean(r.map((x) => x.speechSent)))} · ${pct(pooledClip(r))}`,
        ...noises.map((nz) => pct(only(v, DEFAULT_GATE, nz, levels.at(-1))?.noiseSent))]));
    }
    P();
  }
}

// ---- resample -----------------------------------------------------------------
if (rs) {
  P(`### 44.1 kHz context: linear resampling in front of RNNoise (\`${rs.variant}\`, 10 dB SNR)`);
  P();
  P(row(['Noise', 'Path', 'Noise attenuation dB', 'SI-SDR dB', 'Probability ≥ 0.6 agrees with direct']));
  P(row(Array(5).fill('---')));
  for (const r of rs.rows) P(row([r.noise.replace('noise-', ''), r.path, f(r.noiseAttenuationDb), f(r.siSdrDb, 2), pct(r.probAgreementWithDirect)]));
  P();
}

// ---- browsers -----------------------------------------------------------------
if (bNames.length) {
  P('### Browser probes');
  P();
  P(row(['', ...bNames.map((b) => `${b} ${br[b].version}`)]));
  P(row(Array(1 + bNames.length).fill('---')));
  const cell = (fn) => bNames.map((b) => { try { return fn(br[b]); } catch { return '–'; } });
  P(row(['wasm SIMD', ...cell((r) => (r.caps.wasmSimd ? 'yes' : 'no'))]));
  P(row(['MSTP on Window', ...cell((r) => (r.caps.mstpOnWindow ? 'yes' : 'no'))]));
  P(row(['Default AudioContext rate (null sink)', ...cell((r) => r.caps.defaultContextRate)]));
  P(row(['`noiseSuppression` a supported constraint', ...cell((r) => (r.caps.supportedConstraints.includes('noiseSuppression') ? 'yes' : '**no**'))]));
  P(row(['`voiceIsolation` a supported constraint', ...cell((r) => (r.caps.supportedConstraints.includes('voiceIsolation') ? 'yes' : 'no'))]));
  P(row(['`WebAssembly.Module` → worklet via `processorOptions`', ...cell((r) => (r.worklet.filter((w) => w.via === 'processorOptions').every((w) => w.kind === 'ready') ? 'yes' : 'no'))]));
  P(row(['`WebAssembly.Module` → worklet via `port.postMessage`', ...cell((r) => (r.worklet.filter((w) => w.via === 'port').every((w) => w.kind === 'ready') ? 'yes' : 'no'))]));
  P(row(['Worklet render ratio over 6 s, min–max of the runs (`currentFrame` ÷ the worklet\'s clock ÷ rate; 1 = real time)', ...cell((r) => {
    const v = r.worklet.filter((w) => w.realtime).map((w) => w.realtime.renderRatio);
    return `${f(Math.min(...v), 4)}–${f(Math.max(...v), 4)} (${v.length} runs)`;
  })]));
  const msrc = (rate) => (r) => {
    const c = r.rates.contexts.find((x) => x.requested === rate);
    return `${c?.mediaStreamSource}, peak ${f(c?.peak, 3)}`;
  };
  P(row(['48 kHz context + `createMediaStreamSource` of the mic, peak over 500 ms', ...cell(msrc(48000))]));
  P(row(['44.1 kHz context + `createMediaStreamSource`, peak over 500 ms', ...cell(msrc(44100))]));
  P(row(['Mic opened with NS off → `getSettings().noiseSuppression`', ...cell((r) => String(r.constraints.openedWithNsOff.noiseSuppression))]));
  P(row(['… after `applyConstraints({noiseSuppression: true})`', ...cell((r) => String(r.constraints.afterApplyNsOn.noiseSuppression))]));
  P(row(['wasm fetch + compile + instantiate ms (`main-avx`, localhost)', ...cell((r) => `${f(r.cpuMain['main-avx'].fetchMs)} + ${f(r.cpuMain['main-avx'].compileMs)} + ${f(r.cpuMain['main-avx'].instantiateMs)}`)]));
  P();
  const ac = br.chromium?.applyConstraints;
  if (ac) {
    P('#### Chromium: does `applyConstraints({noiseSuppression})` change the audio?');
    P();
    P('The fake microphone plays keyboard alone at -36 dBFS for 20 s (auto gain on). Level of 2–9 s and of 12–19 s; ' +
      'the switch runs at 10 s, and the no-switch runs show the file\'s own drift.');
    P();
    P(row(['Run', 'Opened with NS', 'Switched to', '`getSettings()` after', '2–9 s dBFS', '12–19 s dBFS']));
    P(row(Array(6).fill('---')));
    for (const [k, v] of Object.entries(ac)) {
      P(row([k, String(v.settings?.noiseSuppression), v.applied ? String(!v.settings?.noiseSuppression) : '–',
        v.applied ? String(v.applied.settingsAfter ?? v.applied.error) : '–', f(v.firstDbfs, 2), f(v.secondDbfs, 2)]));
    }
    P();
  }
}

// ---- Chromium capture -----------------------------------------------------------
const capAgc = br.chromium?.record
  ? `\`getSettings()\`: auto gain ${br.chromium.record.nsOn.settings.autoGainControl} / ${br.chromium.record.nsOff.settings.autoGainControl} in the NS on / off recordings`
  : 'settings not recorded';
if (cap) {
  P(`### Chromium's own noise suppression vs RNNoise (\`${cap.variant}\`), same fake microphone`);
  P();
  P(`The fake device plays 5 s of keyboard (${f(cap.inputLeadDbfs)} dBFS), then speech + keyboard at 10 dB SNR. ` +
    `Echo cancellation and auto gain on, as \`voiceCapture.ts\` opens it today (${capAgc}), except the two auto-gain-off rows. Lead = 0.5–4.5 s, keyboard only, before any speech.`);
  P();
  P(row(['Condition', 'Lead noise dBFS', 'Attenuation vs input dB', 'Speech level change dB', 'Energy gate in the lead: spurts / sent']));
  P(row(Array(5).fill('---')));
  const label = { none: 'Chromium, NS off', 'chromium-ns': 'Chromium, NS on', rnnoise: 'NS off + RNNoise', both: 'NS on + RNNoise',
    'none-agc-off': 'Chromium, NS off, auto gain off', 'rnnoise-agc-off': 'NS off, auto gain off + RNNoise' };
  for (const k of Object.keys(label)) {
    const c = cap.conditions[k];
    if (!c) continue;
    P(row([label[k], f(c.leadNoiseDbfs), f(c.leadAttenuationDb), f(c.speechLevelChangeDb), `${c.energyGateLead.spurts} / ${pct(c.energyGateLead.sentFraction)}`]));
  }
  P();
  for (const [k, lp] of Object.entries(cap.leadProb ?? { rnnoise: cap.rnnoiseLeadProb })) {
    P(`RNNoise's speech probability over the keyboard-only lead, ${label[k]}: max ${f(lp.max, 3)}, ≥ 0.5 on ${pct(lp.over05)} of frames, ≥ 0.9 on ${pct(lp.over09)}.`);
    P();
  }
  const tt = br.chromium?.twoTracks;
  if (tt) {
    P('**Two tracks at once** (keyboard alone, auto gain on): the first opened with NS off, the second, while the first is open, with NS on.');
    P();
    P(row(['Track', 'Asked for NS', '`getSettings()` NS', '2–9 s dBFS', '12–19 s dBFS']));
    P(row(Array(5).fill('---')));
    for (const [t, ask] of [['first', false], ['second', true]]) {
      const v = tt[t];
      P(row([t, String(ask), String(v.settings.noiseSuppression), f(v.firstDbfs, 2), f(v.secondDbfs, 2)]));
    }
  }
  P();
}

const md = out.join('\n');
writeFileSync(join(DIR, 'tables.md'), md + '\n');
const R = join(DIR, '..', 'RESULTS.md');
if (existsSync(R)) {
  const s = readFileSync(R, 'utf8');
  const a = s.indexOf('<!-- BEGIN generated');
  const b = s.indexOf('<!-- END generated -->');
  if (a >= 0 && b > a) {
    const head = s.slice(0, s.indexOf('\n', a) + 1);
    writeFileSync(R, `${head}\n${md}\n${s.slice(b)}`);
  }
}
console.log(md);
