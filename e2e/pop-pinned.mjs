#!/usr/bin/env node
// R74 PP5 tier-1 (docs/76 §6.1 item 5, §11): a browser joins a relay at a
// NON-loopback address by a pinned-hash link, the way a guest joins a
// standalone PoP: `#/view/<id>?relay=https://<lan-ip>:<port>&certHash=<hex>`.
//
// It owns its stack, like run.mjs tier 1: the relay listening on the
// host's private address, gawk-pubsim, and `vite preview` of the built app.
// The relay is gawk-relay, the standalone PoP binary, serving its rolling
// certificate: the link's relay and its two hashes are read from the join
// link its console publishes (/api/state, the QR's link), so a relay that
// serves a certificate it does not publish, or one the browser refuses,
// fails here. Without a gawk-relay build (GAWK_RELAY_BIN, default
// bin/gawk-relay) a gawk-server -dev-cert stands in, and the run says so. The app's own relay is left at its localhost default, so the
// LAN relay reaches the session only through the link: the parser's
// private-literal rule, the session override, the store's hash list and
// webTransportInit's serverCertificateHashes are all on the path.
//
// Chrome's Local Network Access treats a request from a less private
// address space to a more private one as gated. The app is served from
// loopback, which is the MOST private space, so the launch flag
// --ip-address-space-overrides marks the preview origin public, making the
// relay dial public → private as it is from https://gawk.ioio.fi. The
// permission is granted the way a test can (Playwright's
// 'local-network-access', i.e. CDP Browser.grantPermissions). What a person
// sees (the headful prompt, its copy, persistence) is a PP0 measurement, not
// this test's claim.
//
// Passes:
//   1. granted, the view link: frames decode (flow, from the viewer's
//      diagnostics);
//   2. the control, no grant: the same dial is refused with
//      ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS (skipped on a Chrome
//      with no such permission);
//   3. the landing link `#/?relay=…&certHash=…`: "Save and use this server"
//      stores the relay with its hashes and selects it.
//
// Environment: GAWK_RELAY_BIN, GAWK_E2E_SERVER_BIN, GAWK_E2E_PUBSIM_BIN,
// GAWK_E2E_CHROME, GAWK_E2E_CONSOLE_PORT,
// GAWK_E2E_APP_DIR, GAWK_E2E_PREVIEW_PORT, GAWK_E2E_RELAY_PORT,
// GAWK_E2E_OPS_PORT (as in run.mjs), and GAWK_E2E_LAN_IP to pick the
// address (default: the first private-range IPv4 on a non-internal
// interface). With no such address the run fails, unless --allow-skip is
// given, in which case it logs SKIP and exits 0: the row needs a runner with
// a container or VM address.

import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { networkInterfaces } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out-pop-pinned');
const ALLOW_SKIP = process.argv.includes('--allow-skip');
const RELAY_PORT = Number(process.env.GAWK_E2E_RELAY_PORT ?? 4443);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2122);
const PREVIEW_PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4183);
const APP_DIR = resolve(HERE, process.env.GAWK_E2E_APP_DIR ?? '../gawk-app');
const SERVER_BIN = resolve(HERE, process.env.GAWK_E2E_SERVER_BIN ?? 'bin/gawk-server');
const RELAY_BIN = resolve(HERE, process.env.GAWK_RELAY_BIN ?? 'bin/gawk-relay');
const CONSOLE_PORT = Number(process.env.GAWK_E2E_CONSOLE_PORT ?? 4481);
const PUBSIM_BIN = resolve(HERE, process.env.GAWK_E2E_PUBSIM_BIN ?? 'bin/gawk-pubsim');
const APP_URL = `http://127.0.0.1:${PREVIEW_PORT}`;
const WATCHDOG_MS = 180_000;

const children = [];

function log(msg) {
  console.log(`[pop-pinned] ${msg}`);
}

function fail(msg) {
  throw new Error(msg);
}

// The private-literal rule the app applies (gawk-app/src/routing.ts), IPv4
// half: the relay must be named by such an address for the pin to be kept.
function isPrivateV4(a) {
  const m = /^(\d+)\.(\d+)\.\d+\.\d+$/.exec(a);
  if (!m) return false;
  const [x, y] = [Number(m[1]), Number(m[2])];
  return x === 10 || (x === 172 && y >= 16 && y <= 31) || (x === 192 && y === 168);
}

function lanAddress() {
  if (process.env.GAWK_E2E_LAN_IP) return process.env.GAWK_E2E_LAN_IP;
  for (const addrs of Object.values(networkInterfaces())) {
    for (const a of addrs ?? []) {
      if (a.family === 'IPv4' && !a.internal && isPrivateV4(a.address)) return a.address;
    }
  }
  return null;
}

function chromePath() {
  const candidates = process.env.GAWK_E2E_CHROME
    ? [process.env.GAWK_E2E_CHROME]
    : ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser'];
  for (const c of candidates) if (existsSync(c)) return c;
  fail(`no Chrome found (tried ${candidates.join(', ')}); set GAWK_E2E_CHROME`);
}

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
  });
  children.push(entry);
  return entry;
}

async function waitForLine(entry, regex, timeoutMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const m = entry.buf.match(regex);
    if (m) return m[1];
    if (entry.exited) fail(`${entry.name} exited before ${what} (see ${OUT}/${entry.name}.log)`);
    await sleep(100);
  }
  fail(`timeout waiting for ${what} from ${entry.name}`);
}

async function waitForHttp(url, timeoutMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
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
  for (const { child, exited } of children) if (!exited) child.kill('SIGTERM');
}

function rowValue(page, label) {
  return page
    .locator('dt')
    .filter({ hasText: new RegExp(`^${label}$`) })
    .locator('xpath=following-sibling::dd')
    .textContent();
}

async function captureDiagnostics(page, name) {
  const before = await page.evaluate(() => window.__gawkClipboard.length);
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

// Grants Local Network Access to the app origin. Playwright's
// 'local-network-access' maps to every CDP name this Chrome knows in ONE
// Browser.grantPermissions call; granting the names one call at a time does
// not work, because each call replaces the origin's granted set. Returns
// false when this Chrome has no such permission (it then does not gate the
// dial, and the denied control below is skipped).
async function grantLocalNetwork(context) {
  try {
    await context.grantPermissions(['local-network-access'], { origin: APP_URL });
    return true;
  } catch (e) {
    log(`no Local Network Access permission in this Chrome: ${e.message.split('\n')[0]}`);
    return false;
  }
}

async function newContext(browser) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  await context.addInitScript(() => {
    // No relayUrl: the deployment default stays https://localhost:4433, so
    // only the link can name the LAN relay.
    window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e' };
    localStorage.setItem('gawk:terms-accepted', 'e2e');
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
  });
  return context;
}

function viewLink({ relayUrl, certHash, id }) {
  return `${APP_URL}/#/view/${id}?relay=${encodeURIComponent(relayUrl)}&certHash=${certHash}`;
}

// The control: the same link with no grant must be refused by the browser's
// Local Network Access check, so the granted pass is shown to depend on the
// grant rather than on an ungated path. Headless Chrome answers the
// permission prompt with a denial.
async function deniedPass(browser, args) {
  const context = await newContext(browser);
  const page = await context.newPage();
  const seen = [];
  page.on('console', (m) => seen.push(m.text()));
  await page.goto(viewLink(args));
  await pollFor(
    () => seen.some((t) => t.includes('ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS')),
    20_000,
    250,
    'the Local Network Access refusal without a grant',
  );
  log('denied control ok: without the grant the dial is blocked by Local Network Access');
  await context.close();
}

async function viewPass(browser, { relayUrl, certHash, id }) {
  const context = await newContext(browser);
  const granted = await grantLocalNetwork(context);
  const page = await context.newPage();
  page.on('console', (m) => appendFileSync(join(OUT, 'console-view.log'), `[${m.type()}] ${m.text()}\n`));
  const link = viewLink({ relayUrl, certHash, id });
  log(`opening ${link}`);
  await page.goto(link);

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
  await pollFor(
    async () => {
      const v = (await rowValue(page, 'Completed'))?.trim();
      return /^\d+$/.test(v) && Number(v) > 0;
    },
    30_000,
    1000,
    'the first completed frame over the pinned LAN relay',
  );
  await sleep(2000);
  const d1 = await captureDiagnostics(page, 'diagnostics-1');
  await sleep(5000);
  const d2 = await captureDiagnostics(page, 'diagnostics-2');
  const s1 = d1.samples.at(-1).stats;
  const s2 = d2.samples.at(-1).stats;
  const completed = s2.framesCompleted - s1.framesCompleted;
  const decoded = s2.decoderFps;
  if (!(completed > 0)) fail(`no frames completed between captures (${s1.framesCompleted} → ${s2.framesCompleted})`);
  if (!(decoded > 0)) fail(`decoder fps ${decoded}, want > 0`);

  // The session really is on the LAN relay, pinned: the store resolved the
  // link's relay with its hash.
  const resolved = await page.evaluate(() => document.body.innerText.includes('Using server from link'));
  if (!resolved) fail('the in-session indicator does not say the server came from the link');
  log(`view pass ok: ${completed} frames completed between captures, decoder ${decoded} fps`);
  await context.close();
  return granted;
}

async function landingPass(browser, { relayUrl, certHash }) {
  const context = await newContext(browser);
  const page = await context.newPage();
  await page.goto(`${APP_URL}/#/?relay=${encodeURIComponent(relayUrl)}&certHash=${certHash}`);
  await page.getByRole('button', { name: 'Save and use this server' }).click();
  const stored = await page.evaluate(() => ({
    servers: JSON.parse(localStorage.getItem('gawk.servers') ?? '[]'),
    selected: localStorage.getItem('gawk.selectedServer'),
  }));
  const entry = stored.servers.find((s) => s.url === relayUrl);
  if (!entry) fail(`no saved entry for ${relayUrl}: ${JSON.stringify(stored)}`);
  const want = certHash.split(',');
  if (JSON.stringify(entry.certHashes) !== JSON.stringify(want) || entry.certHashHex !== want[0]) {
    fail(`saved entry's hashes are ${JSON.stringify(entry)}, want ${JSON.stringify(want)}`);
  }
  if (stored.selected !== entry.id) fail(`selected ${stored.selected}, want ${entry.id}`);
  log('landing pass ok: the relay was saved with its pin and selected');
  await context.close();
}

// The join link gawk-relay's console publishes (the QR's), as a guest gets
// it: its relay and its comma-separated pins, which must be the serving and
// the next rolling certificate's.
async function consoleJoinLink() {
  let join = '';
  await pollFor(
    async () => {
      try {
        const r = await fetch(`http://127.0.0.1:${CONSOLE_PORT}/api/state`);
        join = r.ok ? ((await r.json()).join?.url ?? '') : '';
      } catch {
        join = '';
      }
      return join !== '';
    },
    15_000,
    250,
    'the console join link',
  );
  const q = new URLSearchParams(new URL(join).hash.replace(/^#\/\?/, ''));
  const relayUrl = q.get('relay');
  const certHash = q.get('certHash') ?? '';
  if (!relayUrl || !/^[0-9a-f]{64},[0-9a-f]{64}$/.test(certHash)) {
    fail(`the console's join link ${join} does not name a relay with two pins`);
  }
  log(`console join link: ${join}`);
  return { relayUrl, certHash };
}

async function main() {
  mkdirSync(OUT, { recursive: true });
  const ip = lanAddress();
  if (ip === null) {
    const msg = 'no private-range IPv4 on a non-internal interface; this row needs a runner with a container or VM address (or set GAWK_E2E_LAN_IP)';
    if (ALLOW_SKIP) {
      log(`SKIP: ${msg}`);
      return;
    }
    fail(msg);
  }
  if (!isPrivateV4(ip)) fail(`GAWK_E2E_LAN_IP=${ip} is not a private-range IPv4; the app would drop the pin`);
  const useRelay = existsSync(RELAY_BIN);
  for (const bin of [useRelay ? RELAY_BIN : SERVER_BIN, PUBSIM_BIN]) {
    if (!existsSync(bin)) fail(`${bin} not found; build it from gawk-server (see e2e/README.md)`);
  }
  if (!existsSync(join(APP_DIR, 'dist', 'index.html'))) fail(`${APP_DIR}/dist missing; run npm run build there`);

  let relayUrl = `https://${ip}:${RELAY_PORT}`;
  let certHash;
  if (useRelay) {
    log(`gawk-relay (${RELAY_BIN}) at ${relayUrl}`);
    const stateDir = join(OUT, 'relay-state');
    rmSync(stateDir, { recursive: true, force: true });
    launch('relay', RELAY_BIN, [
      '-state-dir', stateDir,
      '-addr', `${ip}:${RELAY_PORT}`,
      '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
      '-console-addr', `127.0.0.1:${CONSOLE_PORT}`,
      '-ui-addr', '127.0.0.1:0',
      '-lan-address', ip,
    ]);
    await waitForHttp(`http://127.0.0.1:${OPS_PORT}/healthz`, 15_000, 'the relay ops endpoint');
    ({ relayUrl, certHash } = await consoleJoinLink());
  } else {
    log(`no gawk-relay build; gawk-server -dev-cert stands in at ${relayUrl}`);
    const relay = launch('relay', SERVER_BIN, [
      '-dev-cert',
      '-addr', `${ip}:${RELAY_PORT}`,
      '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
    ]);
    certHash = await waitForLine(relay, /cert_hash_hex\W*"?([0-9a-f]{64})/, 15_000, 'the dev cert hash');
    await waitForHttp(`http://127.0.0.1:${OPS_PORT}/healthz`, 15_000, 'the relay ops endpoint');
  }

  const pubsim = launch('pubsim', PUBSIM_BIN, ['-url', relayUrl, '-insecure', '-duration', '300s']);
  const id = await waitForLine(pubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the broadcast code');
  log(`pubsim publishing as ${id}`);

  launch('preview', join(APP_DIR, 'node_modules', '.bin', 'vite'), [
    'preview', '--port', String(PREVIEW_PORT), '--strictPort', '--host', '127.0.0.1',
  ], { cwd: APP_DIR });
  await waitForHttp(APP_URL, 20_000, 'vite preview');

  const browser = await chromium.launch({
    executablePath: chromePath(),
    headless: true,
    args: ['--no-sandbox', `--ip-address-space-overrides=127.0.0.1:${PREVIEW_PORT}=public`],
  });
  try {
    log(`browser: ${browser.version()}`);
    const granted = await viewPass(browser, { relayUrl, certHash, id });
    if (granted) await deniedPass(browser, { relayUrl, certHash, id });
    await landingPass(browser, { relayUrl, certHash });
  } finally {
    await browser.close();
  }
}

const watchdog = setTimeout(() => {
  console.error(`[pop-pinned] watchdog: run exceeded ${WATCHDOG_MS / 1000}s`);
  cleanup();
  process.exit(1);
}, WATCHDOG_MS);

main()
  .then(() => {
    log('PASS');
    cleanup();
    clearTimeout(watchdog);
  })
  .catch((e) => {
    console.error(`[pop-pinned] FAIL: ${e.message}`);
    cleanup();
    clearTimeout(watchdog);
    process.exitCode = 1;
  });
