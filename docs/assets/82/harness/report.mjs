#!/usr/bin/env node
// R81 VX0: vx0.json → tables.md (docs/82 §12).
//   node docs/assets/82/harness/report.mjs [--results DIR] [--gates a,b,c]

import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const DIR = arg('results', join(HERE, '..', 'results'));
const r = JSON.parse(readFileSync(join(DIR, 'vx0.json'), 'utf8'));
const GATES = arg('gates', null)?.split(',') ?? Object.keys(r.gates);

const pct = (v) => (v === null || v === undefined ? '–' : `${(v * 100).toFixed(1)} %`);
const mean = (a) => (a.length ? a.reduce((x, y) => x + y, 0) / a.length : null);

// Pooled over rows: speech sent (mean), onsets clipped (sum/sum), interferer
// sent (mean).
function pool(rows, gate) {
  const g = rows.map((x) => x.gates[gate]);
  const on = g.reduce((a, x) => a + x.onsets, 0);
  const cl = g.reduce((a, x) => a + x.clipped, 0);
  return {
    speech: mean(g.map((x) => x.speechSent).filter((v) => v !== null)),
    clipped: on ? cl / on : null,
    interferer: mean(g.map((x) => x.interfererSent).filter((v) => v !== null)),
  };
}

const lines = [];
const table = (title, groups) => {
  lines.push(`### ${title}`, '');
  lines.push(`| Gate | ${groups.map((g) => g.name).join(' | ')} |`);
  lines.push(`|---|${groups.map(() => '---').join('|')}|`);
  for (const gate of GATES) {
    lines.push(`| \`${gate}\` | ${groups.map((g) => g.cell(gate)).join(' | ')} |`);
  }
  lines.push('');
};

const off = r.offline;
const chr = r.chromium;
const sel = (rows, pred) => rows.filter(pred);
const humCell = (rows, metric) => (gate) => {
  const p = pool(rows, gate);
  return metric === 'i' ? pct(p.interferer) : `${pct(p.speech)} · ${pct(p.clipped)}`;
};

lines.push('# R81 VX0 tables (docs/82 §12)', '', `Measured ${r.measuredAt}, ${r.node}. Speech cells: *speech sent · onsets clipped ≥ 50 ms (pooled)*. Interferer cells: share of the interferer-only frames sent.`, '');

const hums = ['hum-woman', 'hum-chant', 'hum-mains', 'hum-aircon'];
table('Offline: hums 15 dB below the speaker, interferer sent', hums.map((h) => ({ name: h, cell: humCell(sel(off, (x) => x.id === `hum/${h}/-15`), 'i') })));
table('Offline: hums by level (all four pooled), interferer sent', [10, 15, 20].map((d) => ({ name: `−${d} dB`, cell: humCell(sel(off, (x) => x.kind === 'hum' && x.id.endsWith(`/-${d}`)), 'i') })));
table('Offline: background voice, interferer sent', [10, 15, 20].map((d) => ({ name: `−${d} dB`, cell: humCell(sel(off, (x) => x.id === `bg/speech-b+c/-${d}`), 'i') })));
table('Offline: the speaker (speech sent · onsets clipped)', [
  { name: 'NS0 mix (24 runs)', cell: humCell(sel(off, (x) => x.kind === 'mix'), 's') },
  { name: 'hum scenarios', cell: humCell(sel(off, (x) => x.kind === 'hum'), 's') },
  { name: 'bg scenarios', cell: humCell(sel(off, (x) => x.kind === 'bg'), 's') },
  ...[-40, -26, -16].map((lv) => ({ name: `speaker ${lv} dBFS`, cell: humCell(sel(off, (x) => x.id === `level/${lv}`), 's') })),
]);
table('Keyboard minute at −36 dBFS, nobody talking (docs/81 §1), sent', [
  ...['alone', 'after-speech'].map((k) => ({ name: `offline, ${k}`, cell: humCell(sel(off, (x) => x.id === `kb/${k}`), 'i') })),
  ...['on', 'off'].flatMap((agc) => ['alone', 'after-speech'].map((k) => ({ name: `Chromium AGC ${agc}, ${k}`, cell: humCell(sel(chr, (x) => x.agc === agc && x.id === `kb/${k}`), 'i') }))),
]);
table('Offline: the speaker gets quieter (speech sent in the soft minute)', [6, 10, 14, 18].map((d) => ({
  name: `−${d} dB`,
  cell: (gate) => pct(off.find((x) => x.id === `drop/-${d}`)?.gates[gate].speechSent),
})));
for (const agc of ['on', 'off']) {
  const c = sel(chr, (x) => x.agc === agc);
  if (!c.length) continue;
  table(`Chromium, auto gain ${agc}: interferer sent`, [
    ...hums.map((h) => ({ name: `${h} −15`, cell: humCell(sel(c, (x) => x.id === `hum/${h}/-15`), 'i') })),
    { name: 'hum-woman −20', cell: humCell(sel(c, (x) => x.id === 'hum/hum-woman/-20'), 'i') },
    { name: 'bg −15', cell: humCell(sel(c, (x) => x.id === 'bg/speech-b+c/-15'), 'i') },
    { name: 'bg −20', cell: humCell(sel(c, (x) => x.id === 'bg/speech-b+c/-20'), 'i') },
  ]);
  table(`Chromium, auto gain ${agc}: the speaker (speech sent · onsets clipped)`, [
    { name: 'all nine', cell: humCell(c, 's') },
    { name: 'keyboard mix', cell: humCell(sel(c, (x) => x.kind === 'mix'), 's') },
    { name: 'speaker −40 dBFS', cell: humCell(sel(c, (x) => x.id === 'level/-40'), 's') },
  ]);
}

// Where the speaker's and the interferer's confident blocks sit after RNNoise.
lines.push('### Confident blocks (prob ≥ 0.95): denoised level, dBFS', '', '| Run | speaker blocks · p50 · p85 | interferer blocks · p50 · p90 |', '|---|---|---|');
const f1 = (v) => (v === null ? '–' : v.toFixed(1));
for (const x of [...off.filter((y) => y.kind !== 'mix'), ...chr]) {
  const c = x.gates.confident;
  lines.push(`| ${x.id}${x.agc ? ` (Chromium, AGC ${x.agc}${x.gainDb !== undefined ? `, ${x.gainDb >= 0 ? '+' : ''}${x.gainDb.toFixed(1)} dB` : ''})` : ''} | ${c.speechBlocks} · ${f1(c.speechP50)} · ${f1(c.speechP85)} | ${c.interfererBlocks} · ${f1(c.interfererP50)} · ${f1(c.interfererP90)} |`);
}
lines.push('');
writeFileSync(join(DIR, 'tables.md'), lines.join('\n'));
process.stderr.write(`wrote ${join(DIR, 'tables.md')}\n`);
