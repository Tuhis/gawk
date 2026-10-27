#!/usr/bin/env node
// R81 VX0 — Chromium's processing on the spike's scenarios (docs/82 §12).
// Plays each scenario as Chromium's fake microphone and records what
// getUserMedia delivers with the RNNoise mode's constraints (docs/81 D11:
// echo cancellation on, suppression off), auto gain on and off. vx0.mjs
// aligns and scores the recordings. Not committed: they hold the corpus.
//
//   node docs/assets/82/harness/capture.mjs --corpus DIR --out DIR [--parallel 4]

import { mkdirSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loader, scenarios } from './scenarios.mjs';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const REPO = resolve(HERE, '../../../..');
const pw = createRequire(join(REPO, 'e2e', 'package.json'))('playwright-core');
const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const CORPUS = arg('corpus');
const OUT = resolve(arg('out'));
const PARALLEL = Number(arg('parallel', 4));
// The scenarios worth Chromium's minutes: every hum and background voice at
// -15 dB, the quieter background, and one keyboard mix.
const WANT = new Set([
  'hum/hum-woman/-15', 'hum/hum-chant/-15', 'hum/hum-mains/-15', 'hum/hum-aircon/-15',
  'hum/hum-woman/-20', 'bg/speech-b+c/-15', 'bg/speech-b+c/-20', 'mix/speech-a/noise-keyboard/10',
  'level/-40', 'kb/alone', 'kb/after-speech',
]);
// --only id,id: capture just those (an added scenario, without redoing the rest).
const ONLY = arg('only', null)?.split(',') ?? null;

function wav16(path, x) {
  const data = Buffer.alloc(x.length * 2);
  for (let i = 0; i < x.length; i++) data.writeInt16LE(Math.max(-32768, Math.min(32767, Math.round(x[i] * 32767))), i * 2);
  const h = Buffer.alloc(44);
  h.write('RIFF', 0); h.writeUInt32LE(36 + data.length, 4); h.write('WAVE', 8);
  h.write('fmt ', 12); h.writeUInt32LE(16, 16); h.writeUInt16LE(1, 20); h.writeUInt16LE(1, 22);
  h.writeUInt32LE(48000, 24); h.writeUInt32LE(96000, 28); h.writeUInt16LE(2, 32); h.writeUInt16LE(16, 34);
  h.write('data', 36); h.writeUInt32LE(data.length, 40);
  writeFileSync(path, Buffer.concat([h, data]));
}

// The page: records the microphone through MediaStreamTrackProcessor, as
// docs/81's ns-browser.js record() does, and hands back f32 as base64.
const PAGE = `<!doctype html><meta charset="utf-8"><script>
window.record = async ({ agc, seconds }) => {
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: false, autoGainControl: agc, channelCount: 1 } });
  const track = stream.getAudioTracks()[0];
  const settings = track.getSettings();
  const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
  const chunks = []; let n = 0, rate = 0;
  const end = performance.now() + seconds * 1000;
  while (performance.now() < end) {
    const { value } = await reader.read();
    if (!value) break;
    rate = value.sampleRate;
    const p = new Float32Array(value.numberOfFrames);
    value.copyTo(p, { planeIndex: 0, format: 'f32-planar' });
    chunks.push(p); n += p.length; value.close();
  }
  track.stop();
  const all = new Float32Array(n); let o = 0;
  for (const c of chunks) { all.set(c, o); o += c.length; }
  const u8 = new Uint8Array(all.buffer); let s = '';
  for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000));
  return { rate, settings: { autoGainControl: settings.autoGainControl, noiseSuppression: settings.noiseSuppression, echoCancellation: settings.echoCancellation }, f32: btoa(s) };
};
</script>`;

const srv = createServer((_, res) => res.writeHead(200, { 'content-type': 'text/html' }).end(PAGE));
await new Promise((r) => srv.listen(0, '127.0.0.1', r));
const url = `http://127.0.0.1:${srv.address().port}/`;

mkdirSync(OUT, { recursive: true });
const jobs = [];
for (const s of scenarios(loader(CORPUS))) {
  if (!WANT.has(s.id) || (ONLY && !ONLY.includes(s.id))) continue;
  const base = s.id.replaceAll('/', '_');
  const wav = join(OUT, `${base}.wav`);
  wav16(wav, s.x);
  for (const agc of [true, false]) jobs.push({ id: s.id, base, wav, agc, seconds: s.x.length / 48000 + 3 });
}

const meta = [];
async function run(job) {
  const b = await pw.chromium.launch({
    channel: 'chromium', headless: true,
    args: ['--no-sandbox', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
      '--autoplay-policy=no-user-gesture-required', `--use-file-for-fake-audio-capture=${job.wav}%noloop`],
  });
  try {
    const page = await b.newPage();
    await page.goto(url);
    const r = await page.evaluate((o) => window.record(o), { agc: job.agc, seconds: job.seconds });
    if (r.rate !== 48000) throw new Error(`${job.id}: capture at ${r.rate} Hz`);
    writeFileSync(join(OUT, `${job.base}.agc-${job.agc ? 'on' : 'off'}.f32`), Buffer.from(r.f32, 'base64'));
    meta.push({ id: job.id, agc: job.agc, settings: r.settings, browser: b.version() });
    process.stderr.write(`captured ${job.id} agc ${job.agc} ${JSON.stringify(r.settings)}\n`);
  } finally {
    await b.close();
  }
}
const queue = [...jobs];
await Promise.all(Array.from({ length: PARALLEL }, async () => { while (queue.length) await run(queue.shift()); }));
writeFileSync(join(OUT, ONLY ? 'captures-only.json' : 'captures.json'), JSON.stringify(meta, null, 1));
srv.close();
