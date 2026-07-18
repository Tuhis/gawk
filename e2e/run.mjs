#!/usr/bin/env node
// The R20 browser E2E harness (docs/25 Decisions 4–6): a real Chromium viewer
// decoding real relayed frames, verdict read from the viewer's own
// diagnostics. Two modes:
//
//   node run.mjs              tier 1 — owns the whole stack: spawns
//                             gawk-server (-dev-cert), gawk-pubsim, and
//                             `vite preview`, then drives the browser and
//                             also asserts the relay side via the ops
//                             endpoint (/statusz).
//   node run.mjs --external   tier 2 — the relay already runs elsewhere
//                             (kind); takes GAWK_E2E_URL + GAWK_E2E_CERT_HASH
//                             + GAWK_E2E_ID, spawns only `vite preview` and
//                             the browser. Relay-side assertions live in
//                             cluster-assert.sh (per pod).
//
// Environment (all optional in tier 1):
//   GAWK_E2E_SERVER_BIN  path to gawk-server        (default e2e/bin/gawk-server)
//   GAWK_E2E_PUBSIM_BIN  path to gawk-pubsim        (default e2e/bin/gawk-pubsim)
//   GAWK_E2E_CHROME      Chrome/Chromium executable (default: first of the
//                        usual system locations)
//   GAWK_E2E_APP_DIR     built gawk-app checkout    (default ../gawk-app)
//   GAWK_E2E_RELAY_PORT / GAWK_E2E_OPS_PORT / GAWK_E2E_PREVIEW_PORT
//   GAWK_E2E_URL / GAWK_E2E_CERT_HASH / GAWK_E2E_ID / GAWK_E2E_OPS  (external)
//
// Assertions are flow-shaped, never rate-shaped (Decision 6): a contended
// 2-core CI runner cannot promise 30 fps, but it can promise that frames
// arrive, decode, and render, and that drops stay bounded.

import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';
import { PNG } from 'pngjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, 'out');
const EXTERNAL = process.argv.includes('--external');

const RELAY_PORT = Number(process.env.GAWK_E2E_RELAY_PORT ?? 4433);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2112);
const PREVIEW_PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4173);
const APP_DIR = resolve(HERE, process.env.GAWK_E2E_APP_DIR ?? '../gawk-app');
const SERVER_BIN = resolve(HERE, process.env.GAWK_E2E_SERVER_BIN ?? 'bin/gawk-server');
const PUBSIM_BIN = resolve(HERE, process.env.GAWK_E2E_PUBSIM_BIN ?? 'bin/gawk-pubsim');
const APP_URL = `http://127.0.0.1:${PREVIEW_PORT}`;
// ~5 s between the two diagnostics captures (Decision 6: "sustained").
const SAMPLE_GAP_MS = 5000;
// One hard cap on the whole run — a hung QUIC handshake must fail, not hang.
const WATCHDOG_MS = EXTERNAL ? 180_000 : 240_000;

const children = [];
let watchdog;

function log(msg) {
  console.log(`[e2e] ${msg}`);
}

function fail(msg) {
  throw new Error(msg);
}

function chromePath() {
  const candidates = process.env.GAWK_E2E_CHROME
    ? [process.env.GAWK_E2E_CHROME]
    : [
        '/usr/bin/google-chrome',
        '/usr/bin/google-chrome-stable',
        '/usr/bin/chromium',
        '/usr/bin/chromium-browser',
      ];
  for (const c of candidates) if (existsSync(c)) return c;
  fail(`no Chrome found (tried ${candidates.join(', ')}); set GAWK_E2E_CHROME`);
}

// Spawns a child whose stdout+stderr stream to out/<name>.log and into an
// in-memory buffer that waitForLine() greps.
function launch(name, cmd, args, opts = {}) {
  const logPath = join(OUT, `${name}.log`);
  writeFileSync(logPath, `$ ${cmd} ${args.join(' ')}\n`);
  const child = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'], ...opts });
  const entry = { name, child, buf: '', exited: null };
  const sink = (chunk) => {
    entry.buf += chunk;
    appendFileSync(logPath, chunk);
  };
  child.stdout.on('data', sink);
  child.stderr.on('data', sink);
  child.on('exit', (code, signal) => {
    entry.exited = { code, signal };
    log(`${name} exited (code=${code} signal=${signal})`);
  });
  children.push(entry);
  return entry;
}

async function waitForLine(entry, regex, timeoutMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const m = entry.buf.match(regex);
    if (m) return m[1];
    if (entry.exited) fail(`${entry.name} exited before ${what} (see out/${entry.name}.log)`);
    await sleep(100);
  }
  fail(`timeout waiting for ${what} from ${entry.name}`);
}

async function waitForHttp(url, timeoutMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const rsp = await fetch(url);
      if (rsp.ok) return;
    } catch {
      // not up yet
    }
    await sleep(200);
  }
  fail(`timeout waiting for ${what} at ${url}`);
}

async function pollFor(fn, timeoutMs, intervalMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await sleep(intervalMs);
  }
  fail(`timeout waiting for ${what}`);
}

function cleanup() {
  for (const { name, child, exited } of children) {
    if (!exited) {
      log(`stopping ${name}`);
      child.kill('SIGTERM');
    }
  }
}

// ---------------------------------------------------------------------------
// Browser scenario
// ---------------------------------------------------------------------------

// dt/dd row lookup in the stats overlay (the gawk-app:verify recipe; CSS
// module classes are hashed, the dt text is the stable handle).
function rowValue(page, label) {
  return page
    .locator('dt')
    .filter({ hasText: new RegExp(`^${label}$`) })
    .locator('xpath=following-sibling::dd')
    .textContent();
}

async function captureDiagnostics(page, name) {
  const before = await page.evaluate(() => window.__gawkClipboard.length);
  // The button flips to "Copied" for ~1.8 s after a click.
  await page.getByRole('button', { name: /Copy diagnostics|Copied/ }).click();
  await pollFor(
    () => page.evaluate((n) => window.__gawkClipboard.length > n, before),
    5000,
    100,
    'the diagnostics JSON on the stubbed clipboard',
  );
  const text = await page.evaluate(() => window.__gawkClipboard.at(-1));
  writeFileSync(join(OUT, `${name}.json`), text);
  return JSON.parse(text);
}

function latest(diag) {
  return diag.samples.at(-1).stats;
}

// Median over the last ≤6 samples (~3 s at the 500 ms stats cadence): one bad
// tick on a contended runner must not flip the verdict, but a sustained stall
// still shows.
function medianRecent(diag, key) {
  const vals = diag.samples
    .slice(-6)
    .map((s) => s.stats[key])
    .filter((v) => typeof v === 'number' && Number.isFinite(v))
    .sort((a, b) => a - b);
  if (vals.length === 0) return null;
  return vals[Math.floor(vals.length / 2)];
}

// Decision 6's flow-shaped assertions. d1/d2 are the two diagnostics captures
// ~5 s apart; every violation is collected so one run reports them all.
function assertFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };

  // The codec is fully determined by the committed fixture's SPS — a
  // mismatch means the config path (or the fixture) changed.
  check(d2.codec === 'avc1.42C00D', `codec = ${d2.codec}, want the fixture's avc1.42C00D`);

  for (const [name, d] of [
    ['capture 1', d1],
    ['capture 2', d2],
  ]) {
    // Floors are deliberately far below the 30 fps source rate (Decision 6:
    // flow, not performance).
    const received = medianRecent(d, 'receivedFps');
    const decoded = medianRecent(d, 'decoderFps');
    const rendered = medianRecent(d, 'renderedFps');
    check(received > 0, `${name}: median received fps = ${received}, want > 0`);
    check(decoded >= 10, `${name}: median decoded fps = ${decoded}, want >= 10`);
    check(rendered > 0, `${name}: median rendered fps = ${rendered}, want > 0`);
  }

  const s1 = latest(d1);
  const s2 = latest(d2);
  check(
    s2.timeSinceLastFrameMs != null && s2.timeSinceLastFrameMs < 3000,
    `last frame ${s2.timeSinceLastFrameMs} ms ago, want < 3000`,
  );
  check(
    s2.lastKeyframeAgeMs != null && s2.lastKeyframeAgeMs < 5000,
    `last keyframe ${s2.lastKeyframeAgeMs} ms ago, want < 5000 (fixture GOP is 500 ms)`,
  );

  const growth = (key) => s2[key] - s1[key];
  const completed = growth('framesCompleted');
  check(completed > 0, `no frames completed between the captures`);
  // "Not growing unbounded": more frames must complete than drop, and the
  // awaiting-keyframe discard must not run at the gap-resync-per-GOP
  // signature (~10/s at the fixture cadence — docs/14).
  const dropped = growth('framesDroppedIncomplete') + growth('framesDroppedLate');
  check(
    dropped <= completed,
    `dropped ${dropped} vs completed ${completed} between captures — drops dominate`,
  );
  const awaiting = growth('framesDiscardedAwaitingKey');
  check(awaiting < 30, `${awaiting} frames discarded awaiting keyframe between captures`);
  // Same build on both ends: any unparseable datagram is a real wire bug.
  check(s2.badDatagrams === 0, `viewer counted ${s2.badDatagrams} bad datagrams`);

  if (problems.length > 0) {
    fail(`flow assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `flow ok: received/decoded/rendered ≈ ${medianRecent(d2, 'receivedFps')}/${medianRecent(d2, 'decoderFps')}/${medianRecent(d2, 'renderedFps')} fps, ` +
      `${completed} frames completed between captures`,
  );
}

// The composited-screenshot non-black check (Decision 6 secondary; canvas
// readback is rejected — the R16 finding stands).
function assertNonBlack(pngBuffer) {
  const png = PNG.sync.read(pngBuffer);
  const { width, height, data } = png;
  if (width < 16 || height < 16) fail(`canvas screenshot is ${width}x${height}`);
  let sum = 0;
  let sumSq = 0;
  let max = 0;
  const n = width * height;
  for (let i = 0; i < n * 4; i += 4) {
    const luma = 0.299 * data[i] + 0.587 * data[i + 1] + 0.114 * data[i + 2];
    sum += luma;
    sumSq += luma * luma;
    if (luma > max) max = luma;
  }
  const mean = sum / n;
  const stddev = Math.sqrt(Math.max(0, sumSq / n - mean * mean));
  log(`canvas pixels: mean luma ${mean.toFixed(1)}, stddev ${stddev.toFixed(1)}, max ${max.toFixed(0)}`);
  if (max < 40) fail(`canvas is black (max luma ${max.toFixed(0)})`);
  if (stddev < 8) fail(`canvas is uniform (luma stddev ${stddev.toFixed(1)}) — not video`);
}

async function browserScenario({ relayUrl, certHash, id, attempt }) {
  const browser = await chromium.launch({
    executablePath: chromePath(),
    headless: true,
    // The sandbox needs user namespaces the runner/container may not have;
    // this is a test harness, not a browsing session.
    args: ['--no-sandbox'],
  });
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
    // Seed the persisted transport settings (the keys useTransportStore owns)
    // and stub the clipboard (the ViewerScreen.test.tsx precedent) before any
    // app code runs.
    await context.addInitScript(
      ({ serverUrl, hash }) => {
        localStorage.setItem('gawk.serverUrl', serverUrl);
        if (hash) localStorage.setItem('gawk.certHashHex', hash);
        window.__gawkClipboard = [];
        Object.defineProperty(Navigator.prototype, 'clipboard', {
          configurable: true,
          get: () => ({
            writeText: (text) => {
              window.__gawkClipboard.push(text);
              return Promise.resolve();
            },
          }),
        });
      },
      { serverUrl: relayUrl, hash: certHash },
    );
    const page = await context.newPage();
    const consoleLog = join(OUT, `console-${attempt}.log`);
    writeFileSync(consoleLog, '');
    page.on('console', (msg) => appendFileSync(consoleLog, `[${msg.type()}] ${msg.text()}\n`));
    page.on('pageerror', (err) => appendFileSync(consoleLog, `[pageerror] ${err}\n`));

    await page.goto(`${APP_URL}/#/view/${id}`);

    // Open the stats overlay (Ctrl+Alt+Shift+D). Press-and-check: the hotkey
    // toggles, so never press while it is already visible.
    const overlay = page.locator('[role="dialog"][aria-label="Stream stats"]');
    await pollFor(
      async () => {
        if (await overlay.isVisible()) return true;
        await page.keyboard.press('Control+Alt+Shift+D');
        await sleep(300);
        return overlay.isVisible();
      },
      15_000,
      500,
      'the stats overlay',
    );

    // Frames flowing: the Delivery section's Completed counter goes numeric
    // and positive once the primed keyframe + deltas decode.
    await pollFor(
      async () => {
        const v = (await rowValue(page, 'Completed'))?.trim();
        return /^\d+$/.test(v) && Number(v) > 0;
      },
      30_000,
      1000,
      'the first completed frame',
    );
    log('viewer is receiving frames; sampling diagnostics');

    const diag1 = await captureDiagnostics(page, `diagnostics-${attempt}-1`);
    await sleep(SAMPLE_GAP_MS);
    const diag2 = await captureDiagnostics(page, `diagnostics-${attempt}-2`);
    assertFlow(diag1, diag2);

    const shot = await page.locator('canvas').first().screenshot();
    writeFileSync(join(OUT, `viewer-${attempt}.png`), shot);
    assertNonBlack(shot);
  } finally {
    await browser.close();
  }
}

// ---------------------------------------------------------------------------
// Relay-side assertions (tier 1 / when an ops endpoint is reachable)
// ---------------------------------------------------------------------------

async function assertRelaySide(opsUrl) {
  const rsp = await fetch(`${opsUrl}/statusz`);
  if (!rsp.ok) fail(`statusz: HTTP ${rsp.status}`);
  const st = await rsp.json();
  const entries = Object.values(st.broadcasts ?? {});
  if (entries.length !== 1) fail(`relay shows ${entries.length} broadcasts, want 1`);
  const b = entries[0];
  const problems = [];
  if (!b.publisherActive) problems.push('publisher not active');
  // Loopback link: loss here means our frame IDs disagree with the relay's
  // ingress window — a real bug, not network weather.
  if (b.ingressFramesLost !== 0 || b.ingressChunksLost !== 0) {
    problems.push(`ingress loss ${b.ingressFramesLost} frames / ${b.ingressChunksLost} chunks on loopback`);
  }
  if (b.badDatagrams !== 0) problems.push(`relay rejected ${b.badDatagrams} datagrams`);
  if (problems.length > 0) fail(`relay-side assertions failed: ${problems.join('; ')}`);
  log(`relay side ok: ${b.framesRelayed} frames relayed, ${b.keyframeStreamsIn} keyframe streams in, zero ingress loss`);
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  mkdirSync(OUT, { recursive: true });
  watchdog = setTimeout(() => {
    console.error(`[e2e] watchdog: run exceeded ${WATCHDOG_MS / 1000}s, aborting`);
    cleanup();
    process.exit(1);
  }, WATCHDOG_MS);

  let relayUrl, certHash, id, opsUrl;
  if (EXTERNAL) {
    relayUrl = process.env.GAWK_E2E_URL ?? fail('external mode needs GAWK_E2E_URL');
    certHash = process.env.GAWK_E2E_CERT_HASH ?? fail('external mode needs GAWK_E2E_CERT_HASH');
    id = process.env.GAWK_E2E_ID ?? fail('external mode needs GAWK_E2E_ID');
    opsUrl = process.env.GAWK_E2E_OPS ?? null;
  } else {
    relayUrl = `https://127.0.0.1:${RELAY_PORT}`;
    opsUrl = `http://127.0.0.1:${OPS_PORT}`;
    const relay = launch('relay', SERVER_BIN, [
      '-dev-cert',
      '-addr', `127.0.0.1:${RELAY_PORT}`,
      '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
    ]);
    // Decision 5: scrape the ephemeral dev cert's hash from the startup log
    // and seed it into the app's serverCertificateHashes setting.
    certHash = await waitForLine(relay, /cert_hash_hex\W*"?([0-9a-f]{64})/, 15_000, 'the dev cert hash');
    await waitForHttp(`${opsUrl}/healthz`, 15_000, 'the relay ops endpoint');

    const pubsim = launch('pubsim', PUBSIM_BIN, [
      '-url', relayUrl,
      '-insecure',
      '-duration', '600s',
    ]);
    id = await waitForLine(pubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the broadcast code');
    log(`pubsim publishing as ${id}`);
  }

  const vite = join(APP_DIR, 'node_modules', '.bin', 'vite');
  if (!existsSync(vite)) fail(`${vite} not found — run npm ci in ${APP_DIR}`);
  if (!existsSync(join(APP_DIR, 'dist', 'index.html'))) {
    fail(`${APP_DIR}/dist missing — run npm run build in ${APP_DIR}`);
  }
  launch('preview', vite, ['preview', '--port', String(PREVIEW_PORT), '--strictPort', '--host', '127.0.0.1'], {
    cwd: APP_DIR,
  });
  await waitForHttp(APP_URL, 20_000, 'vite preview');

  // One in-harness retry, relaunching the browser but never the relay or the
  // publisher (Decision 8) — and the retry is logged loudly: recurring
  // attempt-1 failures are findings for docs/25, not noise.
  try {
    await browserScenario({ relayUrl, certHash, id, attempt: 1 });
  } catch (err) {
    log(`RETRY: browser scenario attempt 1 failed: ${err.message ?? err}`);
    await browserScenario({ relayUrl, certHash, id, attempt: 2 });
  }

  if (opsUrl) await assertRelaySide(opsUrl);

  log('PASS');
}

try {
  await main();
} catch (err) {
  console.error(`[e2e] FAIL: ${err.stack ?? err}`);
  process.exitCode = 1;
} finally {
  clearTimeout(watchdog);
  cleanup();
  // vite preview ignores SIGTERM occasionally; give children a moment, then
  // make sure the process exits regardless of lingering handles.
  await sleep(500);
  process.exit(process.exitCode ?? 0);
}
