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
import { existsSync, mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';
import { PIN, chromePath, installFakeWebTransport, overflowAt, waitForHttp } from './voice-fake.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out', 'voice-panel');
const APP_DIR = resolve(process.env.GAWK_E2E_APP_DIR ?? join(HERE, '..', 'gawk-app'));
const PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4179);
const APP_URL = `http://127.0.0.1:${PORT}/`;
const WIDTHS = [360, 390, 430, 1280];

const log = (...a) => console.log('[voice-panel]', ...a);
async function noOverflow(page, width) {
  const msg = await overflowAt(page, width);
  if (msg) fail(msg);
}
function fail(msg) {
  console.error(`[voice-panel] FAIL: ${msg}`);
  process.exitCode = 1;
  throw new Error(msg);
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
    const executablePath = chromePath();
    if (!executablePath) fail('no Chrome found; set GAWK_E2E_CHROME');
    browser = await chromium.launch({
      executablePath,
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
