#!/usr/bin/env node
// R80 NS4 (docs/81 §9): RNNoise in room voice, in real browsers, against the
// in-page fake relay and bridge of voice-panel.mjs (voice-fake.mjs). The
// production capture path runs for real: getUserMedia → the capture
// AudioWorklet with the real rnnoise.wasm → the speech gate → AudioEncoder.
//
// Chromium plays a synthetic microphone from a WAV this script writes
// (--use-file-for-fake-audio-capture, looped; no third-party audio): 8 s of
// keyboard alone, then 6 s of speech over the keyboard. It asserts, each on
// a whole loop of the file measured from the getUserMedia that opened it:
//
//   1. RNNoise (the default) loads and runs: the Audio section says so; it
//      sends most of the speech;
//   2. a switch to Browser re-opens the microphone (a second getUserMedia,
//      the first track stopped) and the new track's audio decides: the
//      level gate sends nearly all of the keyboard Chromium's auto gain
//      lifts, at least three times what RNNoise sent (the synthetic clicks
//      are broadband and RNNoise passes ~15–20 % of them after Chromium's
//      processing; real typing is the corpus pass's);
//   3. a switch back to RNNoise re-opens again and the keyboard is quiet
//      again — judged on the audio sent, never on getSettings() (§12.6);
//   4. screenshots with the Audio section open at 360, 390, 430 and 1280 px,
//      no horizontal overflow at any of them.
//
// Seven seconds of keyboard cannot test docs/81 §1's per-minute row (one
// spurt is ~4 % of it), and Chromium's processing makes the synthetic clicks
// spurt more than NS0's recorded keyboard did. With --corpus DIR (the
// output of docs/assets/81/harness/fetch-corpus.sh, which downloads it at
// run time; it is never committed) a further pass plays NS0's minute of real
// keyboard alone at -36 dBFS and asserts the row: ≤ 3 % of it sent through
// Chromium with docs/81 D11's constraints.
//
// With --browsers (default chromium), a smoke run on Firefox and WebKit
// (their own fake devices): join, RNNoise running with no fallback, and
// frames flow while push-to-talk is held (a fake tone is not speech; PTT
// still denoises, D18). Playwright's WebKit needs its host libraries; where
// they are missing, run it in mcr.microsoft.com/playwright as
// docs/assets/81/harness/run-all.sh does.
//
// Run it (from the repository root):
//
//   (cd gawk-app && npm ci && npm run build)
//   (cd e2e && npm ci)
//   node e2e/voice-rnnoise.mjs [--browsers chromium,firefox,webkit] [--corpus DIR]
//
// Environment: GAWK_E2E_CHROME, GAWK_E2E_APP_DIR, GAWK_E2E_PREVIEW_PORT
// (default 4181), as voice-panel.mjs. Output in e2e/out/voice-rnnoise/.

import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium, firefox, webkit } from 'playwright-core';
import { PIN, chromePath, installFakeWebTransport, overflowAt, waitForHttp } from './voice-fake.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out', 'voice-rnnoise');
const APP_DIR = resolve(process.env.GAWK_E2E_APP_DIR ?? join(HERE, '..', 'gawk-app'));
const PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4181);
const APP_URL = `http://127.0.0.1:${PORT}/`;
const WIDTHS = [360, 390, 430, 1280];
const argValue = (name) => (process.argv.includes(name) ? process.argv[process.argv.indexOf(name) + 1] : null);
const BROWSERS = (argValue('--browsers') ?? 'chromium').split(',');
const CORPUS = argValue('--corpus');

// The fake microphone's timeline, seconds.
const RATE = 48_000;
const KEYBOARD_S = 8;
const SPEECH_S = 6;
const LOOP_S = KEYBOARD_S + SPEECH_S;
// The windows judged inside one loop, leaving the edges to start-up and
// RNNoise's hold.
const KEYBOARD_WINDOW = [1, KEYBOARD_S - 0.5];
const SPEECH_WINDOW = [KEYBOARD_S + 0.5, LOOP_S - 0.5];
// The corpus pass: a minute of keyboard alone.
const MINUTE_S = 60;

const log = (...a) => console.log('[voice-rnnoise]', ...a);
function fail(msg) {
  console.error(`[voice-rnnoise] FAIL: ${msg}`);
  process.exitCode = 1;
  throw new Error(msg);
}

// ---- The synthetic microphone (gawk-app voiceTestKit.ts's generators) --------

function rng(seed) {
  let s = seed >>> 0;
  return () => {
    s = (s * 1103515245 + 12345) >>> 0;
    return s / 2 ** 32;
  };
}

const FORMANTS = [
  [730, 1090, 2440],
  [270, 2290, 3010],
  [570, 840, 2410],
  [300, 870, 2240],
  [530, 1840, 2480],
];

// Syllables of a pulse train through three formant resonators, in talk runs
// with short pauses; RNNoise 0.2 calls it speech.
function speech(seconds, seed) {
  const r = rng(seed);
  const n = Math.floor(seconds * RATE);
  const out = new Float32Array(n);
  const res = [0, 1, 2].map(() => ({ y1: 0, y2: 0 }));
  let i = Math.floor(0.2 * RATE);
  let phase = 0;
  while (i < n - 0.3 * RATE) {
    const talkEnd = Math.min(n - Math.floor(0.3 * RATE), i + Math.floor((1.2 + 1.0 * r()) * RATE));
    while (i < talkEnd) {
      const len = Math.floor((0.15 + 0.15 * r()) * RATE);
      const f = FORMANTS[Math.floor(r() * FORMANTS.length)];
      const f0 = 100 + 40 * r();
      for (let k = 0; k < len && i < n; k++, i++) {
        const t = k / len;
        phase += (f0 + 15 * t) / RATE;
        let src = 0.02 * (r() - 0.5);
        if (phase >= 1) {
          phase -= 1;
          src += 1;
        }
        let y = 0;
        for (let j = 0; j < 3; j++) {
          const rad = Math.exp((-Math.PI * (80 + 40 * j)) / RATE);
          const th = (2 * Math.PI * f[j]) / RATE;
          const s = res[j];
          const v = src + 2 * rad * Math.cos(th) * s.y1 - rad * rad * s.y2;
          s.y2 = s.y1;
          s.y1 = v;
          y += v / (j + 1);
        }
        out[i] = y * Math.sqrt(Math.sin(Math.PI * t));
      }
    }
    for (const s of res) s.y1 = s.y2 = 0;
    i += Math.floor((0.25 + 0.25 * r()) * RATE);
  }
  return out;
}

function keyboard(seconds, seed) {
  const r = rng(seed);
  const n = Math.floor(seconds * RATE);
  const out = new Float32Array(n);
  for (let i = Math.floor(r() * 0.1 * RATE); i < n; i += Math.floor((0.08 + 0.25 * r()) * RATE)) {
    const len = 300 + Math.floor(r() * 400);
    const a = 0.5 + r();
    for (let j = 0; j < len && i + j < n; j++) out[i + j] += a * (r() * 2 - 1) * Math.exp(-j / 60);
  }
  return out;
}

const rms = (x) => Math.sqrt(x.reduce((a, v) => a + v * v, 0) / x.length);
const scaled = (x, dbfs) => {
  const g = 10 ** (dbfs / 20) / rms(x);
  return x.map((v) => v * g);
};

// 8 s of keyboard at -38 dBFS, then speech at -26 dBFS over it (NS0 §12.4's
// shape).
function syntheticMic() {
  const kb = scaled(keyboard(LOOP_S, 5), -38);
  const sp = scaled(speech(SPEECH_S, 9), -26);
  const x = kb.slice();
  for (let i = 0; i < sp.length; i++) x[KEYBOARD_S * RATE + i] += sp[i];
  return x;
}

// NS0's keyboard minute (fetch-corpus.sh's 48 kHz f32le), at -36 dBFS.
function corpusKeyboard(dir) {
  const buf = readFileSync(join(dir, 'noise-keyboard.f32'));
  const x = new Float32Array(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
  return scaled(x.slice(0, MINUTE_S * RATE), -36);
}

// 16-bit mono 48 kHz PCM WAV.
function writeWav(path, x) {
  const data = Buffer.alloc(x.length * 2);
  for (let i = 0; i < x.length; i++) data.writeInt16LE(Math.max(-32768, Math.min(32767, Math.round(x[i] * 32767))), i * 2);
  const h = Buffer.alloc(44);
  h.write('RIFF', 0);
  h.writeUInt32LE(36 + data.length, 4);
  h.write('WAVEfmt ', 8);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(1, 22);
  h.writeUInt32LE(RATE, 24);
  h.writeUInt32LE(RATE * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write('data', 36);
  h.writeUInt32LE(data.length, 40);
  writeFileSync(path, Buffer.concat([h, data]));
}

// ---- In the page ---------------------------------------------------------------

// Every getUserMedia: when it resolved and its track, so a window can be
// placed on the file's timeline and a re-open seen to stop the old track.
// And the room context's rate, which decides whether the worklet resamples.
function instrumentPage() {
  const AC = window.AudioContext;
  window.AudioContext = class extends AC {
    constructor(o) {
      super(o);
      window.__gawkContextRate = this.sampleRate;
      window.__gawkContext = this;
    }
  };
  const md = navigator.mediaDevices;
  const orig = md.getUserMedia.bind(md);
  window.__gum = [];
  md.getUserMedia = async (c) => {
    const s = await orig(c);
    window.__gum.push({ at: performance.now(), track: s.getAudioTracks()[0], asked: c?.audio?.noiseSuppression });
    return s;
  };
}

async function joinVoice(page, name) {
  await page.goto(`${APP_URL}#/room/AB2CD3`);
  await page.getByRole('button', { name: 'People and chat' }).click();
  const section = page.getByTestId('voice-section');
  await section.waitFor({ timeout: 15_000 });
  await section.getByRole('button', { name: 'Join voice' }).click();
  const by = Date.now() + 20_000;
  for (;;) {
    if ((await section.getByText(/Couldn’t/).count()) > 0) fail(`${name}: voice failed: ${await section.innerText()}`);
    const dock = page.getByTestId('voice-dock');
    if ((await dock.count()) > 0 && (await dock.innerText()).includes('Voice') && !(await dock.innerText()).includes('Voice…')) break;
    if (Date.now() > by) fail(`${name}: never got into voice: ${await section.innerText()}`);
    await sleep(200);
  }
  await openAudio(section);
  return section;
}

// Opens the Audio section, and keeps it open: a click on the summary that
// lands mid-render is retried.
async function openAudio(section) {
  const audio = section.getByTestId('voice-audio');
  for (let i = 0; i < 10; i++) {
    if (await audio.evaluate((el) => el.open)) return;
    await audio.locator('summary').click();
    await sleep(200);
  }
  fail(`the Audio section did not open: ${await audio.innerText()}`);
}

async function failWithShot(section, msg) {
  const page = section.page();
  await page.screenshot({ path: join(OUT, 'failure.png'), fullPage: true }).catch(() => {});
  // What the next person needs to tell a slow download from a dead worklet.
  const st = await page
    .evaluate(() => ({
      context: window.__gawkContext?.state ?? null,
      rate: window.__gawkContextRate ?? null,
      gum: (window.__gum ?? []).length,
      framesOut: window.__voiceE2E?.framesOut ?? null,
      wasm: performance
        .getEntriesByType('resource')
        .filter((e) => e.name.endsWith('.wasm'))
        .map((e) => ({ name: e.name, start: Math.round(e.startTime), end: Math.round(e.responseEnd), bytes: e.transferSize })),
    }))
    .catch((e) => String(e));
  log(`browser ${page.context().browser()?.version()}; ${JSON.stringify(st)}`);
  fail(msg);
}

// RNNoise running: the cost line says so and nothing says loading or fallback.
async function waitForRnnoise(section, name) {
  const by = Date.now() + 20_000;
  for (;;) {
    await openAudio(section);
    // textContent, not innerText: it does not depend on how the browser
    // lays the open section out.
    const text = (await section.getByTestId('voice-audio').textContent()) ?? '';
    if (/isn’t available here/.test(text)) await failWithShot(section, `${name}: RNNoise fell back: ${text}`);
    if (/RNNoise adds 20 ms/.test(text) && !/loading/.test(text)) return;
    if (Date.now() > by) await failWithShot(section, `${name}: RNNoise never ran: ${text}`);
    await sleep(200);
  }
}

// The share of 10 ms slots in [from, to) s of the file that sent a frame, on
// the next whole loop after `after` (page time, ms), timed from getUserMedia
// number `gum`.
async function sentInLoop(page, gum, afterMs, loopS = LOOP_S, windows = { keyboard: KEYBOARD_WINDOW, speech: SPEECH_WINDOW }) {
  const { at } = await page.evaluate((i) => ({ at: window.__gum[i].at }), gum);
  const loopMs = loopS * 1000;
  const loopStart = at + Math.ceil((afterMs - at) / loopMs) * loopMs;
  const loopEnd = loopStart + loopMs;
  for (;;) {
    const now = await page.evaluate(() => performance.now());
    if (now >= loopEnd + 300) break;
    await sleep(Math.min(1000, loopEnd + 300 - now));
  }
  const times = await page.evaluate(() => window.__voiceE2E.frameTimes);
  if (process.env.GAWK_E2E_DEBUG) {
    const hist = new Array(loopS * 4).fill(0);
    for (const t of times) if (t >= loopStart && t < loopEnd) hist[Math.floor((t - loopStart) / 250)]++;
    log('frames per 250 ms over the loop:', hist.join(' '));
  }
  const share = ([a, b]) => {
    const from = loopStart + a * 1000;
    const to = loopStart + b * 1000;
    return times.filter((t) => t >= from && t < to).length / ((to - from) / 10);
  };
  return Object.fromEntries(Object.entries(windows).map(([k, w]) => [k, share(w)]));
}

async function chromiumContext(browser) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.grantPermissions(['microphone'], { origin: APP_URL.slice(0, -1) });
  await context.addInitScript(
    ({ pin }) => {
      localStorage.setItem('gawk.certHashHex', pin);
      localStorage.setItem('gawk:nickname', 'me');
      localStorage.setItem('gawk:terms-accepted', 'e2e');
      window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e' };
    },
    { pin: PIN },
  );
  await context.addInitScript(installFakeWebTransport);
  await context.addInitScript(instrumentPage);
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => {
    if (/voice|rnnoise|wasm|worklet/i.test(m.text()) || m.type() === 'error') log('page:', m.text());
  });
  return { context, page, errors };
}

const pct = (x) => `${(x * 100).toFixed(1)} %`;

async function chromiumPass(browser) {
  const { context, page, errors } = await chromiumContext(browser);
  const section = await joinVoice(page, 'chromium');
  await waitForRnnoise(section, 'chromium');
  const audio = section.getByTestId('voice-audio');
  if ((await audio.getByRole('radio', { name: 'RNNoise' }).getAttribute('aria-checked')) !== 'true') {
    fail('RNNoise is not the default');
  }
  if ((await page.evaluate(() => window.__gum[0].asked)) !== false) fail('RNNoise opened the mic with browser suppression');

  // 1. RNNoise: the speech goes out, little of the keyboard does.
  let now = await page.evaluate(() => performance.now());
  const rn = await sentInLoop(page, 0, now);
  log(`RNNoise:  keyboard ${pct(rn.keyboard)} sent, speech ${pct(rn.speech)} sent`);
  if (rn.speech < 0.4) fail(`RNNoise sent only ${pct(rn.speech)} of the speech window`);

  // 2. Browser: a re-open, the old track stopped, the level gate on the new
  // track's audio.
  await audio.getByRole('radio', { name: 'Browser' }).click();
  await page.waitForFunction(() => window.__gum.length === 2, null, { timeout: 10_000 });
  const reopen = await page.evaluate(() => ({
    oldEnded: window.__gum[0].track.readyState === 'ended',
    asked: window.__gum[1].asked,
  }));
  if (!reopen.oldEnded) fail('the re-open left the first track running');
  if (reopen.asked !== true) fail('Browser re-opened without browser suppression');
  await audio.getByText(/no added delay/).waitFor({ timeout: 5000 });
  now = await page.evaluate(() => performance.now());
  const br = await sentInLoop(page, 1, now);
  log(`Browser:  keyboard ${pct(br.keyboard)} sent, speech ${pct(br.speech)} sent`);
  // RNNoise sends a fraction of what the level gate sends of the same clicks.
  if (br.keyboard < 0.3 || rn.keyboard * 3 > br.keyboard) {
    fail(`Browser sent ${pct(br.keyboard)} of the keyboard against RNNoise's ${pct(rn.keyboard)}; the new track's audio is not deciding`);
  }
  if (br.speech < 0.4) fail(`Browser sent only ${pct(br.speech)} of the speech window`);

  // 3. Back to RNNoise: another re-open, and the keyboard is quiet again.
  await audio.getByRole('radio', { name: 'RNNoise' }).click();
  await page.waitForFunction(() => window.__gum.length === 3, null, { timeout: 10_000 });
  await waitForRnnoise(section, 'chromium (back)');
  now = await page.evaluate(() => performance.now());
  const back = await sentInLoop(page, 2, now);
  log(`RNNoise again: keyboard ${pct(back.keyboard)} sent, speech ${pct(back.speech)} sent`);
  if (back.keyboard * 3 > br.keyboard) {
    fail(`RNNoise (after the switch back) sent ${pct(back.keyboard)} of the keyboard, Browser ${pct(br.keyboard)}`);
  }
  if (back.speech < 0.4) fail(`RNNoise (after the switch back) sent only ${pct(back.speech)} of the speech`);

  // 4. Screens with the Audio section open.
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 800 });
    await sleep(400);
    await audio.scrollIntoViewIfNeeded();
    const msg = await overflowAt(page, width);
    if (msg) fail(msg);
    await page.screenshot({ path: join(OUT, `audio-${width}.png`) });
    log(`screenshot ${width}px ok`);
  }
  if (errors.length > 0) fail(`page errors: ${errors.join('; ')}`);
  await context.close();
}

// docs/81 §1's Chromium row: NS0's minute of keyboard alone, -36 dBFS, ≤ 3 %
// sent with the default gate through Chromium's processing. A minute holds
// one to six spurts and single minutes ranged 1.1–3.4 % (§12.7), so the row
// is judged on the mean of three consecutive loops.
const CORPUS_LOOPS = 3;

async function corpusPass(browser) {
  const { context, page, errors } = await chromiumContext(browser);
  const section = await joinVoice(page, 'chromium (corpus)');
  await waitForRnnoise(section, 'chromium (corpus)');
  const rate = await page.evaluate(() => window.__gawkContextRate ?? null);
  const minutes = [];
  for (let i = 0; i < CORPUS_LOOPS; i++) {
    const now = await page.evaluate(() => performance.now());
    log(`playing minute ${i + 1} of ${CORPUS_LOOPS} of keyboard…`);
    const r = await sentInLoop(page, 0, now, MINUTE_S, { keyboard: [0.5, MINUTE_S - 0.5] });
    log(`  ${pct(r.keyboard)} sent`);
    minutes.push(r.keyboard);
  }
  const mean = minutes.reduce((a, v) => a + v, 0) / minutes.length;
  log(`corpus keyboard, ${CORPUS_LOOPS} minutes${rate ? ` at a ${rate} Hz context` : ''}: mean ${pct(mean)} sent (docs/81 §1: ≤ 3 %; NS0 2.2 %)`);
  if (mean > 0.03) fail(`a minute of keyboard sent ${pct(mean)} on average (docs/81 §1: ≤ 3 %)`);
  if (errors.length > 0) fail(`page errors: ${errors.join('; ')}`);
  await context.close();
}

async function launchChromium(wav) {
  const executablePath = chromePath();
  if (!executablePath) fail('no Chrome found; set GAWK_E2E_CHROME');
  return chromium.launch({
    executablePath,
    headless: true,
    args: [
      '--no-sandbox',
      '--use-fake-device-for-media-stream',
      '--use-fake-ui-for-media-stream',
      `--use-file-for-fake-audio-capture=${wav}`,
      '--autoplay-policy=no-user-gesture-required',
    ],
  });
}

// Firefox and WebKit: join, RNNoise running, frames flow under push-to-talk.
async function smokePass(name, browser, contextOpts = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, ...contextOpts });
  await context.addInitScript(
    ({ pin }) => {
      localStorage.setItem('gawk.certHashHex', pin);
      localStorage.setItem('gawk:nickname', 'me');
      localStorage.setItem('gawk:terms-accepted', 'e2e');
      localStorage.setItem('gawk:voice-mode', 'ptt');
      window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e' };
    },
    { pin: PIN },
  );
  await context.addInitScript(installFakeWebTransport);
  await context.addInitScript(instrumentPage);
  const page = await context.newPage();
  const errors = [];
  const consoleLines = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => consoleLines.push(m.text()));
  const section = await joinVoice(page, name);
  await waitForRnnoise(section, name);
  const hold = page.getByTestId('voice-dock').getByRole('button', { name: /Hold to talk/ });
  await hold.hover();
  await page.mouse.down();
  await sleep(1500);
  await page.mouse.up();
  const out = await page.evaluate(() => window.__voiceE2E.framesOut);
  log(`${name}: RNNoise running, ${out} frames out under push-to-talk`);
  if (out < 50) {
    const st = await page.evaluate(() => ({
      context: window.__gawkContext?.state,
      rate: window.__gawkContextRate,
      gum: window.__gum.length,
      track: window.__gum[0]?.track?.readyState,
      dock: document.querySelector('[data-testid="voice-dock"]')?.innerText,
    }));
    log(`${name}: ${JSON.stringify(st)}`);
    for (const l of consoleLines.slice(-20)) log(`${name} console: ${l}`);
    fail(`${name}: only ${out} frames left the browser in 1.5 s of push-to-talk`);
  }
  await page.screenshot({ path: join(OUT, `${name}-1280.png`) });
  if (errors.length > 0) fail(`${name}: page errors: ${errors.join('; ')}`);
  await context.close();
}

async function main() {
  if (!existsSync(join(APP_DIR, 'dist', 'index.html'))) fail(`${APP_DIR}/dist missing — run npm run build in gawk-app`);
  const vite = join(APP_DIR, 'node_modules', '.bin', 'vite');
  if (!existsSync(vite)) fail(`${vite} missing — run npm ci in gawk-app`);
  mkdirSync(OUT, { recursive: true });
  const wav = join(OUT, 'mic.wav');
  writeWav(wav, syntheticMic());
  const preview = spawn(vite, ['preview', '--port', String(PORT), '--strictPort', '--host', '127.0.0.1'], {
    cwd: APP_DIR,
    stdio: 'ignore',
  });
  try {
    await waitForHttp(APP_URL, 20_000);
    if (BROWSERS.includes('chromium')) {
      const browser = await launchChromium(wav);
      try {
        await chromiumPass(browser);
      } finally {
        await browser.close();
      }
      if (CORPUS) {
        const minute = join(OUT, 'keyboard-minute.wav');
        writeWav(minute, corpusKeyboard(CORPUS));
        const b = await launchChromium(minute);
        try {
          await corpusPass(b);
        } finally {
          await b.close();
        }
      }
    }
    if (BROWSERS.includes('firefox')) {
      const ff = await firefox.launch({
        headless: true,
        firefoxUserPrefs: {
          'media.navigator.streams.fake': true,
          'media.navigator.permission.disabled': true,
          'media.autoplay.default': 0,
        },
      });
      try {
        await smokePass('firefox', ff);
      } finally {
        await ff.close();
      }
    }
    if (BROWSERS.includes('webkit')) {
      const wk = await webkit.launch({ headless: true });
      try {
        await smokePass('webkit', wk, { permissions: ['microphone'] });
      } finally {
        await wk.close();
      }
    }
    log(`ok — output in ${OUT}`);
  } finally {
    preview.kill('SIGTERM');
  }
}

main().catch((e) => {
  if (!process.exitCode) {
    console.error(e);
    process.exitCode = 1;
  }
});
