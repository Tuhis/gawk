#!/usr/bin/env node
// R79 MV6 (docs/80 §9): the room voice panel in a real Chromium, against a
// FAKE relay and a FAKE bridge. No gawk-server, no gawk-mumble-bridge, no
// murmur: WebTransport itself is replaced in the page (addInitScript) by a
// stub that plays a relay room session (`/room/{code}`: RoomState with the
// voice capability, then a VoiceRosterUpdated, and a VoiceTicket for every
// VoiceJoin) and a bridge voice session (`/voice`: answers the VoiceHello
// with VoiceState and loops every VoiceFrame back as another speaker's).
// The microphone is Chromium's fake device, so the production capture path
// (getUserMedia → MediaStreamTrackProcessor → AudioEncoder) really runs, and
// the looped frames really go through AudioDecoder and the room's mixer.
//
// It clicks Join voice, waits for the panel to say it is in voice, checks
// that frames went out and came back (speaking indicator) and that the voice
// session dialled with the same certificate pins as the relay, then saves
// screenshots at 360, 390, 430 and 1280 px into e2e/out/voice-panel/
// (gitignored, e2e/out*/), asserting no horizontal overflow at each width.
//
// Run it (from the repository root):
//
//   (cd gawk-app && npm ci && npm run build)
//   (cd e2e && npm ci)
//   node e2e/voice-panel.mjs
//
// Environment: GAWK_E2E_CHROME (a Chrome/Chromium executable; default: the
// usual system paths, then Playwright's cached Chromium), GAWK_E2E_APP_DIR
// (default ../gawk-app), GAWK_E2E_PREVIEW_PORT (default 4179).

import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out', 'voice-panel');
const APP_DIR = resolve(process.env.GAWK_E2E_APP_DIR ?? join(HERE, '..', 'gawk-app'));
const PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4179);
const APP_URL = `http://127.0.0.1:${PORT}/`;
const PIN = 'ab'.repeat(32);
const WIDTHS = [360, 390, 430, 1280];

const log = (...a) => console.log('[voice-panel]', ...a);
function fail(msg) {
  console.error(`[voice-panel] FAIL: ${msg}`);
  process.exitCode = 1;
  throw new Error(msg);
}

function chromePath() {
  const fixed = process.env.GAWK_E2E_CHROME
    ? [process.env.GAWK_E2E_CHROME]
    : ['/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/usr/bin/chromium', '/usr/bin/chromium-browser'];
  for (const c of fixed) if (existsSync(c)) return c;
  const cache = join(homedir(), '.cache', 'ms-playwright');
  if (existsSync(cache)) {
    for (const d of readdirSync(cache).filter((n) => /^chromium-\d+$/.test(n)).sort().reverse()) {
      for (const sub of ['chrome-linux64/chrome', 'chrome-linux/chrome']) {
        const p = join(cache, d, sub);
        if (existsSync(p)) return p;
      }
    }
  }
  fail('no Chrome found; set GAWK_E2E_CHROME');
}

// The in-page stub. Runs before any app code; plain JS, its own tiny
// encoders (the layouts are gawk-server/wire/room.go and voice.go's).
function installFakeWebTransport() {
  const enc = new TextEncoder();
  const log = (window.__voiceE2E = {
    wantCaps: null,
    voiceJoins: 0,
    voiceDials: [],
    roomPins: null,
    helloFlags: null,
    controls: [],
    framesOut: 0,
    framesIn: 0,
  });
  const u16 = (v) => [(v >> 8) & 255, v & 255];
  const u32 = (v) => [(v >>> 24) & 255, (v >>> 16) & 255, (v >>> 8) & 255, v & 255];
  const str8 = (s) => {
    const b = enc.encode(s);
    return [b.length, ...b];
  };
  const record = (msg) => new Uint8Array([...u16(msg.length), ...msg]);
  const roomState = () => [
    1, 0x14, 0x01 /* dynamic */, 0x02 /* voice */, ...u32(3), ...u16(7), ...str8('AB2CD3'), ...str8(''),
    0, 0, 0 /* no attachments */, ...u16(3),
    ...u16(7), 0, 0, ...str8('me'), ...str8(''),
    ...u16(8), 0, 0, ...str8('alice'), ...str8(''),
    ...u16(9), 0, 0, ...str8('a-very-long-nickname-x'), ...str8(''),
  ];
  const entry = (session, kind, pid, flags, name) => [...u32(session), kind, ...u16(pid), flags, ...str8(name)];
  const roster = (withMe) => {
    const entries = [
      entry(3, 0, 0, 0x01, 'bob'),
      entry(4, 0, 0, 0x02 | 0x01, 'Carol the Mumble regular'),
      entry(9, 1, 8, 0, 'alice [web]'),
      entry(11, 1, 9, 0x04, 'a-very-long-nickname-x [web]'),
    ];
    if (withMe) entries.push(entry(42, 1, 7, 0, 'me [web]'));
    return [1, 0x15, ...u32(3), 0x51, 0x01, ...u16(entries.length), ...entries.flat()];
  };
  const ticket = () => {
    const url = enc.encode('https://bridge.test:4443/voice');
    return [1, 0x15, ...u32(3), 0x50, ...u16(url.length), ...url, ...u16(4), 9, 9, 9, 9];
  };
  const voiceState = () => [1, 0x1c, 0x20, ...u32(42), ...str8('me [web]')];

  class FakeWebTransport {
    constructor(url, init) {
      this.url = url;
      const path = new URL(url).pathname;
      this.kind = path.startsWith('/room/') ? 'room' : path === '/voice' ? 'voice' : 'other';
      this.closed = new Promise((res) => (this._close = res));
      this.incomingUnidirectionalStreams = new ReadableStream({});
      let dg;
      this.datagrams = {
        maxDatagramSize: 1200,
        readable: new ReadableStream({ start: (c) => (dg = c) }),
        writable: new WritableStream({ write: (d) => this._datagram(new Uint8Array(d)) }),
      };
      this._dg = dg;
      if (this.kind === 'other') {
        // Anything else (probes, telemetry) finds no server.
        this.ready = Promise.reject(new Error('stubbed: no server'));
        this.ready.catch(() => {});
        this.closed = Promise.reject(new Error('stubbed'));
        this.closed.catch(() => {});
        return;
      }
      this.ready = Promise.resolve();
      const pins = (init?.serverCertificateHashes ?? []).length;
      if (this.kind === 'room') log.roomPins = pins;
      else log.voiceDials.push({ url, pins });
    }
    async createBidirectionalStream() {
      let ctl;
      const readable = new ReadableStream({ start: (c) => (ctl = c) });
      let buf = new Uint8Array(0);
      const onRecord = (rec) => this._record(rec, (msg) => ctl.enqueue(record(msg)));
      const writable = new WritableStream({
        write(chunk) {
          const next = new Uint8Array(buf.length + chunk.length);
          next.set(buf);
          next.set(chunk, buf.length);
          buf = next;
          while (buf.length >= 2) {
            const n = (buf[0] << 8) | buf[1];
            if (buf.length < 2 + n) break;
            onRecord(buf.slice(2, 2 + n));
            buf = buf.slice(2 + n);
          }
        },
      });
      return { readable, writable };
    }
    _record(rec, send) {
      const type = rec[1];
      if (this.kind === 'room') {
        if (type === 0x13) {
          log.wantCaps = rec[4];
          send(roomState());
          send(roster(false));
        } else if (type === 0x16 && rec[2] === 0x50) {
          log.voiceJoins++;
          send(ticket());
        }
        return;
      }
      if (type === 0x1a) {
        log.helloFlags = rec[3];
        send(voiceState());
        // The relay's roster now lists us (the bridge registered the session).
        window.__voiceE2ERoomSend?.(roster(true));
      } else if (type === 0x1d) {
        log.controls.push(rec[2]);
      }
    }
    _datagram(d) {
      if (this.kind !== 'voice' || d[1] !== 0x1b) return;
      log.framesOut++;
      // Loop it back as bob's (session 3): the speaking indicator lights.
      const back = d.slice();
      back.set(u32(3), 4);
      log.framesIn++;
      try {
        this._dg.enqueue(back);
      } catch {
        // the session is gone
      }
    }
    close() {
      this._close({ closeCode: 0, reason: '' });
      try {
        this._dg.close();
      } catch {
        // already closed
      }
    }
  }
  // Remember the room session's sender so the bridge can push a roster.
  const origCreate = FakeWebTransport.prototype.createBidirectionalStream;
  FakeWebTransport.prototype.createBidirectionalStream = async function () {
    const s = await origCreate.call(this);
    if (this.kind === 'room') {
      const reader = s.readable;
      let ctl;
      const tee = new ReadableStream({
        start: (c) => (ctl = c),
      });
      const r = reader.getReader();
      void (async () => {
        for (;;) {
          const { value, done } = await r.read();
          if (done) break;
          ctl.enqueue(value);
        }
      })();
      window.__voiceE2ERoomSend = (msg) => ctl.enqueue(record(msg));
      return { readable: tee, writable: s.writable };
    }
    return s;
  };
  window.WebTransport = FakeWebTransport;
}

async function waitForHttp(url, ms) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      // not up yet
    }
    await sleep(200);
  }
  fail(`${url} did not come up`);
}

async function noOverflow(page, width) {
  const r = await page.evaluate(() => {
    const doc = document.documentElement;
    const panel = document.querySelector('[aria-label="People and chat"]');
    const voice = document.querySelector('[data-testid="voice-section"]');
    const offenders = [...(voice?.querySelectorAll('*') ?? [])]
      .filter((el) => {
        const b = el.getBoundingClientRect();
        return b.width > 0 && (b.right > window.innerWidth + 0.5 || b.left < -0.5);
      })
      .map((el) => `${el.tagName.toLowerCase()}.${el.className}`.slice(0, 80));
    return {
      doc: doc.scrollWidth - doc.clientWidth,
      panel: panel ? panel.scrollWidth - panel.clientWidth : 0,
      offenders: offenders.slice(0, 5),
    };
  });
  if (r.doc > 0 || r.panel > 0 || r.offenders.length > 0) {
    fail(`horizontal overflow at ${width}px: page +${r.doc}px, panel +${r.panel}px, ${r.offenders.join(', ')}`);
  }
}

async function main() {
  if (!existsSync(join(APP_DIR, 'dist', 'index.html'))) fail(`${APP_DIR}/dist missing — run npm run build in gawk-app`);
  const vite = join(APP_DIR, 'node_modules', '.bin', 'vite');
  if (!existsSync(vite)) fail(`${vite} missing — run npm ci in gawk-app`);
  mkdirSync(OUT, { recursive: true });
  const preview = spawn(vite, ['preview', '--port', String(PORT), '--strictPort', '--host', '127.0.0.1'], {
    cwd: APP_DIR,
    stdio: 'ignore',
  });
  let browser;
  try {
    await waitForHttp(APP_URL, 20_000);
    browser = await chromium.launch({
      executablePath: chromePath(),
      headless: true,
      args: [
        '--no-sandbox',
        '--use-fake-device-for-media-stream',
        '--use-fake-ui-for-media-stream',
        '--autoplay-policy=no-user-gesture-required',
      ],
    });
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
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));
    page.on('console', (m) => {
      if (/voice/i.test(m.text())) log('page:', m.text());
    });
    await page.goto(`${APP_URL}#/room/AB2CD3`);

    await page.getByRole('button', { name: 'People and chat' }).click();
    const section = page.getByTestId('voice-section');
    await section.waitFor({ timeout: 15_000 });
    const e2e = () => page.evaluate(() => window.__voiceE2E);
    if ((await e2e()).wantCaps !== 0x02) fail(`RoomHello wantCaps ${(await e2e()).wantCaps}, want ROOM_CAP_VOICE`);
    await section.getByText('This is a Mumble channel shared with people outside gawk').waitFor();
    if ((await section.getByTestId('voice-row').count()) !== 4) fail('the roster does not show four people');
    await page.screenshot({ path: join(OUT, 'before-join-1280.png') });

    await section.getByRole('button', { name: 'Join voice' }).click();
    // Joining → (mic) → VoiceJoin → ticket → voice session → VoiceState.
    const joinBy = Date.now() + 20_000;
    for (;;) {
      if ((await section.getByText(/Couldn’t/).count()) > 0) fail(`voice failed: ${await section.innerText()}`);
      if ((await e2e()).helloFlags !== null && (await page.getByTestId('voice-dock').innerText()).includes('Voice')) {
        if (!(await page.getByTestId('voice-dock').innerText()).includes('Voice…')) break;
      }
      if (Date.now() > joinBy) fail(`never got into voice: ${await section.innerText()}`);
      await sleep(200);
    }
    const st = await e2e();
    if (st.voiceJoins !== 1) fail(`${st.voiceJoins} VoiceJoins, want 1`);
    if (st.voiceDials.length !== 1 || st.voiceDials[0].pins !== st.roomPins || st.roomPins !== 1) {
      fail(`the voice session's pins ${JSON.stringify(st.voiceDials)} differ from the relay's (${st.roomPins})`);
    }
    if (st.helloFlags !== 0) fail(`hello flags ${st.helloFlags}, want 0 (speaking, not muted)`);
    log('in voice; waiting for the fake mic to talk');
    // Chromium's fake microphone beeps: voice activity sends, the bridge
    // loops it back as bob, and bob's row lights.
    const deadline = Date.now() + 15_000;
    while ((await e2e()).framesOut === 0 && Date.now() < deadline) await sleep(200);
    const after = await e2e();
    if (after.framesOut === 0) fail('no VoiceFrame left the browser (capture → encoder → gate)');
    log(`frames out ${after.framesOut}, looped back ${after.framesIn}`);
    await page.waitForFunction(
      () => document.querySelector('[data-testid="voice-row"] [data-on="true"]') !== null,
      null,
      { timeout: 10_000 },
    );
    await page.getByTestId('voice-dock').getByRole('button', { name: 'Mute microphone' }).click();
    if (!(await e2e()).controls.includes(0x01)) fail('mute sent no VoiceControl');
    await page.getByTestId('voice-dock').getByRole('button', { name: 'Unmute microphone' }).click();
    await section.getByRole('radio', { name: 'Push to talk' }).click();

    for (const width of WIDTHS) {
      await page.setViewportSize({ width, height: width < 720 ? 800 : 800 });
      await sleep(400);
      await section.scrollIntoViewIfNeeded();
      await noOverflow(page, width);
      await page.screenshot({ path: join(OUT, `in-voice-${width}.png`) });
      log(`screenshot ${width}px ok`);
    }
    // The hold button on a phone layout talks while pressed.
    // On a phone the open panel is a bottom sheet over the dock: close it,
    // the call (and the dock) stays.
    await page.setViewportSize({ width: 390, height: 800 });
    await page.getByRole('button', { name: 'Close panel' }).click();
    await noOverflow(page, 390);
    const hold = page.getByTestId('voice-dock').getByRole('button', { name: /Hold to talk/ });
    await hold.hover();
    await page.mouse.down();
    await page.getByTestId('voice-dock').getByRole('button', { name: /Talking/ }).waitFor({ timeout: 3000 });
    await page.screenshot({ path: join(OUT, 'hold-to-talk-390.png') });
    await page.mouse.up();
    if (errors.length > 0) fail(`page errors: ${errors.join('; ')}`);
    log(`ok — screenshots in ${OUT}`);
  } finally {
    await browser?.close();
    preview.kill('SIGTERM');
  }
}

main().catch((e) => {
  if (!process.exitCode) {
    console.error(e);
    process.exitCode = 1;
  }
});
