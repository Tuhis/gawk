#!/usr/bin/env node
// R74 PP8 tier-1 (docs/76 items 27-28, §11): the fourth outside stream of a
// PoP whose uplink budget forwards three is refused, and Chrome reads 4012
// PopUplinkFull.
//
// It owns its stack: a single-pod home gawk-server with -pops and a
// -pops-file naming one PoP with a static credential; a gawk-relay (the
// PoP) linked to it with that credential (`-pair gawk-cred:…`); four
// gawk-pubsim broadcasters publishing on the PoP; and a blank page served
// from loopback, in which headless Chrome dials home directly with
// WebTransport, pinned to home's certificate hash. Each broadcast is
// reserved at home before the PoP announces it (item 14), so the page can
// subscribe to it at home by its code.
//
// Passes:
//   1. three outside viewers, one per broadcast, each receive a keyframe
//      stream: the PoP admitted three wants (the default budget, OD13: three
//      streams) and dialled three uplink legs;
//   2. the fourth broadcast's outside viewer is closed with 4012: the page
//      reads the in-band SessionClosing (0x17) carrying 4012, which is what
//      Chrome acts on, since the WebTransport close code itself rarely
//      reaches it (docs/59), and records whatever `closed` reports beside it.
//
// The viewer's copy and its 20 s retry are the app's (W3-switch-web); this
// test pins only that the code reaches the browser.
//
// Environment: GAWK_E2E_SERVER_BIN, GAWK_E2E_RELAY_BIN, GAWK_E2E_PUBSIM_BIN,
// GAWK_E2E_DEVCERT_BIN (default bin/<name>), GAWK_E2E_CHROME (else the
// usual system paths), GAWK_E2E_HOME_PORT, GAWK_E2E_POP_PORT,
// GAWK_E2E_PAGE_PORT, GAWK_E2E_OPS_PORT.

import { spawn } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = resolve(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out-pop-uplink-full');
const HOME_PORT = Number(process.env.GAWK_E2E_HOME_PORT ?? 4453);
const POP_PORT = Number(process.env.GAWK_E2E_POP_PORT ?? 4463);
const PAGE_PORT = Number(process.env.GAWK_E2E_PAGE_PORT ?? 4193);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2132);
const bin = (env, name) => resolve(HERE, process.env[env] ?? `bin/${name}`);
const SERVER_BIN = bin('GAWK_E2E_SERVER_BIN', 'gawk-server');
const RELAY_BIN = bin('GAWK_E2E_RELAY_BIN', 'gawk-relay');
const PUBSIM_BIN = bin('GAWK_E2E_PUBSIM_BIN', 'gawk-pubsim');
const DEVCERT_BIN = bin('GAWK_E2E_DEVCERT_BIN', 'gawk-devcert');
const PAGE_URL = `http://127.0.0.1:${PAGE_PORT}/`;
const HOME_URL = `https://127.0.0.1:${HOME_PORT}`;
const POP_NAME = 'e2e-party';
const STREAMS = 3; // pops.DefaultUplinkStreams (OD13)
const WATCHDOG_MS = 180_000;

const children = [];

function log(msg) {
  console.log(`[pop-uplink-full] ${msg}`);
}

function fail(msg) {
  throw new Error(msg);
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

async function run(name, cmd, args) {
  const e = launch(name, cmd, args);
  while (!e.exited) await sleep(50);
  if (e.exited.code !== 0) fail(`${name} exited ${e.exited.code} (see ${OUT}/${name}.log)`);
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

function cleanup() {
  for (const { child, exited } of children) if (!exited) child.kill('SIGTERM');
}

// The SHA-256 of a PEM certificate's DER, as serverCertificateHashes wants.
function certHash(pemPath) {
  const pem = readFileSync(pemPath, 'utf8');
  const b64 = pem.replace(/-----[^-]+-----/g, '').replace(/\s+/g, '');
  return createHash('sha256').update(Buffer.from(b64, 'base64')).digest('hex');
}

// Runs in the page: subscribe to one broadcast at home and report what
// happened — a keyframe stream (admitted, the leg is up), or the close.
// window.__subs[id] keeps the session open for the rest of the run.
async function pageSubscribe({ url, hash, id, settleMs }) {
  const bytes = new Uint8Array(hash.match(/../g).map((h) => parseInt(h, 16)));
  const wt = new WebTransport(`${url}/subscribe/${id}`, {
    serverCertificateHashes: [{ algorithm: 'sha-256', value: bytes }],
  });
  window.__subs = window.__subs ?? {};
  window.__subs[id] = wt;
  const r = { id, keyframe: false, noticed: null, closed: null };
  await wt.ready;
  const closed = wt.closed.then(
    (info) => (r.closed = { closeCode: info?.closeCode ?? null, reason: info?.reason ?? '' }),
    (err) => (r.closed = { error: String(err) }),
  );
  // Server uni streams: a keyframe stream (a StreamFrame, type 0x04) or
  // the in-band SessionClosing (version, 0x17, u32 BE code). Others
  // (RelayCapabilities, the telemetry and identity notices) are skipped.
  const reader = wt.incomingUnidirectionalStreams.getReader();
  const streams = (async () => {
    for (;;) {
      const { value: st, done } = await reader.read();
      if (done) return;
      (async () => {
        const rd = st.getReader();
        const { value } = await rd.read();
        if (!value || value.length < 2) return;
        if (value[1] === 0x17 && value.length >= 6) {
          r.noticed = new DataView(value.buffer, value.byteOffset).getUint32(2);
        } else if (value[1] === 0x04) {
          r.keyframe = true;
        }
        rd.cancel().catch(() => {});
      })().catch(() => {});
    }
  })().catch(() => {});
  const deadline = Date.now() + settleMs;
  while (Date.now() < deadline && !r.keyframe && r.noticed === null && r.closed === null) {
    await new Promise((res) => setTimeout(res, 100));
  }
  if (r.noticed !== null || r.closed !== null) {
    await Promise.race([closed, new Promise((res) => setTimeout(res, 3000))]);
  }
  void streams;
  return r;
}

async function main() {
  mkdirSync(OUT, { recursive: true });
  for (const b of [SERVER_BIN, RELAY_BIN, PUBSIM_BIN, DEVCERT_BIN]) {
    if (!existsSync(b)) fail(`${b} not found; build it from gawk-server (see e2e/README.md)`);
  }

  // Home's certificate, on disk: the PoP trusts it through SSL_CERT_FILE
  // (its dialer uses the system roots), the page pins its hash.
  const certDir = join(OUT, 'home-cert');
  mkdirSync(certDir, { recursive: true });
  await run('devcert', DEVCERT_BIN, ['-out', certDir, '-hosts', 'localhost,127.0.0.1']);
  const homeHash = certHash(join(certDir, 'cert.pem'));

  const credential = `e2e-${randomUUID()}`;
  const popsFile = join(OUT, 'pops.json');
  writeFileSync(
    popsFile,
    JSON.stringify([
      {
        name: POP_NAME,
        uid: randomUUID(),
        displayName: 'E2E party',
        credentialSHA256: createHash('sha256').update(credential).digest('hex'),
      },
    ]),
  );

  const home = launch('home', SERVER_BIN, [
    '-addr', `127.0.0.1:${HOME_PORT}`,
    '-cert-file', join(certDir, 'cert.pem'),
    '-key-file', join(certDir, 'key.pem'),
    '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
    '-allowed-origins', `http://127.0.0.1:${PAGE_PORT}`,
    '-pops',
    '-pops-file', popsFile,
  ]);
  await waitForHttp(`http://127.0.0.1:${OPS_PORT}/healthz`, 15_000, 'the home ops endpoint');
  void home;

  const stateDir = join(OUT, 'pop-state');
  const pop = launch('pop', RELAY_BIN, [
    '-addr', `127.0.0.1:${POP_PORT}`,
    '-state-dir', stateDir,
    '-console-addr', '127.0.0.1:0',
    '-lan-address', '127.0.0.1',
    '-pair', `gawk-cred:127.0.0.1:${HOME_PORT}/${credential}`,
  ], { env: { ...process.env, SSL_CERT_FILE: join(certDir, 'cert.pem') } });
  await waitForLine(pop, /(linked to home)/, 30_000, 'the link to home');
  log('the PoP is linked to home');

  const ids = [];
  for (let i = 0; i < STREAMS + 1; i++) {
    const p = launch(`pubsim-${i}`, PUBSIM_BIN, ['-url', `https://127.0.0.1:${POP_PORT}`, '-insecure', '-duration', '300s']);
    ids.push(await waitForLine(p, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the broadcast code'));
  }
  log(`publishing on the PoP: ${ids.join(' ')}`);

  const page = createServer((_, res) => {
    res.writeHead(200, { 'content-type': 'text/html' });
    res.end('<!doctype html><title>pop-uplink-full</title>');
  });
  await new Promise((r) => page.listen(PAGE_PORT, '127.0.0.1', r));

  const browser = await chromium.launch({ executablePath: chromePath(), headless: true, args: ['--no-sandbox'] });
  try {
    log(`browser: ${browser.version()}`);
    const tab = await (await browser.newContext()).newPage();
    tab.on('console', (m) => appendFileSync(join(OUT, 'console.log'), `[${m.type()}] ${m.text()}\n`));
    await tab.goto(PAGE_URL);
    for (const id of ids.slice(0, STREAMS)) {
      const r = await tab.evaluate(pageSubscribe, { url: HOME_URL, hash: homeHash, id, settleMs: 20_000 });
      log(`viewer of ${id}: ${JSON.stringify(r)}`);
      if (!r.keyframe) fail(`the outside viewer of ${id} got no keyframe: ${JSON.stringify(r)}`);
    }
    const id = ids[STREAMS];
    const r = await tab.evaluate(pageSubscribe, { url: HOME_URL, hash: homeHash, id, settleMs: 20_000 });
    log(`viewer of ${id} (the fourth stream): ${JSON.stringify(r)}`);
    writeFileSync(join(OUT, 'fourth.json'), JSON.stringify(r, null, 2));
    if (r.keyframe) fail(`the fourth stream was forwarded: ${JSON.stringify(r)}`);
    if (r.noticed !== 4012) fail(`the fourth viewer read ${r.noticed} in-band, want 4012: ${JSON.stringify(r)}`);
    log(`fourth stream refused: Chrome read 4012 in-band (closed: ${JSON.stringify(r.closed)})`);
  } finally {
    await browser.close();
    page.close();
  }
}

const watchdog = setTimeout(() => {
  console.error(`[pop-uplink-full] watchdog: run exceeded ${WATCHDOG_MS / 1000}s`);
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
    console.error(`[pop-uplink-full] FAIL: ${e.message}`);
    cleanup();
    clearTimeout(watchdog);
    process.exitCode = 1;
  });
