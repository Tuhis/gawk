#!/usr/bin/env node
// R79 MV0 — Playwright driver for voice-pipeline.html (docs/80 §9 MV0 (a)).
//
// Usage (from the repo root; see docs/assets/80/RESULTS.md §Reproduce):
//   (cd e2e && npm ci)                                   # playwright-core
//   (cd e2e && npx playwright-core install chromium firefox webkit)
//   node docs/assets/80/harness/run.mjs [--browsers chromium,firefox,webkit]
//        [--seconds 6] [--reps 1] [--relay-bin PATH] [--out DIR] [--only LABEL-SUBSTRING]
//
// --relay-bin: a gawk-server binary; the driver starts it with -dev-cert on
// 127.0.0.1, scrapes the cert hash from its log (as e2e/run.mjs does) and adds
// WebTransport /echo loopback runs to the matrix.

import { spawn } from 'node:child_process';
import { createReadStream, existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, extname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '../../../..');
const require = createRequire(join(REPO, 'e2e', 'package.json'));
const pw = require('playwright-core');

const argv = process.argv.slice(2);
const arg = (name, dflt) => {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 ? argv[i + 1] : dflt;
};
const BROWSERS = arg('browsers', 'chromium,firefox,webkit').split(',');
const SECONDS = Number(arg('seconds', '6'));
const REPS = Number(arg('reps', '1'));
const OUT = resolve(arg('out', join(HERE, '..', 'results')));
const RELAY_BIN = arg('relay-bin', null);
const ONLY = arg('only', null);

// A 48 kHz mono 16-bit WAV of the same tone-burst train the page synthesises
// (20 ms of 1 kHz every 12350 frames = 257.3 ms), for Chromium's --use-file-for-fake-audio-capture.
function writeBurstWav(path) {
  const sr = 48000;
  const period = 12350;
  const n = period * 8; // ~2 s, looped by Chromium
  const data = Buffer.alloc(n * 2);
  for (let i = 0; i < n; i++) {
    const p = i % period;
    const v = p < 0.02 * sr ? 0.5 * Math.sin((2 * Math.PI * 1000 * p) / sr) : 0;
    data.writeInt16LE(Math.round(v * 32767), i * 2);
  }
  const h = Buffer.alloc(44);
  h.write('RIFF', 0); h.writeUInt32LE(36 + data.length, 4); h.write('WAVE', 8);
  h.write('fmt ', 12); h.writeUInt32LE(16, 16); h.writeUInt16LE(1, 20); h.writeUInt16LE(1, 22);
  h.writeUInt32LE(sr, 24); h.writeUInt32LE(sr * 2, 28); h.writeUInt16LE(2, 32); h.writeUInt16LE(16, 34);
  h.write('data', 36); h.writeUInt32LE(data.length, 40);
  writeFileSync(path, Buffer.concat([h, data]));
}

function serveHarness() {
  const types = { '.html': 'text/html', '.js': 'text/javascript' };
  const srv = createServer((req, res) => {
    const p = new URL(req.url, 'http://x').pathname.replace(/^\/+/, '') || 'voice-pipeline.html';
    const f = join(HERE, p);
    if (!f.startsWith(HERE) || !existsSync(f)) { res.writeHead(404).end(); return; }
    res.writeHead(200, { 'content-type': types[extname(f)] ?? 'application/octet-stream', 'cache-control': 'no-store' });
    createReadStream(f).pipe(res);
  });
  return new Promise((r) => srv.listen(0, '127.0.0.1', () => r(srv)));
}

function startRelay(bin) {
  const port = 20000 + Math.floor(Math.random() * 20000);
  const opsPort = port + 1;
  const child = spawn(bin, ['-dev-cert', '-addr', `127.0.0.1:${port}`, '-metrics-addr', `127.0.0.1:${opsPort}`],
    { stdio: ['ignore', 'pipe', 'pipe'] });
  let buf = '';
  return new Promise((res, rej) => {
    const t = setTimeout(() => rej(new Error(`no cert hash from relay:\n${buf}`)), 15000);
    const on = (d) => {
      buf += d;
      const m = buf.match(/cert_hash_hex\W*"?([0-9a-f]{64})/);
      if (m) { clearTimeout(t); res({ child, url: `https://127.0.0.1:${port}/echo`, hash: m[1] }); }
    };
    child.stdout.on('data', on);
    child.stderr.on('data', on);
  });
}

async function launch(name, wav) {
  if (name === 'chromium') {
    return pw.chromium.launch({
      channel: 'chromium', // the full browser in new headless, not headless-shell
      headless: true,
      args: ['--no-sandbox', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream',
        `--use-file-for-fake-audio-capture=${wav}`, '--autoplay-policy=no-user-gesture-required'],
    });
  }
  if (name === 'firefox') {
    return pw.firefox.launch({
      headless: true,
      firefoxUserPrefs: {
        'media.navigator.streams.fake': true,
        'media.navigator.permission.disabled': true,
        'media.autoplay.default': 0,
        'media.autoplay.block-webaudio': false,
        'media.autoplay.blocking_policy': 0,
        // Test-only: full-resolution performance.now() (default clamps to 1 ms).
        'privacy.reduceTimerPrecision': false,
      },
    });
  }
  return pw.webkit.launch({ headless: true });
}

function matrix(name, haveRelay) {
  const runs = [];
  const captures = name === 'chromium' ? ['mstp', 'worklet'] : ['worklet', 'mstp'];
  for (const capture of captures) {
    for (const source of ['synth', 'gum']) {
      for (const fd of [10000, 20000]) runs.push({ capture, source, frameDurationUs: fd, transport: 'inpage' });
    }
  }
  for (const application of ['voip', 'lowdelay']) {
    for (const fd of [10000, 20000]) runs.push({ capture: 'worklet', source: 'synth', frameDurationUs: fd, transport: 'inpage', application });
  }
  if (haveRelay) {
    for (const fd of [10000, 20000]) runs.push({ capture: 'worklet', source: 'synth', frameDurationUs: fd, transport: 'wt' });
  }
  return runs;
}

function label(r, rep) {
  return [r.capture, r.source, `${r.frameDurationUs / 1000}ms`, r.transport, r.application ?? 'default', `rep${rep}`].join('-');
}

async function main() {
  const tmp = join(tmpdir(), `mv0-${process.pid}`);
  mkdirSync(tmp, { recursive: true });
  const wav = join(tmp, 'bursts.wav');
  writeBurstWav(wav);
  const srv = await serveHarness();
  const base = `http://127.0.0.1:${srv.address().port}/voice-pipeline.html`;
  let relay = null;
  if (RELAY_BIN) {
    relay = await startRelay(RELAY_BIN);
    console.log(`[mv0] relay /echo at ${relay.url}`);
  }
  try {
    for (const name of BROWSERS) {
      const browser = await launch(name, wav);
      const version = browser.version();
      const dir = join(OUT, name);
      mkdirSync(dir, { recursive: true });
      console.log(`[mv0] ${name} ${version}`);
      const meta = { browser: name, version, playwright: require('playwright-core/package.json').version,
        host: `${process.platform} ${process.arch}`, headless: true, date: new Date().toISOString(), seconds: SECONDS };
      for (const r of matrix(name, !!relay)) {
        for (let rep = 0; rep < REPS; rep++) {
          const l = label(r, rep);
          if (ONLY && !l.includes(ONLY)) continue;
          const context = await browser.newContext();
          if (name === 'webkit') await context.grantPermissions(['microphone']).catch(() => {});
          if (name === 'chromium') await context.grantPermissions(['microphone']).catch(() => {});
          const page = await context.newPage();
          const consoleLines = [];
          page.on('console', (m) => consoleLines.push(`${m.type()}: ${m.text()}`));
          page.on('pageerror', (e) => consoleLines.push(`pageerror: ${e}`));
          let res;
          try {
            await page.goto(base);
            await page.waitForFunction(() => window.mv0Ready === true, null, { timeout: 15000 });
            res = await page.evaluate((o) => window.mv0.run(o),
              { ...r, seconds: SECONDS, echoUrl: relay?.url, echoHash: relay?.hash });
          } catch (e) {
            res = { opts: r, fatal: String(e) };
          }
          if (res.track?.settings) { delete res.track.settings.deviceId; delete res.track.settings.groupId; }
          res.meta = { ...meta, label: l };
          res.console = consoleLines.slice(0, 50);
          // Summary fields pretty, the per-packet raw arrays on one line.
          const { raw, ...rest } = res;
          const pretty = JSON.stringify(rest, null, 2);
          writeFileSync(join(dir, `${l}.json`), (raw ? `${pretty.slice(0, -2)},\n  "raw": ${JSON.stringify(raw)}\n}` : pretty) + '\n');
          const e = res.encoder;
          console.log(`[mv0] ${name} ${l}: ${res.fatal ?? (res.errors?.length ? res.errors.join('; ') : 'ok')}` +
            (e ? ` pkts=${e.packets} dur=${JSON.stringify(e.packetDurationUsHist)} enc p50=${e.latencyMs.median?.toFixed(2)} dec p50=${res.decoder.latencyMs.median?.toFixed(2)} inGraph p50=${res.inGraphLatencyMs?.median?.toFixed(1)}` : ''));
          await context.close();
        }
      }
      // Opus lookahead / pre-skip alignment probe (sample indices only).
      {
        const context = await browser.newContext();
        const page = await context.newPage();
        const res = { meta: { ...meta, label: 'alignment' }, runs: [] };
        try {
          await page.goto(base);
          await page.waitForFunction(() => window.mv0Ready === true, null, { timeout: 15000 });
          for (const app of [null, 'lowdelay']) {
            for (const fd of [10000, 20000]) {
              for (const at of [4800 + 123, 4800 + 300, 4800 + 450]) {
                res.runs.push(await page.evaluate(([f, a, ap]) => window.mv0.alignment({ frameDurationUs: f, impulseAt: a, application: ap })
                  .catch((e) => ({ frameDurationUs: f, impulseAt: a, application: ap, fatal: String(e) })), [fd, at, app]));
              }
            }
          }
        } catch (e) {
          res.fatal = String(e);
        }
        writeFileSync(join(dir, 'alignment.json'), JSON.stringify(res, null, 2) + '\n');
        console.log(`[mv0] ${name} alignment: ${JSON.stringify(res.runs.map((r) => [r.application, r.frameDurationUs / 1000, r.decodedMinusInputSamples, r.blocksFedWhenImpulsePacketEmitted, r.minBlocksPossible, r.fatal]))}`);
        await context.close();
      }
      // AEC probe: headless smoke only (fake devices do not hear speakers).
      {
        const context = await browser.newContext();
        if (name !== 'firefox') await context.grantPermissions(['microphone']).catch(() => {});
        const page = await context.newPage();
        let res;
        try {
          await page.goto(base);
          await page.waitForFunction(() => window.mv0Ready === true, null, { timeout: 15000 });
          res = await page.evaluate(() => window.mv0.aec({ echoCancellation: true, seconds: 3 }));
        } catch (e) {
          res = { fatal: String(e) };
        }
        if (res.trackSettings) { delete res.trackSettings.deviceId; delete res.trackSettings.groupId; }
        res.meta = { ...meta, label: 'aec-smoke', note: 'headless fake device: proves the code path runs; the number means nothing' };
        writeFileSync(join(dir, 'aec-smoke.json'), JSON.stringify(res, null, 2) + '\n');
        await context.close();
      }
      writeFileSync(join(dir, 'meta.json'), JSON.stringify(meta, null, 2) + '\n');
      await browser.close();
    }
  } finally {
    srv.close();
    if (relay) relay.child.kill();
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
