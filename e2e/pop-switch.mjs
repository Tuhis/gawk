#!/usr/bin/env node
// R74 PP14 tier-1 (docs/76 item 43, §11): a party guest's browser follows
// home's LocalRelayOffer (0x19) to the PoP on its LAN.
//
// It owns its stack, like pop-pinned.mjs:
//   - home: gawk-server with -pops-file naming one PoP by a static
//     credential, on loopback, with a short-lived self-signed certificate
//     this script mints (the PoP verifies home through SSL_CERT_FILE, the
//     browser through the app's pinned hash);
//   - the PoP: gawk-relay, linked to home with -pair gawk-cred:…, serving
//     on the host's private address at :4433 (the port an offer names);
//   - gawk-pubsim publishing to HOME, so a viewer on the PoP is served by
//     the PoP's downlink (docs/76 §6.6);
//   - `vite preview` of the built app.
//
// The viewer dials home from loopback, and the PoP linked from loopback
// too, so home sees one public address for both and sends the offer. The
// launch flag --ip-address-space-overrides marks the app's origin AND
// home's port public, so the dial to the PoP's private address is a
// public → private request gated by Local Network Access, as it is from
// https://gawk.ioio.fi, while home stays reachable without the permission.
//
// Passes:
//   1. granted (CDP, as pop-pinned.mjs grants it): the viewer switches by
//      itself, the chip names the PoP, frames are drawn from the PoP, and
//      the longest gap between drawn frames once drawing started is at most
//      1 s (docs/76 item 43.3, "one freeze of at most about 1 s");
//   2. denied (CDP Browser.setPermission): the viewer stays on home, frames
//      keep flowing, and the denied note shows (item 43.4).
// What a person sees at the headful prompt is a PP0 measurement, not this
// test's claim.
//
// Environment: GAWK_E2E_SERVER_BIN, GAWK_RELAY_BIN (default
// e2e/bin/gawk-relay), GAWK_E2E_PUBSIM_BIN, GAWK_E2E_CHROME,
// GAWK_E2E_APP_DIR, GAWK_E2E_PREVIEW_PORT, GAWK_E2E_RELAY_PORT (home),
// GAWK_E2E_OPS_PORT, GAWK_E2E_LAN_IP, GAWK_E2E_GO (the go binary that mints
// the certificate; default `go`). With no private address the run fails,
// unless --allow-skip is given (as pop-pinned.mjs).

import { spawn, spawnSync } from 'node:child_process';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { networkInterfaces } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out-pop-switch');
const ALLOW_SKIP = process.argv.includes('--allow-skip');
const HOME_PORT = Number(process.env.GAWK_E2E_RELAY_PORT ?? 4443);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2122);
const POP_OPS_PORT = OPS_PORT + 1;
const POP_PORT = 4433; // the port an offer's IP-literal candidate names
const PREVIEW_PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4183);
const APP_DIR = resolve(HERE, process.env.GAWK_E2E_APP_DIR ?? '../gawk-app');
const SERVER_BIN = resolve(HERE, process.env.GAWK_E2E_SERVER_BIN ?? 'bin/gawk-server');
const RELAY_BIN = resolve(HERE, process.env.GAWK_RELAY_BIN ?? 'bin/gawk-relay');
const PUBSIM_BIN = resolve(HERE, process.env.GAWK_E2E_PUBSIM_BIN ?? 'bin/gawk-pubsim');
const GO = process.env.GAWK_E2E_GO ?? 'go';
const APP_URL = `http://127.0.0.1:${PREVIEW_PORT}`;
const HOME_URL = `https://127.0.0.1:${HOME_PORT}`;
const POP_NAME = 'assembly';
const POP_DISPLAY = 'Assembly 2026';
const MAX_FREEZE_MS = 1000;
const WATCHDOG_MS = 240_000;

const children = [];

function log(msg) {
  console.log(`[pop-switch] ${msg}`);
}

function fail(msg) {
  throw new Error(msg);
}

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
    if (m) return m[1] ?? m[0];
    if (entry.exited) fail(`${entry.name} exited before ${what} (see ${OUT}/${entry.name}.log)`);
    await sleep(100);
  }
  fail(`timeout waiting for ${what} from ${entry.name} (see ${OUT}/${entry.name}.log)`);
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

// Home's certificate: ECDSA P-256, 10 days (inside WebTransport's 14-day
// limit for serverCertificateHashes), for 127.0.0.1 and localhost. Its own
// issuer, so the PoP can trust it as a root through SSL_CERT_FILE. Minted
// by a throwaway Go program (stdlib only): Node has no certificate
// generator, and Go is already what builds the binaries under test.
const CERTGEN = `package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"time"
)

func main() {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "gawk e2e home"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	must(os.WriteFile(os.Args[1], pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	must(os.WriteFile(os.Args[2], pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
`;

function mintHomeCert() {
  const dir = join(OUT, 'certgen');
  mkdirSync(dir, { recursive: true });
  const src = join(dir, 'main.go');
  writeFileSync(src, CERTGEN);
  const certPath = join(OUT, 'home.crt');
  const keyPath = join(OUT, 'home.key');
  const r = spawnSync(GO, ['run', src, certPath, keyPath], {
    cwd: dir,
    encoding: 'utf8',
    env: { ...process.env, GO111MODULE: 'off', GOFLAGS: '' },
  });
  if (r.status !== 0) fail(`minting home's certificate failed: ${r.error?.message ?? ''}${r.stderr ?? ''}`);
  const pem = readFileSync(certPath, 'utf8');
  const der = Buffer.from(pem.replace(/-----[^-]+-----/g, '').replace(/\s+/g, ''), 'base64');
  return { certPath, keyPath, hash: createHash('sha256').update(der).digest('hex') };
}

// The page sees what the viewer draws: every VideoFrame handed to a 2D or
// WebGL context, on the page and in every worker (the viewer renders to an
// OffscreenCanvas in its worker), stamped with Date.now() so the clocks of
// the page and the workers agree.
const DRAW_HOOK = `(() => {
  if (self.__gawkDraws) return;
  self.__gawkDraws = [];
  const isFrame = (v) => typeof VideoFrame !== 'undefined' && v instanceof VideoFrame;
  const wrap = (proto, name, argIndex) => {
    if (!proto || typeof proto[name] !== 'function') return;
    const orig = proto[name];
    proto[name] = function (...args) {
      if (isFrame(args[argIndex])) self.__gawkDraws.push(Date.now());
      return orig.apply(this, args);
    };
  };
  for (const C of [self.CanvasRenderingContext2D, self.OffscreenCanvasRenderingContext2D]) {
    if (C) wrap(C.prototype, 'drawImage', 0);
  }
  for (const C of [self.WebGLRenderingContext, self.WebGL2RenderingContext]) {
    if (C) {
      wrap(C.prototype, 'texImage2D', 5);
      wrap(C.prototype, 'texSubImage2D', 6);
    }
  }
})();`;

async function newContext(browser) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  await context.addInitScript(
    ({ homeUrl, hash, hook }) => {
      // Home is the deployment default, pinned the way run.mjs pins it.
      localStorage.setItem('gawk.serverUrl', homeUrl);
      localStorage.setItem('gawk.certHashHex', hash);
      window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e', relayUrl: homeUrl };
      localStorage.setItem('gawk:terms-accepted', 'e2e');
      (0, eval)(hook);
    },
    { homeUrl: HOME_URL, hash: homeHash, hook: DRAW_HOOK },
  );
  return context;
}

// Workers get the hook as they appear; one that started before its hook
// only reports its frames late, which can make a freeze look longer, never
// shorter.
function hookWorkers(page) {
  const workers = [];
  page.on('worker', (w) => {
    workers.push(w);
    w.evaluate(DRAW_HOOK).catch(() => {});
  });
  return async () => {
    const all = [...(await page.evaluate(() => self.__gawkDraws ?? []))];
    for (const w of workers) {
      try {
        all.push(...(await w.evaluate(() => self.__gawkDraws ?? [])));
      } catch {
        // a worker that is gone took its frames with it; the gap shows
      }
    }
    return all.sort((a, b) => a - b);
  };
}

function longestGap(draws) {
  let gap = 0;
  for (let i = 1; i < draws.length; i++) gap = Math.max(gap, draws[i] - draws[i - 1]);
  return gap;
}

async function setLocalNetwork(context, page, setting) {
  if (setting === 'granted') {
    try {
      await context.grantPermissions(['local-network-access'], { origin: APP_URL });
      return true;
    } catch (e) {
      log(`no Local Network Access permission in this Chrome: ${e.message.split('\n')[0]}`);
      return false;
    }
  }
  const cdp = await context.newCDPSession(page);
  let any = false;
  for (const name of ['local-network-access', 'local-network', 'loopback-network']) {
    try {
      await cdp.send('Browser.setPermission', { permission: { name }, setting, origin: APP_URL });
      any = true;
    } catch {
      // not a name this Chrome knows
    }
  }
  return any;
}

let homeHash = '';

async function grantedPass(browser, id, pop) {
  const context = await newContext(browser);
  const page = await context.newPage();
  page.on('console', (m) => appendFileSync(join(OUT, 'console-granted.log'), `[${m.type()}] ${m.text()}\n`));
  if (!(await setLocalNetwork(context, page, 'granted'))) fail('cannot grant Local Network Access in this Chrome');
  const draws = hookWorkers(page);
  await page.goto(`${APP_URL}/#/view/${id}`);

  // The switch: the chip names the PoP once the offer was followed.
  const chip = page.getByTestId('local-relay-chip');
  await pollFor(() => chip.isVisible(), 20_000, 200, 'the local relay chip (the viewer following the offer)');
  const chipText = (await chip.textContent())?.trim() ?? '';
  if (!chipText.includes(POP_DISPLAY)) fail(`the chip says "${chipText}", want the PoP's name ${POP_DISPLAY}`);
  const switchedAt = Date.now();
  log(`switched: chip "${chipText}"`);

  // Frames from the PoP, then a window long enough to see the whole freeze.
  await pollFor(async () => (await draws()).some((t) => t > switchedAt), 20_000, 200, 'frames drawn after the switch');
  await sleep(5000);
  const all = await draws();
  writeFileSync(join(OUT, 'draws-granted.json'), JSON.stringify({ switchedAt, draws: all }));
  const before = all.filter((t) => t <= switchedAt).length;
  const after = all.filter((t) => t > switchedAt).length;
  const freeze = longestGap(all);
  log(`frames drawn: ${before} before the switch, ${after} after; longest gap ${freeze} ms`);
  if (after < 10) fail(`only ${after} frames drawn on the PoP`);
  // The frames really came through the PoP: it served the viewer's
  // /subscribe and pulled the broadcast from home over its downlink.
  const subscribeLine = new RegExp(`subscriber session started.*broadcast_id=${id}`);
  if (!subscribeLine.test(pop.buf)) fail('the PoP served no /subscribe for the broadcast');
  if (!new RegExp(`component=pop-downlink.*broadcast_id=${id}|broadcast_id=${id}.*component=pop-downlink`).test(pop.buf)) {
    fail('the PoP did not pull the broadcast from home');
  }
  if (freeze > MAX_FREEZE_MS) fail(`a ${freeze} ms freeze, want at most ${MAX_FREEZE_MS} ms`);
  await context.close();
  return { before, after, freeze };
}

async function deniedPass(browser, id, pop) {
  const context = await newContext(browser);
  const page = await context.newPage();
  page.on('console', (m) => appendFileSync(join(OUT, 'console-denied.log'), `[${m.type()}] ${m.text()}\n`));
  if (!(await setLocalNetwork(context, page, 'denied'))) fail('cannot deny Local Network Access in this Chrome');
  const draws = hookWorkers(page);
  const popLogAt = pop.buf.length;
  await page.goto(`${APP_URL}/#/view/${id}`);
  await pollFor(async () => (await draws()).length > 30, 20_000, 200, 'frames drawn from home');
  await pollFor(
    () => page.evaluate(() => document.body.innerText.includes('Your browser blocked the local relay')),
    20_000,
    200,
    'the denied note',
  );
  const n1 = (await draws()).length;
  await sleep(3000);
  const n2 = (await draws()).length;
  if (await page.getByTestId('local-relay-chip').isVisible()) fail('the denied viewer switched to the PoP');
  if (/(session|subscriber session) started.*route=(echo|subscribe)/.test(pop.buf.slice(popLogAt))) fail('the denied viewer reached the PoP');
  if (!(n2 > n1)) fail(`frames stopped on home with the permission denied (${n1} → ${n2})`);
  log(`denied pass ok: stayed on home, ${n2 - n1} frames in 3 s, the denied note shown`);
  await context.close();
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
  if (!isPrivateV4(ip)) fail(`GAWK_E2E_LAN_IP=${ip} is not a private-range IPv4`);
  for (const bin of [SERVER_BIN, RELAY_BIN, PUBSIM_BIN]) {
    if (!existsSync(bin)) fail(`${bin} not found; build it from gawk-server (see e2e/README.md)`);
  }
  if (!existsSync(join(APP_DIR, 'dist', 'index.html'))) fail(`${APP_DIR}/dist missing; run npm run build there`);

  const cert = mintHomeCert();
  homeHash = cert.hash;
  const credential = randomBytes(32).toString('hex');
  const popsFile = join(OUT, 'pops.yaml');
  writeFileSync(
    popsFile,
    [
      `- name: ${POP_NAME}`,
      `  uid: ${randomUUID()}`,
      `  displayName: ${POP_DISPLAY}`,
      `  credentialSHA256: ${createHash('sha256').update(credential).digest('hex')}`,
      '  guests:',
      '    autoSwitch: true',
      '',
    ].join('\n'),
  );

  const home = launch('home', SERVER_BIN, [
    '-cert-file', cert.certPath,
    '-key-file', cert.keyPath,
    '-addr', `127.0.0.1:${HOME_PORT}`,
    '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
    '-pops',
    '-pops-file', popsFile,
  ]);
  await waitForHttp(`http://127.0.0.1:${OPS_PORT}/healthz`, 15_000, 'home ops endpoint');

  const stateDir = join(OUT, 'pop-state');
  rmSync(stateDir, { recursive: true, force: true });
  const pop = launch(
    'pop',
    RELAY_BIN,
    [
      '-state-dir', stateDir,
      '-addr', `:${POP_PORT}`,
      '-metrics-addr', `127.0.0.1:${POP_OPS_PORT}`,
      '-console-addr', '127.0.0.1:0',
      '-ui-addr', '127.0.0.1:0',
      '-lan-address', ip,
      '-pair', `gawk-cred:127.0.0.1:${HOME_PORT}/${credential}`,
    ],
    { env: { ...process.env, SSL_CERT_FILE: cert.certPath } },
  );
  await waitForLine(pop, /linked to home/, 30_000, 'the link to home');
  log(`PoP linked to home; it serves https://${ip}:${POP_PORT}`);

  const pubsim = launch('pubsim', PUBSIM_BIN, ['-url', HOME_URL, '-insecure', '-duration', '300s']);
  const id = await waitForLine(pubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the broadcast code');
  log(`pubsim publishing to home as ${id}`);

  launch('preview', join(APP_DIR, 'node_modules', '.bin', 'vite'), [
    'preview', '--port', String(PREVIEW_PORT), '--strictPort', '--host', '127.0.0.1',
  ], { cwd: APP_DIR });
  await waitForHttp(APP_URL, 20_000, 'vite preview');

  const browser = await chromium.launch({
    executablePath: chromePath(),
    headless: true,
    args: [
      '--no-sandbox',
      `--ip-address-space-overrides=127.0.0.1:${PREVIEW_PORT}=public,127.0.0.1:${HOME_PORT}=public`,
    ],
  });
  try {
    log(`browser: ${browser.version()}`);
    const g = await grantedPass(browser, id, pop);
    log(`granted pass ok: one switch, longest gap between drawn frames ${g.freeze} ms (${g.before} frames on home first)`);
    await deniedPass(browser, id, pop);
  } finally {
    await browser.close();
  }
}

const watchdog = setTimeout(() => {
  console.error(`[pop-switch] watchdog: run exceeded ${WATCHDOG_MS / 1000}s`);
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
    console.error(`[pop-switch] FAIL: ${e.message}`);
    cleanup();
    clearTimeout(watchdog);
    process.exitCode = 1;
  });
