#!/usr/bin/env node
// R79 MV7 (docs/80 §9): room voice end to end, with every real component.
// R85 MI4 (docs/89): in both roster modes, VOICE_ROSTER=ice (the default)
// or VOICE_ROSTER=observer.
// R88 PX4 (docs/92): with gawk-mmproxy beside murmur by default
// (VOICE_PROXY=off dials murmur directly). The UDP LoadBalancer stand-in
// gives each flow its own loopback address, so the bridges, and through the
// proxy murmur, see each browser at its own address.
//
//   murmur 1.6 (docker)  ←  two gawk-mumble-bridge pods  ←  the relay
//          ↑                 behind a UDP "LoadBalancer"      ↑
//   gawk-mumblesim                (e2e/udp-lb.mjs)          the web app in
//   (a native Mumble user)                                  headless Chromium
//
// Nothing is faked: the relay mints real tickets and runs the real observer
// and session links, the bridges speak real Mumble to a real murmur, and the
// browsers capture Chromium's fake microphone through the production
// pipeline. The native user is gawk-mumblesim, which echoes every voice
// packet it hears as its own voice and reports what it received per speaker
// once a second — so one echo proves both directions.
//
// The passes, in order (each one's assertion is in the log line it prints):
//
//   1. roster before voice  — a room viewer who never joins voice sees the
//                             native Mumble user in the room (OD28, D15)
//   2. both directions      — alice joins voice, bob listen-only; the native
//                             user receives alice's frames, and alice and bob
//                             both see the native user speaking (the echo)
//   3. Mumble moderation    — a SuperUser server-mutes alice (the panel says
//                             so) and kicks bob (his voice ends, terminal)
//   4. pod drain            — the bridge pod hosting alice drains: she is
//                             told 4002 while it is still serving, rejoins
//                             through the LoadBalancer onto the other pod,
//                             and the native user hears her again
//   5. temporary channel    — a dynamic room minted with ?voice=1 gets its
//                             channel under the parent; a native user joins
//                             it by gawk's fleet-stable name and is in the
//                             room's roster
//   9. a Mumble ban (R88)   — with the proxy on, murmur logs each browser
//                             at its own address; a SuperUser kickban of
//                             erin ends her voice (4015) while alice stays
//                             in voice and the native user still hears her
//   6. gawk ban             — an IP ban covering the browsers (127.0.0.0/8:
//                             each flow has its own loopback address) ends
//                             alice's voice (4015) on the relay's own ban
//                             event
//
// Ice mode (docs/89): the roster comes from murmur's Ice, so pass 1 also
// checks that no user named the observer name exists, pass 5 that the
// channel is created by the first gawk voice user (not before) and removed
// once everyone has left, and two passes are added, run before the ban:
//
//   7. Ice outage           — the Ice proxy is cut mid-call: alice and the
//                             native user still hear each other, the relay
//                             reports the observer unavailable, and once it
//                             is restored the roster is back, no bot seen
//   8. a pod SIGKILLed      — the pod watching the static room dies without
//                             cleaning up: the observer link moves, the
//                             native user's next mute reaches the roster,
//                             murmur stays up and drops the dead registration
//
// Run it (from the repository root):
//
//   mkdir -p e2e/bin
//   (cd gawk-server && go build -o ../e2e/bin/ ./cmd/gawk-server ./cmd/gawk-pubsim ./cmd/gawk-devcert)
//   (cd gawk-mumble-bridge && go build -o ../e2e/bin/ ./cmd/gawk-mumble-bridge ./cmd/gawk-mumblesim)
//   (cd gawk-app && npm ci && npm run build)
//   (cd e2e && npm ci)
//   node e2e/voice.mjs
//
// It needs docker (murmur) and writes logs and screenshots to
// e2e/out/voice/ (gitignored). Assertions are flow-shaped — frames arrive,
// states change — never latency numbers (docs/25 Decision 6).

import { spawn, spawnSync, execFileSync } from 'node:child_process';
import net from 'node:net';
import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';
import { UdpBalancer } from './udp-lb.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, '..');
const OUT = join(HERE, 'out', 'voice');
const BIN = join(HERE, 'bin');
const APP_DIR = join(REPO, 'gawk-app');

const RELAY_PORT = 4733;
const RELAY_OPS = 2712;
const LB_PUBLIC = 4743;
const LB_INTERNAL = 4744;
const PREVIEW_PORT = 4189;
const APP_ORIGIN = `http://127.0.0.1:${PREVIEW_PORT}`;
const APP_URL = `${APP_ORIGIN}/`;
const BRIDGES = [
  { name: 'bridge-a', pub: 5443, internal: 5444, ops: 5113 },
  { name: 'bridge-b', pub: 6443, internal: 6444, ops: 6113 },
];
const PSK = 'e2e-voice-psk-0123456789';
const SECRET = 'e2e-voice-ticket-secret-0123456789';
const TOKEN = 'gawke2e';
const ROOM = 'voicee2e';
const CHANNEL = 'E2E Voice';
const TEMP_PARENT = 'gawk rooms';
const MURMUR_BASE = 64760;
const MURMUR = `127.0.0.1:${MURMUR_BASE}`;
const MURMUR_SU = 'gawk-test-superuser';
const OBSERVER_NAME = 'gawkBridgeBot';
const ROSTER = process.env.VOICE_ROSTER ?? 'ice';
if (!['ice', 'observer'].includes(ROSTER)) throw new Error(`VOICE_ROSTER=${ROSTER}: want ice or observer`);
const ICE = ROSTER === 'ice';
const PROXY = (process.env.VOICE_PROXY ?? 'on') !== 'off';
const MMPROXY_PORT = 4746;
const MMPROXY_OPS = 4747;
const MMPROXY_CONTAINER = 'gawk-e2e-mmproxy';
// murmur.sh puts Ice at the base port + 3; the bridges reach it through a
// proxy the outage pass can cut.
const ICE_PROXY_PORT = 4745;
const CALLBACK_PORTS = { 'bridge-a': 5503, 'bridge-b': 6503 };

const log = (...a) => console.log('[voice]', ...a);
const children = [];
function fail(msg) {
  console.error(`[voice] FAIL: ${msg}`);
  process.exitCode = 1;
  throw new Error(msg);
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

async function pollFor(fn, timeoutMs, everyMs, what) {
  const deadline = Date.now() + timeoutMs;
  let last;
  while (Date.now() < deadline) {
    try {
      last = await fn();
      if (last) return last;
    } catch (e) {
      last = e;
    }
    await sleep(everyMs);
  }
  fail(`timeout waiting for ${what}${last instanceof Error ? ` (${last.message})` : ''}`);
}

async function waitForHttp(url, timeoutMs, what) {
  await pollFor(async () => (await fetch(url)).ok, timeoutMs, 200, what);
}

function bin(name) {
  const p = join(BIN, name);
  if (!existsSync(p)) fail(`${p} not found — see the build steps at the top of this file`);
  return p;
}

function chromePath() {
  const fixed = process.env.GAWK_E2E_CHROME ? [process.env.GAWK_E2E_CHROME] : ['/usr/bin/google-chrome', '/usr/bin/chromium'];
  for (const c of fixed) if (existsSync(c)) return c;
  const cache = join(homedir(), '.cache', 'ms-playwright');
  for (const d of readdirSync(cache).filter((n) => /^chromium-\d+$/.test(n)).sort().reverse()) {
    for (const sub of ['chrome-linux64/chrome', 'chrome-linux/chrome']) {
      const p = join(cache, d, sub);
      if (existsSync(p)) return p;
    }
  }
  fail('no Chromium found; set GAWK_E2E_CHROME');
}

// One native Mumble user. Its JSON report lines are parsed as they arrive;
// `latest()` is the last one.
function mumblesim(name, args) {
  const e = launch(name, bin('gawk-mumblesim'), [
    '-addr', MURMUR, '-insecure', '-name', name, ...args,
  ]);
  e.latest = () => {
    const lines = e.buf.split('\n').filter((l) => l.startsWith('{'));
    return lines.length ? JSON.parse(lines[lines.length - 1]) : null;
  };
  return e;
}

// A SuperUser action (kick, mute, create a channel, grant the ACL), run to
// completion.
//
// murmur 1.6's image loses MUMBLE_SUPERUSER_PASSWORD after its first start,
// and the script's re-set on the running server can lose its SQLite write
// to the server (CI, PR #595), so a WrongUserPW here sets it again and
// retries rather than failing the run on a harness race.
// murmurAddresses maps each user name to the address murmur logged for its
// connection (R88). Murmur logs a connection as session 0,
// "<0:(-1)> New connection: 127.0.0.3:41234", and assigns the session only
// at "<7:alice [web](-1)> Authenticated", so a login is paired with the
// latest connection line not yet paired. The latest login of a name wins.
function murmurAddresses() {
  const pending = [];
  const out = new Map();
  for (const line of murmurLog().split('\n')) {
    let m = line.match(/New connection: (\[[^\]]+\]|[0-9.]+):\d+/);
    if (m) pending.push(m[1].replace(/^\[|\]$/g, ''));
    m = line.match(/<\d+:(.+)\(-?\d+\)> Authenticated/);
    if (m && pending.length) out.set(m[1], pending.pop());
  }
  return out;
}

async function admin(name, args) {
  for (let attempt = 1; ; attempt++) {
    const run = attempt === 1 ? name : `${name}-${attempt}`;
    const e = mumblesim(run, ['-name', 'SuperUser', '-password', MURMUR_SU, '-duration', '3s', ...args]);
    await pollFor(() => e.exited, 15_000, 100, `${run} to finish`);
    if (e.exited.code === 0) return e;
    if (attempt >= 5 || !e.buf.includes('WrongUserPW')) fail(`${run} exited ${e.exited.code} (see out/voice/${run}.log)`);
    log(`${name}: SuperUser refused; setting its password again (attempt ${attempt})`);
    try {
      execFileSync('docker', ['exec', '-u', '10000', 'gawk-e2e-murmur-main', '/usr/bin/mumble-server',
        '--ini', '/data/mumble_server_config.ini', '--set-su-pw', MURMUR_SU]);
    } catch {
      // the next attempt reports it
    }
    await sleep(1000);
  }
}

// murmur's log: it writes to stderr, which docker logs replays as stderr.
function murmurLog() {
  const r = spawnSync('docker', ['logs', 'gawk-e2e-murmur-main'], { encoding: 'utf8', maxBuffer: 64 << 20 });
  return `${r.stdout ?? ''}${r.stderr ?? ''}`;
}

// The whole server as SuperUser sees it: every user's name and every
// channel's path (gawk-mumblesim's report).
let looks = 0;
async function serverView() {
  const e = await admin(`admin-look-${++looks}`, []);
  return e.latest();
}

// G1 (docs/89): with Ice configured, no Mumble user the bridge makes exists
// except voice users.
async function noObserverUser(when) {
  const v = await serverView();
  const bot = (v.allUsers ?? []).filter((n) => n.startsWith(OBSERVER_NAME));
  if (bot.length) fail(`Ice mode: ${bot.join(', ')} exists ${when} (docs/89 G1)`);
}

// A TCP proxy in front of murmur's Ice, for the outage pass.
function iceProxy(listen, target) {
  const p = { server: null, sockets: new Set() };
  p.start = () =>
    new Promise((resolve, reject) => {
      p.server = net.createServer((c) => {
        const u = net.connect(target, '127.0.0.1');
        p.sockets.add(c);
        p.sockets.add(u);
        const end = () => {
          c.destroy();
          u.destroy();
          p.sockets.delete(c);
          p.sockets.delete(u);
        };
        c.on('error', end).on('close', end);
        u.on('error', end).on('close', end);
        c.pipe(u);
        u.pipe(c);
      });
      p.server.once('error', reject);
      p.server.listen(listen, '127.0.0.1', resolve);
    });
  p.cut = () => {
    p.server?.close();
    p.server = null;
    for (const s of p.sockets) s.destroy();
    p.sockets.clear();
  };
  return p;
}

async function relayStatus() {
  return (await fetch(`http://127.0.0.1:${RELAY_OPS}/statusz`)).json();
}
async function bridgeStatus(b) {
  return (await fetch(`http://127.0.0.1:${b.ops}/statusz`)).json();
}
async function bridgeToBrowser(b) {
  const m = await (await fetch(`http://127.0.0.1:${b.ops}/metrics`)).text();
  const line = m.split('\n').find((l) => l.startsWith('gawk_bridge_frames_forwarded_total{direction="to_browser"}'));
  return line ? Number(line.split(' ')[1]) : 0;
}
async function bridgeSessions(b) {
  const m = await (await fetch(`http://127.0.0.1:${b.ops}/metrics`)).text();
  const line = m.split('\n').find((l) => l.startsWith('gawk_bridge_voice_sessions '));
  return line ? Number(line.split(' ')[1]) : 0;
}

// --- browsers -----------------------------------------------------------

async function newParticipant(browser, nick, certHash) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.grantPermissions(['microphone'], { origin: APP_ORIGIN });
  await context.addInitScript(
    ({ relay, hash, nick }) => {
      localStorage.setItem('gawk.serverUrl', relay);
      localStorage.setItem('gawk.certHashHex', hash);
      localStorage.setItem('gawk:terms-accepted', 'e2e');
      localStorage.setItem('gawk:nickname', nick);
      window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e', relayUrl: relay };
    },
    { relay: `https://127.0.0.1:${RELAY_PORT}`, hash: certHash, nick },
  );
  const page = await context.newPage();
  const consoleLog = join(OUT, `console-${nick}.log`);
  writeFileSync(consoleLog, '');
  page.on('console', (m) => appendFileSync(consoleLog, `[${m.type()}] ${m.text()}\n`));
  page.on('pageerror', (e) => appendFileSync(consoleLog, `[pageerror] ${e}\n`));
  return { nick, context, page };
}

async function openRoom(p, code) {
  await p.page.goto(`${APP_URL}#/room/${code}`);
  // R81 (docs/82 D1): voice has its own panel, opened from the header.
  await p.page.getByRole('button', { name: /^Voice(:|$)/ }).click();
  await p.page.getByTestId('voice-panel').waitFor({ timeout: 20_000 });
}

// The voice rows as the panel renders them.
async function voiceRows(p) {
  return p.page.evaluate(() =>
    [...document.querySelectorAll('[data-testid="voice-row"]')].map((li) => ({
      text: li.textContent ?? '',
      speaking: li.querySelector('[data-on]')?.getAttribute('data-on') === 'true',
    })),
  );
}

async function sectionText(p) {
  return p.page.getByTestId('voice-panel').innerText();
}

async function shot(p, name) {
  writeFileSync(join(OUT, `${name}.png`), await p.page.screenshot());
}

// --- main ---------------------------------------------------------------

async function main() {
  rmSync(OUT, { recursive: true, force: true });
  mkdirSync(OUT, { recursive: true });

  // Murmur 1.6, the operator prerequisites of docs/80 §5 (the bridge's own
  // test script: autoban off, a username rule that takes "{nick} [web]").
  log('starting murmur 1.6');
  const murmurEnv = execFileSync(join(REPO, 'gawk-mumble-bridge/testdata/murmur.sh'), ['up', '1.6'], {
    // GAWK_MURMUR_NO_PROXIED: murmur.sh's `proxied` server is for the Go
    // tests; this run puts its own gawk-mmproxy beside `main` (below).
    env: { ...process.env, GAWK_MURMUR_PREFIX: 'gawk-e2e-murmur', GAWK_MURMUR_BASE_PORT: String(MURMUR_BASE), GAWK_MURMUR_NO_PROXIED: '1' },
  }).toString();
  appendFileSync(join(OUT, 'murmur.env'), murmurEnv);
  const menv = Object.fromEntries([...murmurEnv.matchAll(/^export (\w+)=(.*)$/gm)].map((m) => [m[1], m[2]]));
  log(`roster mode: ${ROSTER}`);
  const proxy = iceProxy(ICE_PROXY_PORT, Number(menv.GAWK_TEST_MURMUR_ICE.split(':')[1]));
  if (ICE) await proxy.start();

  // R88: gawk-mmproxy beside murmur. `main` runs on the host's network, so
  // the proxy does too, in a container for its NET_RAW (IP_TRANSPARENT).
  // Every source here is a local 127.0.x.y, which needs no policy route:
  // -require-routes=false, and none is installed in this namespace.
  if (PROXY) {
    log('starting gawk-mmproxy beside murmur');
    spawnSync('docker', ['rm', '-f', MMPROXY_CONTAINER]);
    execFileSync('docker', ['create', '--name', MMPROXY_CONTAINER, '--network', 'host', '--user', '0',
      '--cap-drop', 'ALL', '--cap-add', 'NET_RAW', 'alpine:3.24', '/gawk-mmproxy', 'serve',
      '-listen', `127.0.0.1:${MMPROXY_PORT}`, '-ops-listen', `127.0.0.1:${MMPROXY_OPS}`,
      '-murmur-port', String(MURMUR_BASE), '-require-routes=false']);
    execFileSync('docker', ['cp', bin('gawk-mmproxy'), `${MMPROXY_CONTAINER}:/gawk-mmproxy`]);
    execFileSync('docker', ['start', MMPROXY_CONTAINER]);
    await waitForHttp(`http://127.0.0.1:${MMPROXY_OPS}/healthz`, 15_000, 'gawk-mmproxy');
  }

  // The static room's channel and the temporary-channel parent with its
  // MakeTempChannel grant for the bridge's access token (docs/80 §5, D10).
  await admin('admin-channel', ['-channel', CHANNEL]);
  await admin('admin-parent', ['-channel', TEMP_PARENT, '-token', TOKEN, '-grant-temp', TOKEN]);

  // One certificate for the relay, both bridges and the browsers' pins.
  const certDir = join(OUT, 'certs');
  execFileSync(bin('gawk-devcert'), ['-out', certDir, '-hosts', 'localhost,127.0.0.1']);
  const certPem = readFileSync(join(certDir, 'cert.pem'), 'utf8');
  const der = Buffer.from(certPem.replace(/-----[^-]+-----/g, '').replace(/\s+/g, ''), 'base64');
  const certHash = createHash('sha256').update(der).digest('hex');

  // The LoadBalancer in front of both bridge pods, public and internal.
  // distinctSources: each browser flow reaches a bridge from its own
  // 127.0.x.y, as a Local-policy LoadBalancer keeps each client's address.
  const lbPublic = new UdpBalancer({ listen: LB_PUBLIC, distinctSources: true, backends: BRIDGES.map((b) => ({ name: b.name, port: b.pub })) });
  const lbInternal = new UdpBalancer({ listen: LB_INTERNAL, backends: BRIDGES.map((b) => ({ name: b.name, port: b.internal })) });
  await lbPublic.start();
  await lbInternal.start();

  const roomsFile = join(OUT, 'rooms.json');
  writeFileSync(roomsFile, JSON.stringify([{ code: ROOM, displayName: 'Voice e2e', mumble: { channel: CHANNEL } }]));
  const bansFile = join(OUT, 'bans.json');
  writeFileSync(bansFile, '[]');

  const relay = launch('relay', bin('gawk-server'), [
    '-addr', `127.0.0.1:${RELAY_PORT}`,
    '-metrics-addr', `127.0.0.1:${RELAY_OPS}`,
    '-cert-file', join(certDir, 'cert.pem'),
    '-key-file', join(certDir, 'key.pem'),
    '-rooms', '-rooms-file', roomsFile,
    '-moderation-source', `file:${bansFile}`,
    '-voice-bridge-addr', `127.0.0.1:${LB_INTERNAL}`,
    '-voice-bridge-public-url', `https://127.0.0.1:${LB_PUBLIC}/voice`,
    '-voice-bridge-psk', PSK,
    '-voice-ticket-secret', SECRET,
    '-voice-bridge-ca-file', join(certDir, 'cert.pem'),
    '-conn-rate-limit', '100', '-conn-burst-limit', '400',
  ]);
  await waitForHttp(`http://127.0.0.1:${RELAY_OPS}/healthz`, 15_000, 'the relay');

  const startBridge = (b) =>
    launch(b.name, bin('gawk-mumble-bridge'), [
      '-listen', `127.0.0.1:${b.pub}`,
      '-internal-listen', `127.0.0.1:${b.internal}`,
      '-ops-listen', `127.0.0.1:${b.ops}`,
      '-cert-file', join(certDir, 'cert.pem'),
      '-key-file', join(certDir, 'key.pem'),
      '-allowed-origins', APP_ORIGIN,
      '-psk', PSK,
      '-ticket-secret', SECRET,
      '-relay-url', `https://127.0.0.1:${RELAY_PORT}`,
      '-relay-ca-file', join(certDir, 'cert.pem'),
      '-mumble-addr', MURMUR,
      '-mumble-insecure-skip-verify',
      '-mumble-access-token', TOKEN,
      '-mumble-temp-parent', TEMP_PARENT,
      '-drain-timeout', '5s',
      ...(PROXY ? ['-mumble-proxy-addr', `127.0.0.1:${MMPROXY_PORT}`] : []),
      ...(ICE
        ? [
            '-mumble-ice-addr', `127.0.0.1:${ICE_PROXY_PORT}`,
            '-mumble-ice-secret', menv.GAWK_TEST_MURMUR_ICE_SECRET,
            '-mumble-ice-callback-listen', `127.0.0.1:${CALLBACK_PORTS[b.name]}`,
            '-mumble-ice-callback-host', '127.0.0.1',
          ]
        : []),
    ]);
  const bridgeProcs = Object.fromEntries(BRIDGES.map((b) => [b.name, startBridge(b)]));
  for (const b of BRIDGES) await waitForHttp(`http://127.0.0.1:${b.ops}/readyz`, 15_000, b.name);

  const vite = join(APP_DIR, 'node_modules', '.bin', 'vite');
  launch('preview', vite, ['preview', '--port', String(PREVIEW_PORT), '--strictPort', '--host', '127.0.0.1'], { cwd: APP_DIR });
  await waitForHttp(APP_URL, 20_000, 'vite preview');

  // The native user, in the static room's channel, echoing.
  const native = mumblesim('native', ['-channel', CHANNEL, '-echo']);
  await pollFor(() => native.latest()?.session > 0, 15_000, 200, 'the native Mumble user to connect');

  // ---- 1. the roster before anyone is in voice -------------------------
  await pollFor(
    async () => {
      const st = await relayStatus();
      return Object.values(st.rooms ?? {}).some((r) => r.voiceLive && r.voiceObserver === 'available');
    },
    20_000,
    250,
    'the relay to report the static room voice-live with its observer in the channel',
  );
  const browser = await chromium.launch({
    executablePath: chromePath(),
    headless: true,
    args: ['--no-sandbox', '--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
  try {
    const viewer = await newParticipant(browser, 'carol', certHash);
    await openRoom(viewer, ROOM);
    await pollFor(
      async () => (await voiceRows(viewer)).some((r) => r.text.includes('native') && r.text.includes('Mumble')),
      15_000,
      250,
      'a room viewer outside voice to see the native Mumble user',
    );
    if (ICE) await noObserverUser('beside the static room’s roster');
    log(`1 ok: a viewer who never joined voice sees the native Mumble user in the room${ICE ? ', and no observer user exists' : ''}`);

    // ---- 2. both directions --------------------------------------------
    const alice = await newParticipant(browser, 'alice', certHash);
    const bob = await newParticipant(browser, 'bob', certHash);
    await openRoom(alice, ROOM);
    await openRoom(bob, ROOM);
    await alice.page.getByTestId('voice-panel').getByRole('button', { name: 'Join voice' }).click();
    await bob.page.getByTestId('voice-panel').getByRole('button', { name: /Listen only/ }).click();
    for (const p of [alice, bob]) {
      await pollFor(
        async () => {
          const rows = await voiceRows(p);
          return rows.length >= 3 && ['native', 'alice', 'bob'].every((n) => rows.some((r) => r.text.includes(n)));
        },
        20_000,
        250,
        `${p.nick} to see native, alice and bob in the voice roster`,
      );
    }
    await pollFor(() => (native.latest()?.received?.['alice [web]'] ?? 0) > 50, 20_000, 250, 'the native user to receive alice’s voice');
    log(`2a ok: the native Mumble user received ${native.latest().received['alice [web]']} frames from "alice [web]"`);
    for (const p of [alice, bob]) {
      await pollFor(
        async () => (await voiceRows(p)).some((r) => r.text.includes('native') && r.speaking),
        15_000,
        100,
        `${p.nick} to see the native user speaking (the echo)`,
      );
    }
    log('2b ok: alice and bob both hear the native user (speaking indicator from received frames)');
    if ((native.latest()?.received?.['bob [web]'] ?? 0) > 0) fail('listen-only bob sent voice');
    await shot(alice, 'alice-in-voice');

    // ---- 3. Mumble-side moderation ---------------------------------------
    await admin('admin-mute', ['-mute', 'alice [web]']);
    await pollFor(async () => /A Mumble admin muted you/.test(await sectionText(alice)), 10_000, 200, 'alice’s panel to say a Mumble admin muted her');
    log('3a ok: a Mumble server mute reaches alice’s panel');
    await admin('admin-unmute', ['-unmute', 'alice [web]']);
    await admin('admin-kick', ['-kick', 'bob [web]']);
    await pollFor(async () => /removed from voice/i.test(await sectionText(bob)), 10_000, 200, 'bob’s panel to say he was removed from voice');
    log('3b ok: a Mumble kick ends bob’s voice session, terminally (4015)');
    await shot(bob, 'bob-kicked');

    // ---- 9. a Mumble ban covers one person (R88) -------------------------
    if (PROXY) {
      const erin = await newParticipant(browser, 'erin', certHash);
      await openRoom(erin, ROOM);
      await erin.page.getByTestId('voice-panel').getByRole('button', { name: /Listen only/ }).click();
      await pollFor(async () => (await voiceRows(alice)).some((r) => r.text.includes('erin')), 20_000, 250, 'erin to join voice');
      const addrs = murmurAddresses();
      const a = addrs.get('alice [web]');
      const e = addrs.get('erin [web]');
      if (!a || !e) fail(`murmur's log has no address for alice (${a}) or erin (${e})`);
      if (a === e || [a, e].includes('127.0.0.1')) fail(`murmur sees alice at ${a} and erin at ${e}: want two browser addresses, neither the bridge's`);
      log(`9a ok: murmur sees alice at ${a} and erin at ${e}, the browsers' own addresses`);
      const heard = native.latest()?.received?.['alice [web]'] ?? 0;
      await admin('admin-ban', ['-kick', 'erin [web]', '-ban']);
      await pollFor(async () => /removed from voice/i.test(await sectionText(erin)), 10_000, 200, 'erin’s panel to say she was removed from voice');
      await pollFor(() => (native.latest()?.received?.['alice [web]'] ?? 0) > heard + 50, 15_000, 250, 'the native user to keep hearing alice after erin’s ban');
      if (!/Leave voice/.test(await sectionText(alice))) fail('alice left voice when erin was banned');
      log('9b ok: a Mumble kickban of erin ends her voice (4015); alice stays in voice and the native user still hears her');
      await shot(erin, 'erin-banned');
    }

    // ---- 4. a bridge pod drains under alice ------------------------------
    const counts = await Promise.all(BRIDGES.map(bridgeSessions));
    const host = BRIDGES[counts.findIndex((n) => n > 0)];
    if (!host) fail(`no bridge pod reports alice's session (${counts})`);
    const other = BRIDGES.find((b) => b !== host);
    log(`alice is on ${host.name}; draining it`);
    lbPublic.terminate(host.name);
    lbInternal.terminate(host.name);
    const before = native.latest()?.received?.['alice [web]'] ?? 0;
    bridgeProcs[host.name].child.kill('SIGTERM');
    await pollFor(() => bridgeProcs[host.name].exited, 15_000, 100, `${host.name} to finish its drain`);
    await pollFor(async () => (await bridgeSessions(other)) >= 1, 20_000, 250, `alice to rejoin on ${other.name}`);
    await pollFor(
      () => (native.latest()?.received?.['alice [web]'] ?? 0) > before + 50,
      20_000,
      250,
      'the native user to hear alice again after the drain',
    );
    await pollFor(
      async () => Object.values((await relayStatus()).rooms ?? {}).some((r) => r.voiceObserver === 'available'),
      30_000,
      250,
      'the room’s observer to be back (redialled onto the surviving pod)',
    );
    log(`4 ok: the drain moved alice to ${other.name} with a short gap and the observer followed`);

    // ---- 5. a dynamic room's temporary channel ---------------------------
    const pub = launch('pubsim', bin('gawk-pubsim'), [
      '-url', `https://127.0.0.1:${RELAY_PORT}`, '-insecure', '-duration', '300s', '-room-new', '-room-voice', '-nick', 'dyn',
    ]);
    const code = (await pollFor(() => pub.buf.match(/^ROOM ([A-Z0-9]{6}) /m)?.[1], 30_000, 200, 'the minted voice room')).toLowerCase();
    const dave = await newParticipant(browser, 'dave', certHash);
    const tempChannels = async () => ((await serverView()).channels ?? []).filter((c) => c.startsWith(`${TEMP_PARENT}/`));
    let dynNative;
    if (ICE) {
      // docs/89 D5: nobody keeps the channel, so it does not exist until
      // the first gawk voice user creates it, and the roster is empty.
      await pollFor(
        async () => Object.values((await relayStatus()).rooms ?? {}).filter((r) => r.voiceObserver === 'available').length >= 2,
        20_000,
        250,
        'the dynamic room’s watcher to be available',
      );
      if ((await tempChannels()).length) fail(`a temporary channel exists before anyone joined voice: ${await tempChannels()}`);
      await noObserverUser('before the dynamic room’s first voice user');
      await openRoom(dave, code.toUpperCase());
      if ((await voiceRows(dave)).length) fail('the dynamic room’s roster is not empty before anyone joined voice');
      await dave.page.getByTestId('voice-panel').getByRole('button', { name: 'Join voice' }).click();
      await pollFor(async () => (await tempChannels()).length === 1, 20_000, 500, 'dave’s join to create the room’s temporary channel');
      dynNative = mumblesim('dyn-native', ['-temp-under', TEMP_PARENT, '-temp-code', code, '-ticket-secret', SECRET]);
      await pollFor(() => dynNative.latest()?.channel > 0, 20_000, 250, 'a native user to find the room’s temporary channel by gawk’s name');
    } else {
      dynNative = mumblesim('dyn-native', ['-temp-under', TEMP_PARENT, '-temp-code', code, '-ticket-secret', SECRET]);
      await pollFor(() => dynNative.latest()?.channel > 0, 20_000, 250, 'a native user to find the room’s temporary channel by gawk’s name');
      await openRoom(dave, code.toUpperCase());
    }
    await pollFor(
      async () => (await voiceRows(dave)).some((r) => r.text.includes('dyn-native')),
      20_000,
      250,
      'the dynamic room’s roster to show the native user in its temporary channel',
    );
    if (ICE) {
      await noObserverUser('after the dynamic room’s first voice user');
      await dave.page.getByTestId('voice-panel').getByRole('button', { name: 'Leave voice' }).click();
      dynNative.child.kill('SIGTERM');
      await pollFor(async () => (await tempChannels()).length === 0, 20_000, 500, 'murmur to remove the temporary channel once everyone left');
      log('5 ok: the first voice user created the dynamic room’s channel, a native user joined it by its fleet-stable name, no observer user ever existed, and murmur removed it once empty');
    } else {
      log('5 ok: the dynamic room got a temporary channel under the parent, found by its fleet-stable name');
    }

    if (ICE) {
      // ---- 7. an Ice outage mid-call -----------------------------------
      const heard = () => native.latest()?.received?.['alice [web]'] ?? 0;
      proxy.cut();
      await pollFor(
        async () => Object.values((await relayStatus()).rooms ?? {}).some((r) => r.voiceObserver === 'unavailable'),
        20_000,
        250,
        'the relay to report the observer unavailable during the Ice outage',
      );
      // The roster is unavailable, so the panel has no native row to
      // light up: alice hearing the native user is the frames her pod
      // forwards to her browser (the native user's echo of her).
      const before7 = heard();
      const alicePod = BRIDGES.find((b) => b.name === other.name);
      const toAlice = await bridgeToBrowser(alicePod);
      await pollFor(() => heard() > before7 + 50, 20_000, 250, 'the native user to keep hearing alice during the outage');
      await pollFor(async () => (await bridgeToBrowser(alicePod)) > toAlice + 50, 20_000, 250, 'alice to keep hearing the native user during the outage');
      await noObserverUser('during the Ice outage');
      await proxy.start();
      await pollFor(
        async () => (await voiceRows(viewer)).some((r) => r.text.includes('native')),
        45_000,
        250,
        'the roster to come back once Ice is restored',
      );
      await noObserverUser('after the Ice outage');
      log('7 ok: an Ice outage left voice up both ways, the observer reported unavailable, and the roster came back with no bot');

      // ---- 8. the watching pod is SIGKILLed ----------------------------
      // Two pods again: the one pass 4 drained comes back.
      bridgeProcs[host.name] = startBridge(host);
      await waitForHttp(`http://127.0.0.1:${host.ops}/readyz`, 15_000, host.name);
      lbPublic.restore(host.name);
      lbInternal.restore(host.name);
      const watching = async () => {
        for (const b of BRIDGES) {
          if (bridgeProcs[b.name].exited) continue;
          const st = await bridgeStatus(b).catch(() => null);
          if (Object.values(st?.rooms ?? {}).some((r) => r.observer && !r.observer.temporary)) return b;
        }
        return null;
      };
      const victim = await pollFor(watching, 20_000, 250, 'a pod to hold the static room’s observer link');
      const survivor = BRIDGES.find((b) => b !== victim);
      await pollFor(async () => (await bridgeStatus(victim)).ice?.callbacks === 'registered', 20_000, 250, `${victim.name} to hold its registration`);
      const removed = () =>
        murmurLog().split('\n').filter((l) => l.includes('Removed Ice ServerCallback') && l.includes(`-p ${CALLBACK_PORTS[victim.name]} `)).length;
      const removedBefore = removed();
      lbPublic.terminate(victim.name);
      lbInternal.terminate(victim.name);
      bridgeProcs[victim.name].child.kill('SIGKILL');
      await pollFor(() => bridgeProcs[victim.name].exited, 10_000, 100, `${victim.name} to die`);
      await pollFor(
        async () => (await watching()) === survivor && Object.values((await relayStatus()).rooms ?? {}).some((r) => r.voiceObserver === 'available'),
        45_000,
        250,
        `the static room’s observer link to move to ${survivor.name}`,
      );
      await admin('admin-mute-native', ['-mute', 'native']);
      await pollFor(
        async () => (await viewer.page.getByTestId('voice-panel').getByLabel('Muted by the server').count()) > 0,
        15_000,
        200,
        'the native user’s server mute to reach the roster through the surviving pod',
      );
      await admin('admin-unmute-native', ['-unmute', 'native']);
      const running = execFileSync('docker', ['inspect', '-f', '{{.State.Running}}', 'gawk-e2e-murmur-main']).toString().trim();
      if (running !== 'true') fail('murmur is not running after the SIGKILL');
      await pollFor(() => removed() > removedBefore, 20_000, 500, 'murmur to drop the dead pod’s registration');
      await noObserverUser('after the SIGKILL');
      log(`8 ok: ${victim.name} died uncleanly; the observer link moved to ${survivor.name}, the native user’s mute reached the roster, and murmur dropped the dead registration`);
    }

    // ---- 6. a gawk IP ban ends voice -------------------------------------
    // 127.0.0.0/8: the LoadBalancer stand-in gives each flow its own
    // loopback address (R88), so alice's isn't necessarily 127.0.0.1.
    writeFileSync(bansFile, JSON.stringify([{ target: { type: 'ip', value: '127.0.0.0/8' }, reason: 'e2e' }]));
    await pollFor(async () => /removed from voice/i.test(await sectionText(alice)), 15_000, 250, 'alice’s voice to end on the gawk ban');
    log('6 ok: a gawk IP ban ends alice’s voice session (4015)');
    await shot(alice, 'alice-banned');

    const live = BRIDGES.find((b) => !bridgeProcs[b.name].exited) ?? other;
    const bs = await bridgeStatus(live);
    if (bs.roster !== ROSTER) fail(`the bridge reports roster ${bs.roster}, want ${ROSTER}`);
    if (JSON.stringify(bs).includes(ROOM)) fail('the bridge /statusz names a raw room code');
    log('all voice passes ok');
  } finally {
    await browser.close().catch(() => {});
    lbPublic.close();
    lbInternal.close();
    proxy.cut();
  }
}

try {
  await main();
} catch (e) {
  if (!process.exitCode) {
    console.error(e);
    process.exitCode = 1;
  }
} finally {
  for (const c of children) if (!c.exited) c.child.kill('SIGTERM');
  await sleep(500);
  for (const c of children) if (!c.exited) c.child.kill('SIGKILL');
  try {
    writeFileSync(join(OUT, 'murmur.log'), murmurLog());
  } catch {
    // best effort
  }
  spawnSync('docker', ['rm', '-f', MMPROXY_CONTAINER]);
  try {
    execFileSync(join(REPO, 'gawk-mumble-bridge/testdata/murmur.sh'), ['down'], {
      env: { ...process.env, GAWK_MURMUR_PREFIX: 'gawk-e2e-murmur' },
    });
  } catch {
    // best effort
  }
}
