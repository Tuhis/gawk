#!/usr/bin/env node
// R79 MV7 (docs/80 §9): room voice end to end, with every real component.
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
//   6. gawk ban             — an IP ban covering the browsers ends alice's
//                             voice (4015) on the relay's own ban event
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

import { spawn, execFileSync } from 'node:child_process';
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
async function admin(name, args) {
  const e = mumblesim(name, ['-name', 'SuperUser', '-password', MURMUR_SU, '-duration', '3s', ...args]);
  await pollFor(() => e.exited, 15_000, 100, `${name} to finish`);
  if (e.exited.code !== 0) fail(`${name} exited ${e.exited.code} (see out/voice/${name}.log)`);
}

async function relayStatus() {
  return (await fetch(`http://127.0.0.1:${RELAY_OPS}/statusz`)).json();
}
async function bridgeStatus(b) {
  return (await fetch(`http://127.0.0.1:${b.ops}/statusz`)).json();
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
  await p.page.getByRole('button', { name: 'People and chat' }).click();
  await p.page.getByTestId('voice-section').waitFor({ timeout: 20_000 });
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
  return p.page.getByTestId('voice-section').innerText();
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
    env: { ...process.env, GAWK_MURMUR_PREFIX: 'gawk-e2e-murmur', GAWK_MURMUR_BASE_PORT: String(MURMUR_BASE) },
  }).toString();
  appendFileSync(join(OUT, 'murmur.env'), murmurEnv);

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
  const lbPublic = new UdpBalancer({ listen: LB_PUBLIC, backends: BRIDGES.map((b) => ({ name: b.name, port: b.pub })) });
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
    log('1 ok: a viewer who never joined voice sees the native Mumble user in the room');

    // ---- 2. both directions --------------------------------------------
    const alice = await newParticipant(browser, 'alice', certHash);
    const bob = await newParticipant(browser, 'bob', certHash);
    await openRoom(alice, ROOM);
    await openRoom(bob, ROOM);
    await alice.page.getByTestId('voice-section').getByRole('button', { name: 'Join voice' }).click();
    await bob.page.getByTestId('voice-section').getByRole('button', { name: /Listen only/ }).click();
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
    const dynNative = mumblesim('dyn-native', ['-temp-under', TEMP_PARENT, '-temp-code', code, '-ticket-secret', SECRET]);
    await pollFor(() => dynNative.latest()?.channel > 0, 20_000, 250, 'a native user to find the room’s temporary channel by gawk’s name');
    const dave = await newParticipant(browser, 'dave', certHash);
    await openRoom(dave, code.toUpperCase());
    await pollFor(
      async () => (await voiceRows(dave)).some((r) => r.text.includes('dyn-native')),
      20_000,
      250,
      'the dynamic room’s roster to show the native user in its temporary channel',
    );
    log('5 ok: the dynamic room got a temporary channel under the parent, found by its fleet-stable name');

    // ---- 6. a gawk IP ban ends voice -------------------------------------
    writeFileSync(bansFile, JSON.stringify([{ target: { type: 'ip', value: '127.0.0.1/32' }, reason: 'e2e' }]));
    await pollFor(async () => /removed from voice/i.test(await sectionText(alice)), 15_000, 250, 'alice’s voice to end on the gawk ban');
    log('6 ok: a gawk IP ban ends alice’s voice session (4015)');
    await shot(alice, 'alice-banned');

    const bs = await bridgeStatus(other);
    if (JSON.stringify(bs).includes(ROOM)) fail('the bridge /statusz names a raw room code');
    log('all voice passes ok');
  } finally {
    await browser.close().catch(() => {});
    lbPublic.close();
    lbInternal.close();
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
    execFileSync(join(REPO, 'gawk-mumble-bridge/testdata/murmur.sh'), ['down'], {
      env: { ...process.env, GAWK_MURMUR_PREFIX: 'gawk-e2e-murmur' },
    });
  } catch {
    // best effort
  }
}
