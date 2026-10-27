#!/usr/bin/env node
// R80 NS0 — Playwright driver for ns-browser.html (docs/81 §12).
//
//   node docs/assets/81/harness/run-browser.mjs --wasm DIR [--corpus DIR]
//        [--browsers chromium,firefox,webkit] [--out DIR]
//
// --corpus (fetch-corpus.sh's output) enables Chromium's record probe: the
// fake microphone plays 5 s of keyboard noise, then speech-a with that noise
// at 10 dB SNR, and the page records it with the browser's noise
// suppression on and off. The captures go to OUT/capture-*.f32, which the
// repository does not keep (they contain the CC BY-SA reading).

import { createReadStream, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, extname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { f32 } from './rnnoise.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '../../../..');
const require = createRequire(join(REPO, 'e2e', 'package.json'));
const pw = require('playwright-core');

const argv = process.argv.slice(2);
const arg = (n, d) => (argv.includes(`--${n}`) ? argv[argv.indexOf(`--${n}`) + 1] : d);
const WASM = resolve(arg('wasm'));
const CORPUS = arg('corpus', null);
const BROWSERS = arg('browsers', 'chromium,firefox,webkit').split(',');
const OUT = resolve(arg('out', join(HERE, '..', 'results')));
const CAPTURE_OUT = resolve(arg('capture-out', OUT));
const VARIANTS = ['v02-avx', 'v02-avx2', 'main-scalar', 'main-avx', 'main-avx2', 'main-little-scalar', 'main-little-avx', 'main-little-avx2'];
const REC_SECONDS = 30;

function serve() {
  const types = { '.html': 'text/html', '.js': 'text/javascript', '.wasm': 'application/wasm' };
  const srv = createServer((req, res) => {
    const p = new URL(req.url, 'http://x').pathname.replace(/^\/+/, '') || 'ns-browser.html';
    const f = p.startsWith('wasm/') ? join(WASM, p.slice(5)) : join(HERE, p);
    if (!(f.startsWith(HERE) || f.startsWith(WASM)) || !existsSync(f)) { res.writeHead(404).end(); return; }
    res.writeHead(200, { 'content-type': types[extname(f)] ?? 'application/octet-stream', 'cache-control': 'no-store' });
    createReadStream(f).pipe(res);
  });
  return new Promise((r) => srv.listen(0, '127.0.0.1', () => r(srv)));
}

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

// 5 s of keyboard, then 25 s of speech-a + keyboard at 10 dB SNR; speech at
// -26 dBFS active level (the labels rule as offline.mjs, simplified).
function mixWav(dir) {
  const ld = (n) => f32(readFileSync(join(CORPUS, `${n}.f32`)));
  const sp = ld('speech-a').subarray(0, 25 * 48000);
  const nz = ld('noise-keyboard').subarray(0, 30 * 48000);
  const rms = (x) => Math.sqrt(x.reduce((a, v) => a + v * v, 0) / x.length);
  // active level ≈ RMS of frames above -45 dB re the loudest
  const fr = [];
  for (let f = 0; f + 480 <= sp.length; f += 480) fr.push(rms(sp.subarray(f, f + 480)));
  const top = [...fr].sort((a, b) => a - b)[Math.floor(fr.length * 0.95)];
  const act = Math.sqrt(fr.filter((v) => v > top / 18).reduce((a, v) => a + v * v, 0) / fr.filter((v) => v > top / 18).length);
  const gs = 10 ** (-26 / 20) / act;
  const gn = 10 ** (-36 / 20) / rms(nz);
  const x = new Float32Array(30 * 48000);
  const clean = new Float32Array(30 * 48000);
  for (let i = 0; i < x.length; i++) {
    const s = i >= 5 * 48000 ? sp[i - 5 * 48000] * gs : 0;
    clean[i] = s;
    x[i] = s + nz[i] * gn;
  }
  const path = join(dir, 'mix.wav');
  wav16(path, x);
  writeFileSync(join(CAPTURE_OUT, 'capture-clean.f32'), Buffer.from(clean.buffer));
  writeFileSync(join(CAPTURE_OUT, 'capture-input.f32'), Buffer.from(x.buffer));
  return path;
}

async function launch(name, wav) {
  if (name === 'chromium') {
    return pw.chromium.launch({
      channel: 'chromium', headless: true,
      args: ['--no-sandbox', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
        '--autoplay-policy=no-user-gesture-required', ...(wav ? [`--use-file-for-fake-audio-capture=${wav}%noloop`] : [])],
    });
  }
  if (name === 'firefox') {
    return pw.firefox.launch({
      headless: true,
      firefoxUserPrefs: {
        'media.navigator.streams.fake': true, 'media.navigator.permission.disabled': true,
        'media.autoplay.default': 0, 'media.autoplay.block-webaudio': false, 'media.autoplay.blocking_policy': 0,
        'privacy.reduceTimerPrecision': false,
      },
    });
  }
  return pw.webkit.launch({ headless: true });
}

async function withPage(browser, name, fn) {
  const context = await browser.newContext();
  if (name !== 'firefox') await context.grantPermissions(['microphone']).catch(() => {});
  const page = await context.newPage();
  const consoleLines = [];
  page.on('console', (m) => consoleLines.push(`${m.type()}: ${m.text()}`));
  page.on('pageerror', (e) => consoleLines.push(`pageerror: ${e}`));
  try {
    await page.goto(base);
    await page.waitForFunction(() => window.ns0Ready === true, null, { timeout: 15000 });
    return await fn(page);
  } catch (e) {
    return { fatal: String(e), console: consoleLines.slice(0, 20) };
  } finally {
    await context.close();
  }
}

let base;
async function main() {
  mkdirSync(OUT, { recursive: true });
  mkdirSync(CAPTURE_OUT, { recursive: true });
  const srv = await serve();
  base = `http://127.0.0.1:${srv.address().port}/ns-browser.html`;
  try {
    for (const name of BROWSERS) {
      const browser = await launch(name, null);
      const res = { browser: name, version: browser.version(), playwright: require('playwright-core/package.json').version, date: new Date().toISOString() };
      console.log(`[ns0] ${name} ${res.version}`);
      res.caps = await withPage(browser, name, (p) => p.evaluate(() => window.ns0.caps()));
      res.cpuMain = await withPage(browser, name, (p) => p.evaluate((v) => window.ns0.cpuMain({ variants: v }), VARIANTS));
      res.worklet = [];
      for (const variant of ['v02-avx', 'main-avx', 'main-little-avx']) {
        for (const via of ['processorOptions', 'port']) {
          res.worklet.push(await withPage(browser, name, (p) => p.evaluate((o) => window.ns0.worklet(o), { variant, via })));
        }
      }
      for (const contextRate of [44100, 48000]) {
        res.worklet.push(await withPage(browser, name, (p) => p.evaluate((o) => window.ns0.worklet(o), { variant: 'main-avx', via: 'port', contextRate })));
      }
      res.rates = await withPage(browser, name, (p) => p.evaluate(() => window.ns0.rates()));
      res.constraints = await withPage(browser, name, (p) => p.evaluate(() => window.ns0.constraints()));
      await browser.close();
      if (name === 'chromium' && CORPUS) {
        const tmp = join(tmpdir(), `ns0-${process.pid}`);
        mkdirSync(tmp, { recursive: true });
        const wav = mixWav(tmp);
        res.record = {};
        for (const ns of [true, false]) {
          const b = await launch('chromium', wav);
          const r = await withPage(b, name, (p) => p.evaluate((o) => window.ns0.record(o), { noiseSuppression: ns, seconds: REC_SECONDS }));
          await b.close();
          if (r.f32) {
            writeFileSync(join(CAPTURE_OUT, `capture-ns-${ns ? 'on' : 'off'}.f32`), Buffer.from(r.f32, 'base64'));
            delete r.f32;
          }
          res.record[ns ? 'nsOn' : 'nsOff'] = r;
        }
      }
      writeFileSync(join(OUT, `browser-${name}.json`), JSON.stringify(res, null, 2) + '\n');
      console.log(JSON.stringify({ caps: res.caps, cpu: Object.fromEntries(Object.entries(res.cpuMain).map(([k, v]) => [k, v.msPerFrame ?? v.error ?? v.fatal])), worklet: res.worklet.map((w) => [w.variant, w.via, w.contextRate, w.kind, w.benchMsPerFrame, w.error, w.realtime?.framesPerSecond?.toFixed(1)]), rates: res.rates, constraints: res.constraints }, null, 1));
    }
  } finally {
    srv.close();
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
