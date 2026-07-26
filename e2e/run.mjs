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
//   node run.mjs --browser-broadcast
//                             Z5 — the browser publishes instead of pubsim:
//                             a second Chromium drives the production
//                             broadcaster surface, capturing an animated tab
//                             granted headlessly via
//                             --auto-select-tab-capture-source-by-title
//                             (the Z5 spike found screen capture works but
//                             delivers black frames in headless — tab
//                             capture delivers real pixels). Asserts the
//                             encode funnel from the broadcaster's own
//                             diagnostics, then runs the unchanged viewer
//                             scenario against the minted ID.
//   node run.mjs --muxer-check
//                             R22 MF1 (docs/27 Decision 10) — the production
//                             fMP4 muxer's output must PLAY in a real Chrome
//                             MediaSource <video> (frames present, currentTime
//                             advances). Standalone: no relay, publisher or
//                             preview — rolldown-bundles the in-page driver
//                             (gawk-app/src/e2e/muxer-check-entry.ts) and runs
//                             it in headless Chrome. Fast (~10 s); the iPhone
//                             native *presentation* stays manual (MF5), this
//                             proves the bytes are real media.
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
// Overridable so CI steps running both modes keep their failure artifacts
// apart (the file names inside overlap).
const OUT = join(HERE, process.env.GAWK_E2E_OUT_DIR ?? 'out');
const EXTERNAL = process.argv.includes('--external');
const BROWSER_BROADCAST = process.argv.includes('--browser-broadcast');
const MUXER_CHECK = process.argv.includes('--muxer-check');

const RELAY_PORT = Number(process.env.GAWK_E2E_RELAY_PORT ?? 4433);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2112);
const PREVIEW_PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4173);
const APP_DIR = resolve(HERE, process.env.GAWK_E2E_APP_DIR ?? '../gawk-app');
const SERVER_BIN = resolve(HERE, process.env.GAWK_E2E_SERVER_BIN ?? 'bin/gawk-server');
const PUBSIM_BIN = resolve(HERE, process.env.GAWK_E2E_PUBSIM_BIN ?? 'bin/gawk-pubsim');
const APP_URL = `http://127.0.0.1:${PREVIEW_PORT}`;
// ~5 s between the two diagnostics captures (Decision 6: "sustained").
const SAMPLE_GAP_MS = 5000;
// The viewer-side decoded-fps floor (Decision 6: flow, not performance). Kept
// well below the 30 fps source rate — a contended 2-core CI runner cannot
// promise the source rate, only that frames keep flowing. Lowered 10 → 8 after
// the runner narrowly missed 10 on otherwise-healthy runs.
const DECODED_FPS_FLOOR = 8;
// Encoded frames the browser broadcaster must produce before its first
// diagnostics capture — a progress gate on encoder warm-up, deliberately not
// a rate gate (that would pre-assert what assertBroadcastFlow() checks).
// Calibrated from a real CI run: the 2-core runner ramps through 2–16 fps for
// ~3.5 s, by which point it has encoded ~28 frames; a fast local box passes 30
// in ~0.5 s.
const WARMUP_FRAMES = 30;
// Up to this many *retries* of the viewer scenario (so MAX_VIEWER_RETRIES + 1
// total attempts) when its flow assertions don't hold — the fps floor above is
// a soft, runner-load-sensitive signal, so one narrow miss must not fail the
// run. Each retry relaunches the browser but never the relay or the publisher
// (Decision 8). Retries are logged loudly: recurring attempt-1 failures are
// findings for docs/25, not noise.
const MAX_VIEWER_RETRIES = 5;
// Extra settle for the R21 deep-buffer pass, on top of the standard one: the
// mode presents a playout offset (3 s) behind arrival, so the median window
// needs that long again of *decoded* samples before the fps floors mean
// anything. Sized to fill medianRecent's ≤6-sample (3 s) window twice over.
const DEEP_BUFFER_SETTLE_MS = 6000;
// The R19 resilient-mode pass (below) runs only in the plain tier-1 mode: the
// relay and publisher are already up there, so it costs one browser session.
// External/cluster mode and the browser-broadcast step spend their budget
// elsewhere (see the pass itself for why).
const RESILIENT_VIEWER_PASS = !EXTERNAL && !BROWSER_BROADCAST;
// Fewer retries than the main pass: it runs after a viewer scenario that
// already proved flow on this broadcast, so a failure here is far more likely
// to be the carrier path than runner weather.
const MAX_RESILIENT_RETRIES = 1;
// One hard cap on the whole run — a hung QUIC handshake must fail, not hang.
// The browser-broadcast mode runs two browser scenarios back to back, and the
// viewer scenario can now retry up to MAX_VIEWER_RETRIES times, so the cap
// leaves room for the retries to run before it aborts (still well under the
// CI job's own timeout-minutes). The plain tier-1 mode gets the same headroom
// as browser-broadcast because of its second (resilient) viewer pass.
const WATCHDOG_MS = EXTERNAL ? 300_000 : 360_000;

// Z5: the tab the headless broadcaster captures. The flag matches by title
// substring, so the value must never be a substring of the app's own title
// ("gawk") — and it isn't, the match direction is tab-title-contains-flag.
const ANIM_TITLE = 'gawk-e2e-motion-source';
// A full-viewport animated canvas: tab capture is damage-driven, so the
// captured tab must keep painting or captureFps reads ~0. Colorful and
// moving so the viewer-side non-black/non-uniform screenshot check holds.
const ANIM_HTML = `<!doctype html><title>${ANIM_TITLE}</title>
<body style="margin:0"><canvas id="c" width="1280" height="720"></canvas>
<script>
const ctx = document.getElementById('c').getContext('2d');
let t = 0;
(function draw() {
  t++;
  const g = ctx.createLinearGradient(0, 0, 1280, 720);
  g.addColorStop(0, 'hsl(' + (t * 3 % 360) + ',90%,55%)');
  g.addColorStop(1, 'hsl(' + ((t * 3 + 180) % 360) + ',90%,45%)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, 1280, 720);
  ctx.fillStyle = '#fff';
  ctx.font = '96px monospace';
  ctx.fillText('frame ' + t, 80 + (t % 400), 360);
  requestAnimationFrame(draw);
})();
</script>`;

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
// expectedCodec: exact string for the fixture publisher, RegExp for the
// browser broadcaster (whose codec is negotiated, not fixed).
function assertFlow(d1, d2, expectedCodec = 'avc1.42C00D') {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };

  // Pubsim: the codec is fully determined by the committed fixture's SPS — a
  // mismatch means the config path (or the fixture) changed. Browser
  // broadcaster: any H.264 variant from the negotiation cascade.
  const codecOk =
    expectedCodec instanceof RegExp ? expectedCodec.test(d2.codec) : d2.codec === expectedCodec;
  check(codecOk, `codec = ${d2.codec}, want ${expectedCodec}`);

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
    check(decoded >= DECODED_FPS_FLOOR, `${name}: median decoded fps = ${decoded}, want >= ${DECODED_FPS_FLOOR}`);
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

function launchBrowser(extraArgs = []) {
  return chromium.launch({
    executablePath: chromePath(),
    headless: true,
    // The sandbox needs user namespaces the runner/container may not have;
    // this is a test harness, not a browsing session.
    args: ['--no-sandbox', ...extraArgs],
  });
}

// A context with the persisted transport settings seeded (the keys
// useTransportStore owns), the clipboard stubbed (the ViewerScreen.test.tsx
// precedent), and the publish-secret prompt disabled (loopback hosts count as
// dev environments, where the prompt defaults on) — all before any app code
// runs. `resilient` seeds R19's persisted toggle, which is the only way in:
// the mode is negotiated at connect, so it has to be set before the app runs.
async function newAppContext(browser, { relayUrl, certHash, delivery = null }) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  await context.addInitScript(
    ({ serverUrl, hash, deliveryMode }) => {
      localStorage.setItem('gawk.serverUrl', serverUrl);
      if (hash) localStorage.setItem('gawk.certHashHex', hash);
      // R19/R21: the persisted delivery choice is the only way in — the mode
      // is negotiated at connect, so it has to be set before the app runs.
      if (deliveryMode) localStorage.setItem('gawk:viewer-delivery', deliveryMode);
      // The shipped public/config.js assigns nothing, so this seed survives.
      // R23: pin the terms version and pre-accept it so the broadcaster's
      // one-time acknowledgment modal never gates "Start a stream" — the same
      // move as requirePublishSecret:false above (skip a pre-start modal the
      // gate has its own unit coverage for). Pinning the version keeps this
      // robust when the bundled version bumps. Viewers are never gated.
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
    },
    { serverUrl: relayUrl, hash: certHash, deliveryMode: delivery },
  );
  return context;
}

function wirePageLogs(page, name) {
  const consoleLog = join(OUT, `${name}.log`);
  writeFileSync(consoleLog, '');
  page.on('console', (msg) => appendFileSync(consoleLog, `[${msg.type()}] ${msg.text()}\n`));
  page.on('pageerror', (err) => appendFileSync(consoleLog, `[pageerror] ${err}\n`));
}

// R19 (docs/24) resilient mode, from the client side. The relay half of the
// carrier path is covered in Go under injected packet loss
// (gawk-server/internal/transport/resilient_loss_test.go); what only a real
// browser can show is the other half — that the production viewer negotiates
// `?delivery=reliable` against a real relay and that its own carrier reader
// turns those uni streams back into decodable frames. Loss is deliberately
// NOT injected here: this pass exists to prove the client path exists and
// works, and the Go test owns the behaviour-under-loss claim.
//
// R21 (docs/26) Deep buffer, from the client side. assertFlow already covers
// the freeze this exists to catch — decoded/rendered fps floors — so this adds
// only what is specific to the mode: that it was actually granted, and the
// one-keyframe-per-carrier invariant.
//
// That invariant is field finding 1 (docs/26, 2026-07-23) turned into a test.
// A DVR subscriber's keyframes come from the ring at its own cursor; anything
// else sending it the *live* keyframe hands it a second, contradictory
// timeline and video freezes while every arrival counter keeps climbing. The
// tell was the ratio: keyframes arriving at ~2x the carrier-rotation rate.
// assertFlow would have failed on the frozen decoder, but the ratio names the
// cause instead of just the symptom.
function assertDvrFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };
  const s1 = latest(d1);
  const s2 = latest(d2);

  // Only the relay's own DeliveryAck can say this: a replayed GOP is
  // byte-identical to a live one, so nothing observable distinguishes them.
  // 'reliable' here means the ?buffer= negotiation did not take.
  check(
    s2.deliveryMode === 'dvr',
    `deliveryMode = ${s2.deliveryMode}, want "dvr" (the ?buffer= negotiation did not take)`,
  );
  check(s2.dvrBufferMs > 0, `dvrBufferMs = ${s2.dvrBufferMs}, want > 0 (the ack carried no buffer)`);

  const growth = (key) => s2[key] - s1[key];
  const carriers = growth('carrierStreams');
  const keyframes = growth('keyframeStreamsReceived');
  check(carriers >= 1, `no carrier rotation between the captures (${s1.carrierStreams} → ${s2.carrierStreams})`);
  check(growth('carrierRecords') > 0, `no carrier records between the captures`);
  // One keyframe per rotation, both from the same cursor. A tolerance of 1
  // absorbs a capture landing between a rotation and its keyframe; 2x is the
  // signature of two timelines and must never pass.
  check(
    Math.abs(keyframes - carriers) <= 1,
    `${keyframes} keyframes vs ${carriers} carrier rotations between the captures — ` +
      `these must be 1:1 in DVR mode; a ratio near 2 means the viewer is being served ` +
      `two timelines (docs/26 field finding 1)`,
  );

  if (problems.length > 0) {
    fail(`deep-buffer assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `deep buffer ok: ${keyframes} keyframes / ${carriers} carrier rotations (1:1), ` +
      `buffer ${s2.dvrBufferMs} ms`,
  );
}

// Flow-shaped like every other assertion here (Decision 6) — counters that
// must move, never a rate or a stream count that a contended runner could
// legitimately miss.
function assertCarrierFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };
  const s1 = latest(d1);
  const s2 = latest(d2);

  // 'reliable-requested' is the graceful degradation the viewer reports when
  // it asked for carriers and the relay served datagrams anyway — here that
  // means the negotiation broke, since both ends are this build.
  check(
    s2.deliveryMode === 'reliable',
    `deliveryMode = ${s2.deliveryMode}, want "reliable" (the ?delivery=reliable negotiation did not take)`,
  );
  check(s2.carrierStreams > 0, `carrierStreams = ${s2.carrierStreams}, want > 0`);
  // Between the captures (~5 s, GOP 500 ms) the relay must have rotated the
  // carrier and kept feeding it: a single stream that stopped delivering
  // would satisfy the counters above and nothing else.
  check(
    s2.carrierStreams - s1.carrierStreams >= 1,
    `no carrier rotation between the captures (${s1.carrierStreams} → ${s2.carrierStreams} streams)`,
  );
  check(
    s2.carrierRecords - s1.carrierRecords > 0,
    `no carrier records between the captures (${s1.carrierRecords} → ${s2.carrierRecords})`,
  );

  if (problems.length > 0) {
    fail(`resilient-mode assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  // Aborted carriers are recorded, not asserted: a reset GOP tail is the
  // relay shedding a slow subscriber, which a contended runner can legitimately
  // provoke — it is not a wire bug.
  log(
    `resilient mode ok: ${s2.carrierStreams} carrier streams, ${s2.carrierRecords} records ` +
      `(+${s2.carrierRecords - s1.carrierRecords} between captures, ${s2.carrierStreamsAborted ?? 0} aborted)`,
  );
}

async function browserScenario({ relayUrl, certHash, id, attempt, expectedCodec, delivery = null }) {
  const browser = await launchBrowser();
  try {
    const context = await newAppContext(browser, { relayUrl, certHash, delivery });
    const page = await context.newPage();
    wirePageLogs(page, `console-${attempt}`);

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

    // Deep buffer holds ~3 s before it presents anything, so "frames are
    // arriving" happens a playout offset before "frames are decoding". Waiting
    // only for arrival would capture a median window still full of the
    // pre-decode zeros and fail assertFlow's fps floors on a healthy run — a
    // flake that would (correctly) get the pass deleted rather than fixed.
    // Wait for decode itself, then let the ≤6-sample window fill with it.
    if (delivery === 'deep') {
      await pollFor(
        async () => {
          const v = (await rowValue(page, 'Decoded'))?.trim();
          return /^\d+$/.test(v) && Number(v) > 0;
        },
        30_000,
        1000,
        'the first decoded frame (deep buffer holds a playout offset first)',
      );
      await sleep(DEEP_BUFFER_SETTLE_MS);
    }

    // The first completed frame proves flow, but the ≤6-sample median window
    // still holds the connect/prime ticks behind it, each a zero. Let ~4
    // healthy ticks land so those cannot be a majority — the same settle the
    // broadcaster scenario takes after its first encoded frame, and for the
    // same reason (a 4-sample window is where medianRecent stops being
    // robust).
    await sleep(2000);
    const diag1 = await captureDiagnostics(page, `diagnostics-${attempt}-1`);
    await sleep(SAMPLE_GAP_MS);
    const diag2 = await captureDiagnostics(page, `diagnostics-${attempt}-2`);
    assertFlow(diag1, diag2, expectedCodec);
    if (delivery === 'resilient') assertCarrierFlow(diag1, diag2);
    if (delivery === 'deep') assertDvrFlow(diag1, diag2);

    const shot = await page.locator('canvas').first().screenshot();
    writeFileSync(join(OUT, `viewer-${attempt}.png`), shot);
    assertNonBlack(shot);
  } finally {
    await browser.close();
  }
}

// ---------------------------------------------------------------------------
// Browser broadcaster (Z5, docs/25 Decision 9)
// ---------------------------------------------------------------------------

// Z5's flow-shaped funnel assertions over the broadcaster's own diagnostics
// (capture → encode → sent, docs/13 D5). Floors sit far below the 30 fps tab
// capture rate — flow, not performance, same as the viewer side.
function assertBroadcastFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };

  // Chromium always carries a software H.264 encoder, so the negotiation
  // cascade must land on some avc1 variant; anything else means the config
  // path changed.
  check(
    typeof d2.encoder?.codec === 'string' && /^avc1\./.test(d2.encoder.codec),
    `encoder codec = ${d2.encoder?.codec}, want an avc1 variant`,
  );

  for (const [name, d] of [
    ['capture 1', d1],
    ['capture 2', d2],
  ]) {
    const capture = medianRecent(d, 'captureFps');
    const encoder = medianRecent(d, 'encoderFps');
    const sent = medianRecent(d, 'sentFps');
    check(capture >= 10, `${name}: median capture fps = ${capture}, want >= 10`);
    check(encoder >= 5, `${name}: median encoder fps = ${encoder}, want >= 5`);
    check(sent > 0, `${name}: median sent fps = ${sent}, want > 0`);
  }

  const s1 = latest(d1);
  const s2 = latest(d2);
  // Placement is recorded, not asserted: headless Chrome 149 exposes no
  // MediaStreamTrackProcessor in workers and refuses MediaStreamTrack
  // transfer, so the R11 capability probe correctly falls back to the
  // main-thread pipeline here — a legitimate production path (Firefox always
  // takes it), exercised end-to-end either way.
  check(
    s2.pipelineContext === 'worker' || s2.pipelineContext === 'main-thread',
    `pipelineContext = ${s2.pipelineContext}, want a reported pipeline placement`,
  );

  const growth = (key) => s2[key] - s1[key];
  const encoded = growth('encodedFrames');
  check(encoded > 0, 'no frames encoded between the captures');
  check(growth('keyframes') >= 1, 'no keyframe encoded between the captures (GOP is 500 ms)');
  check(growth('datagramsSent') > 0, 'no datagrams sent between the captures');
  check(growth('keyframeStreamsSent') >= 1, 'no keyframe streams sent between the captures');
  check(growth('bytesSent') > 0, 'no bytes sent between the captures');
  // Loopback link: a keyframe stream that fails to open here is a real bug,
  // and encoder drops must not dominate the encode rate.
  check(growth('keyframeStreamsFailed') === 0, `${growth('keyframeStreamsFailed')} keyframe streams failed between captures`);
  check(
    growth('droppedFrames') <= encoded,
    `encoder dropped ${growth('droppedFrames')} vs encoded ${encoded} between captures — drops dominate`,
  );

  if (problems.length > 0) {
    fail(`broadcast flow assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `broadcast flow ok: capture/encode/sent ≈ ${medianRecent(d2, 'captureFps')}/${medianRecent(d2, 'encoderFps')}/${medianRecent(d2, 'sentFps')} fps, ` +
      `codec ${d2.encoder.codec} (${d2.encoder.acceleration}), ${encoded} frames encoded between captures, ` +
      `pipeline ${s2.pipelineContext}`,
  );
}

// Drives the production broadcaster surface in its own headless browser: an
// animated tab is opened first, then the real "Start a stream" click's
// getDisplayMedia picker auto-selects it via the launch flag (the Z5 spike:
// headless *screen* capture grants but delivers black frames; *tab* capture
// delivers real pixels — don't switch back to a desktop-source flag). Returns
// the live browser (the broadcast must outlive this function) + the minted ID.
async function broadcasterScenario({ relayUrl, certHash, attempt }) {
  const browser = await launchBrowser([
    `--auto-select-tab-capture-source-by-title=${ANIM_TITLE}`,
  ]);
  try {
    const context = await newAppContext(browser, { relayUrl, certHash });

    // The capture source. Tab capture keeps a captured background tab
    // painting (spike-verified: full 30 fps with the tab unfocused).
    const animPage = await context.newPage();
    await animPage.setContent(ANIM_HTML);

    const page = await context.newPage();
    wirePageLogs(page, `console-broadcaster-${attempt}`);
    await page.goto(`${APP_URL}/#/broadcast`);
    await page.getByRole('button', { name: 'Start a stream' }).click();

    // LIVE = the topbar shows the minted broadcast code. Covers connect +
    // auto-granted capture + encoder init.
    const code = page.locator('code').first();
    await pollFor(
      async () => /^[A-Z0-9]{6}$/.test(((await code.textContent().catch(() => '')) ?? '').trim()),
      30_000,
      500,
      'the LIVE stage with a broadcast code',
    );
    const id = (await code.textContent()).trim();
    log(`browser broadcaster is LIVE as ${id}`);

    await page.getByRole('button', { name: 'Show stats' }).click();
    await page
      .locator('[role="dialog"][aria-label="Broadcast stats"]')
      .waitFor({ state: 'visible', timeout: 5000 });

    // Wait for the funnel to warm up, never a fixed guess at how long that
    // takes — and gate on *progress*, not on a rate, so the wait stays
    // independent of the rates assertBroadcastFlow() then checks.
    //
    // A wall-clock sleep is knife-edge here: the stats cadence is 500 ms, so
    // 2 s buys 4 samples, and medianRecent() over a 4-sample window flips on
    // a single tick. The two ends warm up very differently — a dev macOS box
    // reaches 60 fps in ~1.5 s (before that, zeros), while a 2-core CI runner
    // software-encoding 720p30 ramps through 2–16 fps for ~3.5 s before it
    // settles. Neither a fixed sleep nor "the first encoded frame" (true at
    // 2 fps, on the runner's very first tick) survives both.
    //
    // The encoded-frame counter does: it is monotonic, and reaching it takes
    // proportionally longer on a slower pipeline, which is exactly the
    // scaling wanted. 30 frames lands at t≈3.5 s on the CI runner (past the
    // ramp) and ~0.5 s on a fast local box. Bounded like every other wait.
    await pollFor(
      async () => {
        const v = (await rowValue(page, 'Encoded'))?.trim();
        return /^\d+$/.test(v) && Number(v) >= WARMUP_FRAMES;
      },
      30_000,
      500,
      `${WARMUP_FRAMES} encoded frames (encoder warm-up)`,
    );
    // Then let ~4 post-warm-up ticks land, so any residual ramp sample is a
    // minority of the ≤6-sample median window.
    await sleep(2000);
    const diag1 = await captureDiagnostics(page, `broadcast-diagnostics-${attempt}-1`);
    await sleep(SAMPLE_GAP_MS);
    const diag2 = await captureDiagnostics(page, `broadcast-diagnostics-${attempt}-2`);
    assertBroadcastFlow(diag1, diag2);

    return { browser, id };
  } catch (err) {
    await browser.close();
    throw err;
  }
}

// ---------------------------------------------------------------------------
// Muxer check (R22 MF1, docs/27 Decision 10)
// ---------------------------------------------------------------------------

// Bundles the production muxer + MSE presenter with the committed fixture into
// an IIFE, loads it into a blank headless-Chrome page, and asserts the video
// element actually presents frames from the muxed bytes. Chrome's classic
// MediaSource is the stand-in for iPhone's ManagedMediaSource (same
// SourceBuffer contract); only the native-player *presentation* is
// iPhone-manual.
async function muxerCheckScenario() {
  const bundle = join(OUT, 'muxer-check.js');
  const rolldown = join(APP_DIR, 'node_modules', '.bin', 'rolldown');
  if (!existsSync(rolldown)) fail(`${rolldown} not found — run npm ci in ${APP_DIR}`);
  const build = launch('rolldown', rolldown, [
    'src/e2e/muxer-check-entry.ts',
    '--format', 'iife',
    '--platform', 'browser',
    '-o', bundle,
  ], { cwd: APP_DIR });
  await pollFor(
    () => build.exited != null,
    60_000,
    200,
    'the muxer-check bundle build',
  );
  if (build.exited.code !== 0) fail('rolldown failed to bundle the muxer check (see out/rolldown.log)');

  const browser = await launchBrowser();
  try {
    const page = await browser.newPage();
    wirePageLogs(page, 'console-muxer-check');
    await page.goto('about:blank');
    await page.addScriptTag({ path: bundle });
    const res = await page.evaluate(() => window.__gawkMuxerCheck());
    writeFileSync(join(OUT, 'muxer-check.json'), JSON.stringify(res, null, 2));

    const problems = [];
    const check = (ok, msg) => {
      if (!ok) problems.push(msg);
    };
    check(res.videoError === null, `video element errored: ${res.videoError}`);
    check(/^video\/mp4; codecs="avc1\./.test(res.mime ?? ''), `mime = ${res.mime}, want an avc1 fMP4 mime`);
    check(res.initSegments === 1, `initSegments = ${res.initSegments}, want 1`);
    check(res.mediaSegments === 18, `mediaSegments = ${res.mediaSegments}, want 18 (the fixture)`);
    check(res.muxErrors === 0, `muxer counted ${res.muxErrors} errors`);
    check(res.appendErrors === 0, `presenter counted ${res.appendErrors} append errors`);
    check(
      res.segmentsAppended === 19,
      `segmentsAppended = ${res.segmentsAppended}, want 19 (init + 18 media)`,
    );
    // The two load-bearing verdicts (docs/27 MF1): frames present and the
    // clock advances — buffered-but-black media satisfies neither.
    check(res.framesPresented >= 10, `only ${res.framesPresented} frames presented, want >= 10`);
    check(res.currentTime >= 0.3, `currentTime = ${res.currentTime}, want >= 0.3 s`);
    // Dimensions come from the SPS via the muxer's init segment.
    check(
      res.videoWidth === 320 && res.videoHeight === 240,
      `video reports ${res.videoWidth}x${res.videoHeight}, want 320x240`,
    );
    if (problems.length > 0) {
      fail(`muxer-check assertions failed:\n  - ${problems.join('\n  - ')}`);
    }
    log(
      `muxer check ok: ${res.framesPresented} frames presented, currentTime ${res.currentTime.toFixed(2)} s, ` +
        `${res.segmentsAppended} segments appended (${res.mime})`,
    );
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
  // Exactly one *active* publisher. A retried browser-broadcaster attempt
  // leaves its dead predecessor's hub in the GC grace period — inactive
  // entries are expected there, never a second active one.
  const entries = Object.values(st.broadcasts ?? {});
  const active = entries.filter((b) => b.publisherActive);
  if (active.length !== 1) {
    fail(`relay shows ${active.length} active broadcasts (${entries.length} total), want 1`);
  }
  const b = active[0];
  const problems = [];
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

  // R22: the muxer check is self-contained — bundle, browser, verdict.
  if (MUXER_CHECK) {
    await muxerCheckScenario();
    log('PASS');
    return;
  }

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

    if (!BROWSER_BROADCAST) {
      const pubsim = launch('pubsim', PUBSIM_BIN, [
        '-url', relayUrl,
        '-insecure',
        '-duration', '600s',
      ]);
      id = await waitForLine(pubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the broadcast code');
      log(`pubsim publishing as ${id}`);
    }
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

  // Z5: the browser publishes. Same one-retry policy as the viewer scenario
  // (fresh browser, never the relay); a retry mints a fresh broadcast ID and
  // the predecessor's hub sits out its GC grace period, which the relay-side
  // assertion tolerates by keying on *active* publishers.
  let publisherBrowser = null;
  let expectedCodec = 'avc1.42C00D';
  if (BROWSER_BROADCAST) {
    expectedCodec = /^avc1\./;
    let res;
    try {
      res = await broadcasterScenario({ relayUrl, certHash, attempt: 1 });
    } catch (err) {
      log(`RETRY: broadcaster scenario attempt 1 failed: ${err.message ?? err}`);
      res = await broadcasterScenario({ relayUrl, certHash, attempt: 2 });
    }
    publisherBrowser = res.browser;
    id = res.id;
  }

  // Up to `retries` in-harness retries, relaunching the browser but never the
  // relay or the publisher (Decision 8). Every failed attempt is logged
  // loudly; only the last failure is fatal.
  const runViewer = async (label, retries, opts) => {
    let lastErr;
    for (let attempt = 1; attempt <= retries + 1; attempt++) {
      try {
        // The attempt tag names this pass's artifacts (console log, both
        // diagnostics JSONs, the screenshot), so the two passes never
        // overwrite each other's failure evidence.
        const tag = label ? `${label}-${attempt}` : String(attempt);
        await browserScenario({ relayUrl, certHash, id, attempt: tag, expectedCodec, ...opts });
        return;
      } catch (err) {
        lastErr = err;
        if (attempt <= retries) {
          log(`RETRY: ${label || 'browser'} scenario attempt ${attempt} failed: ${err.message ?? err}`);
        }
      }
    }
    throw lastErr;
  };

  try {
    await runViewer('', MAX_VIEWER_RETRIES, {});

    // R19 resilient mode, on the same live broadcast (PRODUCT-1): the only
    // automated exercise of the browser's carrier reader and of the
    // ?delivery=reliable negotiation end to end. Reuses the running relay,
    // publisher and preview — the marginal cost is one browser session.
    //
    // Tier 1 with the fixture publisher only: tier 2's value is the
    // origin/edge split (asserted per pod in cluster-assert.sh), and the
    // browser-broadcast step already spends its budget on the encode funnel.
    if (RESILIENT_VIEWER_PASS) {
      log('running the resilient-mode viewer pass (R19 carrier path)');
      await runViewer('resilient', MAX_RESILIENT_RETRIES, { delivery: 'resilient' });

      // R21 Deep buffer, same broadcast, one more browser session. This pass
      // exists because the freeze that shipped in R21 (docs/26 field finding
      // 1) was catchable by the assertions already here — the mode simply was
      // never run. A mode with no pass is a mode nobody tests.
      log('running the deep-buffer viewer pass (R21 DVR ring)');
      await runViewer('deep', MAX_RESILIENT_RETRIES, { delivery: 'deep' });
    }

    if (opsUrl) await assertRelaySide(opsUrl);
  } finally {
    await publisherBrowser?.close();
  }

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
