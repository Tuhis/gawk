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
import dgram from 'node:dgram';
import { appendFileSync, existsSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
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
// R28 (docs/33): the telemetry pass. Off by default — it starts a fourth
// process and spends ~20 s of dwell waiting for a real batch to flush, so it
// is its own CI step rather than a tax on every viewer run.
const TELEMETRY_CHECK = process.argv.includes('--telemetry');

const RELAY_PORT = Number(process.env.GAWK_E2E_RELAY_PORT ?? 4433);
const OPS_PORT = Number(process.env.GAWK_E2E_OPS_PORT ?? 2112);
const TM_INGEST_PORT = Number(process.env.GAWK_E2E_TM_INGEST_PORT ?? 8090);
const TM_READ_PORT = Number(process.env.GAWK_E2E_TM_READ_PORT ?? 8091);
const PREVIEW_PORT = Number(process.env.GAWK_E2E_PREVIEW_PORT ?? 4173);
const APP_DIR = resolve(HERE, process.env.GAWK_E2E_APP_DIR ?? '../gawk-app');
const SERVER_BIN = resolve(HERE, process.env.GAWK_E2E_SERVER_BIN ?? 'bin/gawk-server');
const PUBSIM_BIN = resolve(HERE, process.env.GAWK_E2E_PUBSIM_BIN ?? 'bin/gawk-pubsim');
const TELEMETRY_BIN = resolve(HERE, process.env.GAWK_E2E_TELEMETRY_BIN ?? 'bin/gawk-telemetry');
const APP_URL = `http://127.0.0.1:${PREVIEW_PORT}`;
// ~5 s between the two diagnostics captures (Decision 6: "sustained").
const SAMPLE_GAP_MS = 5000;

// R28: the collector flushes every 10 s (FLUSH_INTERVAL_MS in
// gawk-app/src/lib/telemetry.ts), so a pass that watched for less than that
// would assert on a batch that was never due. 25 s buys two flushes — one to
// prove the periodic path, one so a single unlucky one is not the whole
// result.
const TELEMETRY_DWELL_MS = 25_000;
// A fixed, obviously-fake fleet key. It is a TEST key in a loopback harness;
// the point it proves is that both ends derive the same tokens from the same
// value, which any 32 bytes shows equally well.
const TELEMETRY_KEY = 'e2e'.padEnd(64, '0');
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
// R25 (docs/28 NA7): the audio pass. A *second* publisher rather than a flag
// on the first one, deliberately — tier-1's standing assertion is that the
// no-audio path stays intact, and that assertion only means something while a
// video-only broadcast is still running beside this one. Same tier-1-only
// budget rule as the resilient pass.
const AUDIO_VIEWER_PASS = !EXTERNAL && !BROWSER_BROADCAST;
// R29 (docs/34 FP8): the parity pass, and the only tier-1 step that puts the
// browser behind a link that actually loses packets. Everything else here runs
// over a zero-loss loopback, which is exactly the blind spot docs/24 finding 10
// found in R19 — a feature whose entire purpose is loss recovery, with no test
// that loses anything.
const PARITY_VIEWER_PASS = !EXTERNAL && !BROWSER_BROADCAST;
// 5 %, above the ~3 % measured in docs/34 §1: high enough that a 20 s pass
// reliably produces recoveries on a fixture whose deltas are small (few chunks
// per frame means fewer chances to lose one), low enough to stay far from the
// congestion collapse that would turn recovery into a timeout.
const PARITY_LOSS_PERCENT = 5;
// R30 (docs/35 §12): the striped pass, on its own broadcast — the default
// fixture's ~2–4-chunk deltas sit under one stripe share, so the controller
// correctly engages nothing against it (the designed §5.4 hold). A second
// publisher with `-fixture large` (deltas median 15 chunks) is what makes
// engagement deterministic. Same tier-1-only budget rule as the passes above.
const STRIPE_VIEWER_PASS = !EXTERNAL && !BROWSER_BROADCAST;
// Tier 2 (docs/35 §5.7): in EXTERNAL mode the harness owns no publisher, so
// striping is driven by env instead — the workflow points GAWK_E2E_ID at a
// large-frame broadcast and sets GAWK_E2E_STRIPE=on, and the MAIN pass then
// runs striped. That is the cross-pod claim's first meeting with real
// kube-proxy hashing: the primary and each leg land on whichever pod
// conntrack picks, and the per-leg static filter must compose with the edge
// pull with no coordination at all.
const STRIPE_SEED = process.env.GAWK_E2E_STRIPE || null;
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

// Same row, its tooltip: where a value too long for the 320 px panel keeps its
// full form (R16's gate detail, R28's 24-hex session id).
function rowTitle(page, label) {
  return page
    .locator('dt')
    .filter({ hasText: new RegExp(`^${label}$`) })
    .locator('xpath=following-sibling::dd')
    .getAttribute('title');
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

// R29 FP8: a userspace UDP forwarder that drops a percentage of the packets
// travelling relay → browser. The Go mirror of this lives in
// gawk-server/internal/transport/resilient_loss_test.go; this is the same
// model in Node so the BROWSER path gets exercised under loss too, not only
// the Go client.
//
// One-way on purpose. The client → relay direction is never touched: QUIC
// acknowledgements have to get through for retransmission and congestion
// control to behave, and a lossy downlink with a clean uplink is the shape of
// the mobile links this feature targets (a viewer's uplink carries almost
// nothing).
//
// tc/netem is not an option — CI runners are unprivileged containers with no
// NET_ADMIN — and this is a few dozen lines in-process.
function startLossyLink(relayPort, lossPercent) {
  const front = dgram.createSocket('udp4');
  const peers = new Map(); // "ip:port" of the client → its upstream socket
  let seed = 0x2545f491;
  const rnd = () => {
    // xorshift: deterministic per run and not Math.random(), so a failure is
    // reproducible from the logs rather than a coin flip nobody can re-throw.
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return (seed >>> 0) / 0xffffffff;
  };

  front.on('message', (msg, rinfo) => {
    const key = `${rinfo.address}:${rinfo.port}`;
    let up = peers.get(key);
    if (!up) {
      up = dgram.createSocket('udp4');
      // The lossy direction. Dropping here rather than on the uplink is what
      // makes every absence at the viewer attributable to this link.
      up.on('message', (reply) => {
        if (rnd() * 100 < lossPercent) return;
        front.send(reply, rinfo.port, rinfo.address);
      });
      peers.set(key, up);
    }
    up.send(msg, relayPort, '127.0.0.1');
  });

  return new Promise((resolveLink) => {
    front.bind(0, '127.0.0.1', () => {
      resolveLink({
        port: front.address().port,
        close: () => {
          for (const up of peers.values()) up.close();
          peers.clear();
          front.close();
        },
      });
    });
  });
}

// R29 FP8: what parity actually bought this viewer, from its own diagnostics.
//
// Flow-shaped like every other assertion here (Decision 6): counters that must
// move, never a recovery RATE — the loss is stochastic and a rate threshold on
// a 2-core runner is a flake generator. The claim under test is "parity was
// served, and it repaired frames that would otherwise have been dropped",
// which is a counter question.
function assertParityFlow(d1, d2, { expectParity }) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };
  const s1 = latest(d1);
  const s2 = latest(d2);

  if (!expectParity) {
    // The opt-down control. Its value is that it proves the relay's
    // per-subscriber filter under a real browser session — the same claim the
    // Go test makes with a synthetic client.
    check(
      (s2.parityChunksReceived ?? 0) === 0,
      `parityChunksReceived = ${s2.parityChunksReceived}, want 0 for a ?parity=0 viewer`,
    );
    check(
      (s2.framesRecoveredByParity ?? 0) === 0,
      `framesRecoveredByParity = ${s2.framesRecoveredByParity}, want 0 without parity`,
    );
    if (problems.length > 0) {
      fail(`parity opt-down assertions failed:\n  - ${problems.join('\n  - ')}`);
    }
    log(
      `parity control: incomplete drops ${s1.framesDroppedIncomplete} → ${s2.framesDroppedIncomplete} ` +
        `over the capture window, with no parity served`,
    );
    return;
  }

  check(s2.parityChunksReceived > 0, `parityChunksReceived = ${s2.parityChunksReceived}, want > 0`);
  // Between the captures the producer must still be emitting: a stream that
  // delivered symbols once and stopped would satisfy the counter above.
  check(
    s2.parityChunksReceived - s1.parityChunksReceived > 0,
    `no parity chunks between the captures (${s1.parityChunksReceived} → ${s2.parityChunksReceived})`,
  );
  // The headline: on a link dropping PARITY_LOSS_PERCENT of the relay's
  // packets, at least one frame came back that would otherwise have been
  // dropped incomplete. This is the assertion the whole item exists for, and
  // it is the one that would have caught a silently-degraded parity path.
  check(
    s2.framesRecoveredByParity > 0,
    `framesRecoveredByParity = ${s2.framesRecoveredByParity}, want > 0 under ${PARITY_LOSS_PERCENT}% injected loss`,
  );

  if (problems.length > 0) {
    fail(`parity assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `parity: ${s2.parityChunksReceived} symbols received, ${s2.framesRecoveredByParity} frames recovered, ` +
      `${s2.parityRecoveryFailures ?? 0} beyond repair, ${s2.framesSkippedWithinAllowance ?? 0} skipped within allowance`,
  );
}

// R30 (docs/35 §12 deviation 1, revised): the striped viewer pass, against
// the large-frame fixture whose deltas all exceed the burst threshold. This
// is the browser half the Go burst test cannot cover: the capability arriving
// on a real 0x0F stream, the controller sizing from the shipped reassembler's
// accounting, setStripe crossing two worker hops, real leg dials against the
// real relay, the 0x10 suppression, and the legs' shares merging back through
// the production pipeline. Flow-shaped and counter-based like every assertion
// here — the burst-buffer PHYSICS stay the Go test's job (frames here ride a
// zero-loss loopback), and the real Firefox buffer stays ST1's (docs/35 §13).
function assertStripeFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };
  const s1 = latest(d1);
  const s2 = latest(d2);

  check(s2.stripeCapable === true, `stripeCapable = ${s2.stripeCapable}, want true (0x0F bit not seen)`);
  check(s2.stripeMode === 'on', `stripeMode = ${s2.stripeMode}, want the seeded 'on'`);
  // The large fixture's deltas are median 15 chunks + 2 parity → the
  // controller must size to at least 2 legs (3 expected; ≥2 asserted so a
  // re-encoded fixture at the margin degrades this to a looser pass rather
  // than a flake).
  check(s2.stripeNeeded >= 2, `stripeNeeded = ${s2.stripeNeeded}, want >= 2 on the large fixture`);
  check(s2.stripeActive >= 2, `stripeActive = ${s2.stripeActive}, want >= 2 (legs engaged)`);
  check((s2.stripeLegDeaths ?? 0) === 0, `stripeLegDeaths = ${s2.stripeLegDeaths}, want 0 on a loopback`);
  // Frames must keep completing THROUGH the stripe — arriving on legs,
  // merging in the reassembler — not merely before it engaged.
  check(
    s2.framesCompleted - s1.framesCompleted > 0,
    `no frames completed between the captures (${s1.framesCompleted} → ${s2.framesCompleted}) while striped`,
  );
  // Loss-shaped counters on a clean loopback: engagement itself must not
  // manufacture holes. A bounded few incompletes are runner weather (the
  // UDP-buffer caps); a stream of them is the split dropping shares.
  const incompletes = (s2.framesDroppedIncomplete ?? 0) - (s1.framesDroppedIncomplete ?? 0);
  check(
    incompletes <= 3,
    `framesDroppedIncomplete rose by ${incompletes} between captures — striping is losing shares`,
  );

  if (problems.length > 0) {
    fail(`stripe assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `stripe: ${s2.stripeActive} legs active (needed ${s2.stripeNeeded}), ` +
      `${s2.stripeLegDials} dials, ${s2.stripeLegDeaths ?? 0} deaths, ` +
      `${s2.framesCompleted - s1.framesCompleted} frames completed striped, ` +
      `${s2.duplicateChunks ?? 0} dup chunks (transition overlap)`,
  );
}

// A context with the persisted transport settings seeded (the keys
// useTransportStore owns), the clipboard stubbed (the ViewerScreen.test.tsx
// precedent), and the publish-secret prompt disabled (loopback hosts count as
// dev environments, where the prompt defaults on) — all before any app code
// runs. `resilient` seeds R19's persisted toggle, which is the only way in:
// the mode is negotiated at connect, so it has to be set before the app runs.
async function newAppContext(browser, { relayUrl, certHash, delivery = null, telemetryUrl = null, parity = null, stripe = null }) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  await context.addInitScript(
    ({ serverUrl, hash, deliveryMode, tmUrl, parityLevel, stripeMode }) => {
      // R37 (docs/40): the test relay is configured the way a real
      // deployment's is — config.relayUrl — so the app resolves it as the
      // pinned DEFAULT server rather than a migrated custom entry. (The
      // interim D15 suppression guard this originally worked around was
      // reverted — review R3-A — but this seeding stays right on its own:
      // it models a real deployment's config shape.) The legacy keys stay
      // seeded on purpose: every run also exercises the R37 migration
      // folding them into the default's credentials record (cert hash
      // included).
      localStorage.setItem('gawk.serverUrl', serverUrl);
      if (hash) localStorage.setItem('gawk.certHashHex', hash);
      // R19/R21: the persisted delivery choice is the only way in — the mode
      // is negotiated at connect, so it has to be set before the app runs.
      if (deliveryMode) localStorage.setItem('gawk:viewer-delivery', deliveryMode);
      // R29 (docs/34 §5.2): the persisted opt-DOWN, seeded the same way and
      // for the same reason — parity is negotiated at connect. Absent means
      // the fleet default, which is what the protected pass wants.
      if (parityLevel != null) localStorage.setItem('gawk:parity-level', String(parityLevel));
      // R30 (docs/35 §5.5): 'on' skips the loss detector (a zero-loss
      // loopback would never fire it) and engages from frame size alone,
      // which is what makes the striped pass deterministic. Seeded rather
      // than clicked for the same reason as delivery: it must be live
      // before the first sizing window fills.
      if (stripeMode) localStorage.setItem('gawk:stripe-mode', stripeMode);
      // The shipped public/config.js assigns nothing, so this seed survives.
      // R23: pin the terms version and pre-accept it so the broadcaster's
      // one-time acknowledgment modal never gates "Start a stream" — the same
      // move as requirePublishSecret:false above (skip a pre-start modal the
      // gate has its own unit coverage for). Pinning the version keeps this
      // robust when the bundled version bumps. Viewers are never gated.
      window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e', relayUrl: serverUrl };
      // R28: the split-origin override (docs/33 D1). Only set for the
      // telemetry pass — every other pass leaves it unset, which is what makes
      // the zero-request assertion below a real check rather than a tautology.
      if (tmUrl) window.__GAWK_CONFIG__.telemetryUrl = tmUrl;
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
    { serverUrl: relayUrl, hash: certHash, deliveryMode: delivery, tmUrl: telemetryUrl, parityLevel: parity, stripeMode: stripe },
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

// R25 (docs/28 NA7): the native broadcaster's audio lane, from the viewer's
// end. gawk-pubsim -audio drives the *real* engine send path — seq stamping,
// the 1 Hz config cadence, the wire encoding — so this covers what no Go test
// can: that a browser configures its decoder from our AudioConfig and decodes
// what follows.
//
// The kill criterion NA7 pre-registered is implemented here rather than
// argued about later: if headless Chrome cannot drive an AudioWorklet, the
// sink rows go unasserted and the harness SAYS SO (docs/25's no-silent-caps
// rule). The decode counters are worker-side and need no audio device, so they
// are asserted unconditionally — degrade the assertion, never drop the pass.
function assertAudioFlow(d1, d2) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };
  const s1 = latest(d1);
  const s2 = latest(d2);

  check(s2.audioState === 'active', `audioState = ${s2.audioState}, want "active"`);
  // The config the native lane advertises, as the viewer parsed it. A wrong
  // channel count here is docs/28 Decision 10's failure mode arriving three
  // layers from its cause — the difference being that the sender now refuses
  // to produce it, so seeing it would mean the check itself regressed.
  check(s2.audioCodec === 'opus', `audioCodec = ${s2.audioCodec}, want "opus"`);
  check(s2.audioSampleRate === 48000, `audioSampleRate = ${s2.audioSampleRate}, want 48000`);
  check(s2.audioChannels === 2, `audioChannels = ${s2.audioChannels}, want 2`);

  const received = s2.audioPacketsReceived - s1.audioPacketsReceived;
  const decoded = s2.audioPacketsDecoded - s1.audioPacketsDecoded;
  check(received > 0, `no audio packets arrived between the captures (${s1.audioPacketsReceived} → ${s2.audioPacketsReceived})`);
  check(decoded > 0, `no audio packets decoded between the captures (${s1.audioPacketsDecoded} → ${s2.audioPacketsDecoded})`);
  // Flow-shaped, like every other assertion here: a ratio, not a rate. A
  // contended 2-core runner may decode a few late, but a decoder that has
  // fallen away from arrivals is the failure this pass exists to catch.
  check(
    decoded >= received * 0.8,
    `decoded ${decoded} of ${received} arriving audio packets — the decoder is not keeping up`,
  );

  const sink = s2.audioBuffer;
  if (!sink) {
    log(
      'audio sink rows UNASSERTED: this browser exposed no AudioWorklet sink (no audio device). ' +
        'The decode counters above are asserted; overflowDrops/gapsSkipped are not (NA7 kill criterion).',
    );
  } else {
    const overflow = sink.overflowDrops - (s1.audioBuffer?.overflowDrops ?? 0);
    const skipped = sink.gapsSkipped - (s1.audioBuffer?.gapsSkipped ?? 0);
    // docs/20 finding 8's latch: overflow drops climbing *with* concealments
    // is the signature, and on a loopback link with a metronomic 20 ms
    // producer neither should move at all.
    check(overflow <= received * 0.05, `${overflow} audio overflow drops in ${received} packets (docs/20 finding 8's latch)`);
    check(skipped <= received * 0.05, `${skipped} audio gaps skipped in ${received} packets`);
  }

  if (problems.length > 0) {
    fail(`audio assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `audio ok: ${received} packets arrived, ${decoded} decoded, ` +
      `${s2.audioCodec} ${s2.audioSampleRate} Hz ${s2.audioChannels} ch` +
      (sink ? `, sink buffered ${sink.bufferedMs} ms` : ', sink rows unasserted'),
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

async function browserScenario({
  relayUrl, certHash, id, attempt, expectedCodec, delivery = null, telemetryUrl = null,
  dwellMs = 0, duringDwell = null, expectAudio = false, parity = null, expectParity = null,
  stripe = null, expectStripe = false,
}) {
  const browser = await launchBrowser();
  try {
    const context = await newAppContext(browser, { relayUrl, certHash, delivery, telemetryUrl, parity, stripe });
    const page = await context.newPage();
    wirePageLogs(page, `console-${attempt}`);

    // R28 (docs/33 D12): "default off" is a RUNTIME claim, not only a
    // chart-render one. Without a fleet telemetry key the relay sends no
    // hello, so a viewer must issue literally zero telemetry requests — and
    // every pass except the telemetry one runs exactly that way. Recording it
    // here makes the guarantee a standing assertion on every PR instead of a
    // property nobody re-checks.
    const telemetryRequests = [];
    page.on('request', (req) => {
      const url = req.url();
      if (url.includes('/api/telemetry/') || url.includes('/v1/ingest') ||
          url.includes(`:${TM_INGEST_PORT}`)) {
        telemetryRequests.push(`${req.method()} ${url}`);
      }
    });

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
    if (expectAudio) assertAudioFlow(diag1, diag2);
    if (expectParity != null) assertParityFlow(diag1, diag2, { expectParity });
    if (expectStripe) assertStripeFlow(diag1, diag2);

    const shot = await page.locator('canvas').first().screenshot();
    writeFileSync(join(OUT, `viewer-${attempt}.png`), shot);
    assertNonBlack(shot);

    // R28: hold the session open past the collector's flush interval, then
    // navigate away so the unmount path sends its final batch. Both halves
    // matter — the periodic flush is what a long session relies on, and the
    // final one is what lets the service finalize without waiting out an idle
    // timeout.
    if (dwellMs > 0) {
      await sleep(dwellMs);
      log(`telemetry dwell complete after ${dwellMs / 1000}s; ${telemetryRequests.length} ingest request(s) observed`);
      // Anything that needs the session to still EXIST has to run here. The
      // relay's subscriberDetails is the obvious one: once the page navigates
      // away the subscriber is gone and /statusz can no longer be joined
      // against — which is how the first version of this pass managed to
      // "prove" the relay had minted no telemetry identity at all.
      if (duringDwell) await duringDwell(page);
      await page.goto('about:blank');
      await sleep(1500);
    }

    // R28: the default-off runtime guarantee. On the telemetry pass this is
    // inverted — the point there is that requests DID happen.
    if (telemetryUrl === null && telemetryRequests.length > 0) {
      fail(
        `telemetry is off (no fleet key) but the viewer made ${telemetryRequests.length} ` +
          `telemetry request(s): ${telemetryRequests.slice(0, 3).join(', ')}`,
      );
    }
    if (telemetryUrl !== null && telemetryRequests.length === 0) {
      fail('telemetry is enabled but the viewer made no ingest request at all');
    }
    return { telemetryRequests };
  } finally {
    await browser.close();
  }
}

// ---------------------------------------------------------------------------
// R28 telemetry (docs/33): the whole pipe, for real
// ---------------------------------------------------------------------------
//
// Every other R28 test runs against fakes, in-memory sinks or handler-level
// calls. This is the only thing that proves the pipe EXISTS end to end: a real
// relay mints a real 0x0D hello onto a real uni stream, a real browser parses
// it and POSTs a real batch, and a real service verifies the token and stores
// a session that diagnose() can then answer about.
//
// It is the R19 finding-10 lesson applied before it costs anything: the
// carrier path shipped with every test against fakes, so a regression
// degrading it back to lossy delivery would have shipped green.

// Captured while the viewer is still attached: once it leaves, /statusz no
// longer lists it and there is nothing left to join against.
async function captureRelayView(opsUrl) {
  const st = await (await fetch(`${opsUrl}/statusz`)).json();
  const broadcast = Object.values(st.broadcasts ?? {}).find((b) => b.publisherActive);
  return {
    sessionIds: new Set(
      (broadcast?.subscriberDetails ?? []).map((d) => d.sessionId).filter(Boolean),
    ),
    publisherSessionId: broadcast?.publisherSessionId,
    subscriberKeys: (broadcast?.subscriberDetails ?? []).map((d) => d.key).filter(Boolean),
  };
}

async function assertTelemetryPipe({ readUrl, relayView, overlaySession, id }) {
  const problems = [];
  const check = (ok, msg) => {
    if (!ok) problems.push(msg);
  };

  const relaySessionIds = relayView.sessionIds;
  check(
    relaySessionIds.size > 0,
    'no subscriberDetails[].sessionId on /statusz — the relay minted no telemetry identity',
  );
  // D2: the display key and the join key stay INDEPENDENT handles. Deriving
  // one from the other would leak part of a bearer credential into a
  // public-ish JSON endpoint.
  for (const key of relayView.subscriberKeys) {
    check(!relaySessionIds.has(key), `subscriber key ${key} is also a sessionId; they must stay independent`);
  }
  // pubsim is a Go publisher with no telemetry client of its own, but the
  // RELAY still mints it an identity — which is what makes a broadcaster
  // session joinable the day a native reporter is pointed at the service.
  check(
    typeof relayView.publisherSessionId === 'string' && relayView.publisherSessionId.length === 24,
    `publisherSessionId = ${relayView.publisherSessionId}, want a 24-hex handle`,
  );

  // docs/33 §4.13: the operator-facing half of the same join. Everything below
  // proves the datasets CAN be joined; this proves a person on a call can
  // perform the join, because the id is on their screen and it is the relay's.
  // Read off the live overlay, so it covers the whole path a unit test fakes:
  // wire 0x0D on a real uni stream → the worker hop → the collector → the row.
  check(
    typeof overlaySession?.full === 'string' && relaySessionIds.has(overlaySession.full),
    `the viewer overlay shows session ${JSON.stringify(overlaySession?.full)}, ` +
      `which is not one the relay minted ([${[...relaySessionIds].join(', ')}])`,
  );
  // And the visible value is the dashboard's own prefix — the eight characters
  // someone reads down a phone line have to match the column they will be
  // looked up in.
  if (typeof overlaySession?.full === 'string') {
    check(
      overlaySession.short === `${overlaySession.full.slice(0, 8)}…`,
      `the overlay shows ${JSON.stringify(overlaySession.short)}, ` +
        `want the dashboard's prefix ${JSON.stringify(`${overlaySession.full.slice(0, 8)}…`)}`,
    );
  }

  // The service's view. Poll: the collector flushes on its own schedule and
  // finalize runs on the sweep. pollFor is a predicate (it returns nothing),
  // so the rows are captured on the way past.
  let sessions = [];
  await pollFor(
    async () => {
      const rsp = await fetch(`${readUrl}/v1/sessions?since=1h`);
      if (!rsp.ok) return false;
      const rows = await rsp.json();
      if (!Array.isArray(rows) || rows.length === 0) return false;
      sessions = rows;
      return true;
    },
    60_000,
    2000,
    'a finalized session in the telemetry store',
  );

  const viewers = sessions.filter((r) => r.role === 'viewer');
  check(viewers.length > 0, `no viewer sessions stored (got ${JSON.stringify(sessions)})`);

  // THE JOIN — the thing R28 exists for, and the one claim no unit test can
  // make: the relay's view of a viewer and the viewer's own report are the
  // same session (docs/33 D2). Before TM1 these were two datasets with no
  // shared key at all.
  const joined = viewers.filter((r) => relaySessionIds.has(r.sessionId));
  check(
    joined.length > 0,
    `no stored session's id appears in the relay's subscriberDetails — ` +
      `relay had [${[...relaySessionIds].join(', ')}], store had ` +
      `[${viewers.map((r) => r.sessionId).join(', ')}]`,
  );

  const target = joined[0] ?? viewers[0];
  if (target) {
    // Zero PII, asserted on what is actually STORED rather than on what the
    // client intended to send (docs/33 D8).
    check(
      /^[A-Za-z]+ \d+$/.test(target.browser ?? ''),
      `browser = ${JSON.stringify(target.browser)}, want a coarse class like "Chrome 152"`,
    );
    check(
      target.relayCoverage === 'full' || target.relayCoverage === 'partial',
      `relayCoverage = ${target.relayCoverage}; the relay was scraped, so it must not be "none"`,
    );

    // diagnose() answers about a real session, and answers cheaply.
    const rsp = await fetch(`${readUrl}/v1/sessions/${target.sessionId}/diagnose`);
    check(rsp.ok, `diagnose: HTTP ${rsp.status}`);
    if (rsp.ok) {
      const raw = await rsp.text();
      const rep = JSON.parse(raw);
      writeFileSync(join(OUT, 'telemetry-diagnose.json'), raw);
      check(
        typeof rep.healthy === 'boolean' && Array.isArray(rep.passed),
        `diagnose returned no verdict shape: ${raw.slice(0, 200)}`,
      );
      // A loopback fixture stream is healthy by construction. A verdict that
      // invents problems here is the "boring case must not invent problems"
      // criterion failing (docs/33 §6.1).
      check(
        rep.healthy === true,
        `diagnose called a clean loopback session unhealthy: ${JSON.stringify(rep.findings)}`,
      );
      check(rep.passed.length > 0, 'a healthy verdict named no passed checks — its basis is missing');
      // D10's ceiling, on a real response rather than a synthetic one.
      check(
        raw.length <= 32 * 1024,
        `diagnose response is ${raw.length} bytes, over the 32 KB ceiling`,
      );
      // And it must carry no series (D6/D10).
      for (const forbidden of ['samples', 'points', 'series']) {
        check(!(forbidden in rep), `diagnose returned "${forbidden}" — it must return verdicts, not series`);
      }
    }
  }

  // The live projection sees the broadcast while it is still running.
  const liveRsp = await fetch(`${readUrl}/live`);
  check(liveRsp.ok, `live: HTTP ${liveRsp.status}`);
  if (liveRsp.ok) {
    const snap = await liveRsp.json();
    writeFileSync(join(OUT, 'telemetry-live.json'), JSON.stringify(snap, null, 2));
    check(Array.isArray(snap.live), 'live snapshot has no live array');
    check(
      (snap.live ?? []).length > 0,
      'the live projection shows no broadcasts while one is streaming',
    );
    const b = (snap.live ?? [])[0];
    if (b) {
      // Never green on absence: every session row must carry an explicit
      // freshness state, and a broadcast with viewers must show them.
      for (const sv of b.sessions ?? []) {
        check(
          ['reporting', 'stale', 'unknown'].includes(sv.clientState),
          `session ${sv.sessionId} has clientState ${JSON.stringify(sv.clientState)}`,
        );
        check(
          ['ok', 'warn', 'bad', 'unknown'].includes(sv.severity),
          `session ${sv.sessionId} has severity ${JSON.stringify(sv.severity)}`,
        );
      }
    }
  }

  // The dashboard is served, self-contained, from the same listener.
  const dashRsp = await fetch(`${readUrl}/`);
  check(dashRsp.ok, `dashboard: HTTP ${dashRsp.status}`);
  if (dashRsp.ok) {
    const html = await dashRsp.text();
    check(html.includes('<title>gawk telemetry'), 'dashboard did not serve its page');
  }

  if (problems.length > 0) {
    fail(`telemetry pipe assertions failed:\n  - ${problems.join('\n  - ')}`);
  }
  log(
    `telemetry pipe ok: ${sessions.length} stored session(s), ` +
      `${joined.length} joined to the relay's own view by sessionId; ` +
      `the viewer's own overlay named ${overlaySession?.short} (${overlaySession?.full})`,
  );
  void id;
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
    // A localhost origin, fulfilled from memory (no server): `about:blank` is
    // NOT a secure context, and WebCodecs is [SecureContext] — the audio leg
    // needs a real AudioEncoder to produce real Opus packets. localhost is
    // potentially-trustworthy, so this costs one intercepted request.
    await page.route('http://localhost/gawk-muxer-check*', (route) =>
      route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>muxer check</title>' }),
    );
    // Two passes: the tier this runtime picks on its own (Chrome → Opus), and
    // the AAC tier forced, because that is the one iOS lands on (docs/27
    // finding 4) and the only way to exercise the mp4a/esds boxes in CI.
    for (const tier of ['auto', 'aac']) {
      await runMuxerCheckPass(page, bundle, tier);
    }
  } finally {
    await browser.close();
  }
}

async function runMuxerCheckPass(page, bundle, tier) {
  {
    const suffix = tier === 'aac' ? '?aac=1' : '';
    await page.goto(`http://localhost/gawk-muxer-check${suffix}`);
    await page.addScriptTag({ path: bundle });
    const res = await page.evaluate(() => window.__gawkMuxerCheck());
    writeFileSync(join(OUT, `muxer-check${tier === 'aac' ? '-aac' : ''}.json`), JSON.stringify(res, null, 2));

    const problems = [];
    const check = (ok, msg) => {
      if (!ok) problems.push(msg);
    };
    check(res.videoError === null, `video element errored: ${res.videoError}`);
    // R22 finding 1: a live presentation must declare an infinite duration —
    // without it the native player draws a finite timeline (no LIVE badge) and
    // WebKit treats reaching the buffered end as end-of-media.
    check(res.liveDuration === true, 'the MediaSource did not accept duration = Infinity');
    check(res.durationIsInfinite === true, 'video.duration is not Infinity');
    check(/^video\/mp4; codecs="avc1\./.test(res.mime ?? ''), `mime = ${res.mime}, want an avc1 fMP4 mime`);
    check(res.initSegments === 1, `initSegments = ${res.initSegments}, want 1`);
    check(res.mediaSegments === 18, `mediaSegments = ${res.mediaSegments}, want 18 (the fixture)`);
    check(res.muxErrors === 0, `muxer counted ${res.muxErrors} errors`);
    check(res.appendErrors === 0, `presenter counted ${res.appendErrors} append errors`);
    // segmentsAppended totals both tracks; the video half is the pinned number.
    check(
      res.segmentsAppended - res.audioSegmentsAppended === 19,
      `video segments appended = ${res.segmentsAppended - res.audioSegmentsAppended} ` +
        `(total ${res.segmentsAppended}, audio ${res.audioSegmentsAppended}), want 19 (init + 18 media)`,
    );
    // The two load-bearing verdicts (docs/27 MF1): frames present and the
    // clock advances — buffered-but-black media satisfies neither.
    // The two load-bearing verdicts (docs/27 MF1): frames present and the clock
    // advances — buffered-but-black media satisfies neither. The frame FLOOR is
    // per-pass on purpose: `currentTime` advancing 0.3 s is what proves real
    // playback, while an rVFC count is display-rate-bound and Decision 6 forbids
    // rate-shaped assertions on a 2-core no-GPU runner. The second pass runs after
    // the first has already burned CPU in the same job, and measured 3 frames
    // where the first got 11 — so it asserts flow (frames happen at all) and
    // leaves the rate claim to the first pass.
    const frameFloor = tier === 'aac' ? 3 : 10;
    check(
      res.framesPresented >= frameFloor,
      `only ${res.framesPresented} frames presented, want >= ${frameFloor}`,
    );
    check(res.currentTime >= 0.3, `currentTime = ${res.currentTime}, want >= 0.3 s`);
    // Dimensions come from the SPS via the muxer's init segment.
    check(
      res.videoWidth === 320 && res.videoHeight === 240,
      `video reports ${res.videoWidth}x${res.videoHeight}, want 320x240`,
    );
    // R22 audio (docs/27 finding 2): the Opus track, where this Chrome takes
    // Opus in MP4. The assertions above already carry most of the weight — the
    // element plays only where the two tracks' buffered ranges INTERSECT, so a
    // malformed audio timeline shows up as framesPresented/currentTime failing.
    // These add the audio-specific verdicts on top.
    if (res.audioSupported) {
      check(res.audioPackets > 0, 'no Opus packets were encoded (AudioEncoder unavailable?)');
      const wantMime =
        res.audioTier === 'aac' ? 'audio/mp4; codecs="mp4a.40.2"' : 'audio/mp4; codecs="opus"';
      if (res.audioTier !== 'aac' || res.audioTranscode === 'active') {
        check(res.audioMime === wantMime, `audioMime = ${res.audioMime}, want ${wantMime}`);
      }
      // The AAC tier is the one iOS lands on (docs/27 finding 4). Chrome's AAC
      // *encoder* is platform-dependent, though — present on macOS, absent on the
      // Linux runners ("NotSupportedError: Unsupported codec type") — so where it
      // is missing this pass proves something else that matters just as much: that
      // an audio track producing NOTHING gets dropped instead of holding video
      // hostage through the buffered intersection. Video assertions still apply.
      if (res.audioTier === 'aac' && res.audioTranscode !== 'active') {
        log(
          `muxer check: AAC transcode unavailable here (${res.audioTranscode}: ` +
            `${res.audioTranscodeDetail}) — asserting the audio-drop path instead`,
        );
        check(
          res.audioSegmentsAppended === 0,
          `audio appended ${res.audioSegmentsAppended} segments with a dead transcoder`,
        );
      } else if (res.audioTier === 'aac') {
        check(
          res.audioTranscode === 'active',
          `audio transcode state = ${res.audioTranscode} (${res.audioTranscodeDetail})`,
        );
      }
      const audioRan = res.audioTier !== 'aac' || res.audioTranscode === 'active';
      if (audioRan) {
        check(res.audioTrack, 'the presenter never created the audio SourceBuffer');
        check(
          res.audioMuxHoles === 0,
          `muxer left ${res.audioMuxHoles} audio holes in a clean feed`,
        );
        check(
          res.audioSegmentsAppended >= res.audioMuxSegments,
          `audio appends (${res.audioSegmentsAppended}) < muxed audio segments (${res.audioMuxSegments})`,
        );
      }
    } else {
      // Never a silent skip: an unsupported audio container is a real finding
      // about the runtime, and it is exactly the question iOS answers too.
      log(`muxer check: audio leg SKIPPED — ${res.audioError}`);
    }
    if (problems.length > 0) {
      fail(`muxer-check (${tier}) assertions failed:\n  - ${problems.join('\n  - ')}`);
    }
    log(
      `muxer check ok: ${res.framesPresented} frames presented, currentTime ${res.currentTime.toFixed(2)} s, ` +
        `${res.segmentsAppended} segments appended (${res.mime})` +
        (res.audioSupported
          ? `, audio ${res.audioSegmentsAppended} appended (${res.audioMime}, tier ${res.audioTier})`
          : ', audio unsupported here'),
    );
  }
}

// ---------------------------------------------------------------------------
// Relay-side assertions (tier 1 / when an ops endpoint is reachable)
// ---------------------------------------------------------------------------

async function assertRelaySide(opsUrl, expectedActive = 1) {
  const rsp = await fetch(`${opsUrl}/statusz`);
  if (!rsp.ok) fail(`statusz: HTTP ${rsp.status}`);
  const st = await rsp.json();
  // An exact count of *active* publishers, not a floor. A retried
  // browser-broadcaster attempt leaves its dead predecessor's hub in the GC
  // grace period — inactive entries are expected there, an unexpected active
  // one never is. The count is a parameter because the R25 audio pass runs a
  // second publisher on purpose (docs/28 NA7).
  const entries = Object.values(st.broadcasts ?? {});
  const active = entries.filter((b) => b.publisherActive);
  if (active.length !== expectedActive) {
    fail(`relay shows ${active.length} active broadcasts (${entries.length} total), want ${expectedActive}`);
  }
  const problems = [];
  let framesRelayed = 0;
  let keyframeStreamsIn = 0;
  for (const b of active) {
    // Loopback link: loss here means our frame IDs disagree with the relay's
    // ingress window — a real bug, not network weather. Checked on *every*
    // active publisher: the audio broadcast's ingress is exactly as
    // load-bearing as the first one's, and R25 adds a whole second lane to it.
    if (b.ingressFramesLost !== 0 || b.ingressChunksLost !== 0) {
      problems.push(`ingress loss ${b.ingressFramesLost} frames / ${b.ingressChunksLost} chunks on loopback`);
    }
    if (b.badDatagrams !== 0) problems.push(`relay rejected ${b.badDatagrams} datagrams`);
    framesRelayed += b.framesRelayed;
    keyframeStreamsIn += b.keyframeStreamsIn;
  }
  if (problems.length > 0) fail(`relay-side assertions failed: ${problems.join('; ')}`);
  log(
    `relay side ok: ${framesRelayed} frames relayed, ${keyframeStreamsIn} keyframe streams in, ` +
      `zero ingress loss across ${active.length} publisher(s)`,
  );
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
    const relayArgs = [
      '-dev-cert',
      '-addr', `127.0.0.1:${RELAY_PORT}`,
      '-metrics-addr', `127.0.0.1:${OPS_PORT}`,
    ];
    // R28: the key's presence IS the feature switch. Every other pass runs
    // without it, which is what makes the zero-request assertion in
    // browserScenario a real check.
    if (TELEMETRY_CHECK) relayArgs.push('-telemetry-key', TELEMETRY_KEY);
    const relay = launch('relay', SERVER_BIN, relayArgs);
    // Decision 5: scrape the ephemeral dev cert's hash from the startup log
    // and seed it into the app's serverCertificateHashes setting.
    certHash = await waitForLine(relay, /cert_hash_hex\W*"?([0-9a-f]{64})/, 15_000, 'the dev cert hash');
    await waitForHttp(`${opsUrl}/healthz`, 15_000, 'the relay ops endpoint');

    if (TELEMETRY_CHECK) {
      if (!existsSync(TELEMETRY_BIN)) {
        fail(`${TELEMETRY_BIN} not found — build it: (cd gawk-telemetry && go build -o ../e2e/bin/gawk-telemetry ./cmd/gawk-telemetry)`);
      }
      // A fresh store per run. Sessions are permanent by design (rollups are
      // never pruned), so a re-run against a dirty directory sees the previous
      // run's sessions and fails its join assertion for a reason that has
      // nothing to do with the product — and a test that fails confusingly is
      // a test that gets ignored.
      const dataDir = join(OUT, 'telemetry-data');
      rmSync(dataDir, { recursive: true, force: true });
      mkdirSync(dataDir, { recursive: true });
      launch('telemetry', TELEMETRY_BIN, [
        '-telemetry-key', TELEMETRY_KEY,
        '-ingest-addr', `127.0.0.1:${TM_INGEST_PORT}`,
        '-read-addr', `127.0.0.1:${TM_READ_PORT}`,
        '-data-dir', dataDir,
        // Static, not the headless Service: there is one relay here and DNS
        // resolution is the cluster's problem, not this harness's.
        '-relay-addrs', `127.0.0.1:${OPS_PORT}`,
        '-scrape-interval', '2s',
        // MUST exceed the client's 10 s flush interval. A shorter idle
        // finalizes a live session between its own flushes, and every later
        // batch then arrives for an already-finalized session — which is how
        // the first run of this pass produced THREE rollup rows for one
        // viewer. Short enough to still finalize inside the run (the sweep
        // cadence follows this knob).
        '-session-idle', '15s',
        // The harness serves the app from a different port, so this exercises
        // the split-origin path D1 documents — including the preflight, which
        // is the part that would silently break.
        '-cors-origin', APP_URL,
        '-log-level', 'debug',
      ]);
      await waitForHttp(`http://127.0.0.1:${TM_INGEST_PORT}/healthz`, 15_000, 'the telemetry ingest listener');
      log(`telemetry service up: ingest :${TM_INGEST_PORT}, read :${TM_READ_PORT}`);
    }

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

  // Set once the R25 audio publisher is up: the relay-side assertion counts
  // active publishers exactly, and this pass deliberately runs a second one.
  let audioPublisherLive = false;
  // Same bookkeeping for the R30 large-frame publisher (a third).
  let stripePublisherLive = false;

  try {
    if (TELEMETRY_CHECK) {
      // One viewer, held long enough for the collector's 10 s flush to fire
      // twice. The dwell is the whole cost of this pass and it is unavoidable:
      // a shorter one would assert on a batch that was never due.
      log(`running the telemetry viewer pass (dwell ${TELEMETRY_DWELL_MS / 1000}s for two flushes)`);
      let relayView = null;
      let overlaySession = null;
      await runViewer('telemetry', MAX_RESILIENT_RETRIES, {
        telemetryUrl: `http://127.0.0.1:${TM_INGEST_PORT}/v1/ingest`,
        dwellMs: TELEMETRY_DWELL_MS,
        duringDwell: async (page) => {
          relayView = await captureRelayView(opsUrl);
          // Both halves of the §4.13 row, captured while the session is still
          // attached (same reason as the relay view above): the tooltip's full
          // 24-hex id and the eight characters actually on screen.
          overlaySession = {
            full: (await rowTitle(page, 'Session id'))?.trim() ?? null,
            short: (await rowValue(page, 'Session id'))?.trim() ?? null,
          };
        },
      });
      await assertTelemetryPipe({
        readUrl: `http://127.0.0.1:${TM_READ_PORT}`,
        relayView,
        overlaySession,
        id,
      });
      if (opsUrl) await assertRelaySide(opsUrl);
      log('PASS');
      return;
    }

    await runViewer(
      '',
      MAX_VIEWER_RETRIES,
      // The striped external run points GAWK_E2E_ID at a LARGE-fixture
      // broadcast, so the codec pin moves with it (720p = baseline level
      // 3.1) — the same override the tier-1 striped pass carries, missed
      // here on the first cluster dispatch (the tier-2 failure was exactly
      // this line's pin reading the small clip's 42C00D).
      STRIPE_SEED
        ? { stripe: STRIPE_SEED, expectStripe: STRIPE_SEED === 'on', expectedCodec: 'avc1.42C01F' }
        : {},
    );

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

    // R29 (docs/34 FP8): the parity pass, behind a link that actually loses
    // packets — the only tier-1 step that does. Two browser sessions on the
    // SAME lossy link so the opt-down control shares conditions with the
    // protected viewer rather than being a separate experiment.
    //
    // This is the browser half of the claim the Go loss test makes with a
    // synthetic client: there, recovery is computed by calling the shared wire
    // codec; here it is the shipped reassembler doing it inside a real worker.
    if (PARITY_VIEWER_PASS) {
      log(`running the parity viewer pass behind a ${PARITY_LOSS_PERCENT}% lossy link (R29)`);
      const link = await startLossyLink(RELAY_PORT, PARITY_LOSS_PERCENT);
      try {
        const lossyUrl = `https://127.0.0.1:${link.port}`;
        await runViewer('parity', MAX_RESILIENT_RETRIES, { relayUrl: lossyUrl, expectParity: true });
        // The control: same link, same loss, parity declined. Proves the
        // relay's per-subscriber filter against a real browser, and gives the
        // "what would this have cost" number in the log beside it.
        await runViewer('parity-off', MAX_RESILIENT_RETRIES, {
          relayUrl: lossyUrl,
          parity: 0,
          expectParity: false,
        });
      } finally {
        link.close();
      }
    }

    // R25 (docs/28 NA7): the audio lane, on its own broadcast.
    //
    // A second publisher, launched here rather than beside the first, for two
    // reasons: the video-only broadcast above must stay video-only (that is
    // tier-1's standing no-audio assertion), and a 2-core runner should not
    // carry two publishers for the whole run to serve one pass.
    if (AUDIO_VIEWER_PASS) {
      log('running the audio viewer pass (R25 native audio lane)');
      audioPublisherLive = true;
      const audioPubsim = launch('pubsim-audio', PUBSIM_BIN, [
        '-url', relayUrl,
        '-insecure',
        '-audio',
        '-duration', '180s',
      ]);
      const audioId = await waitForLine(
        audioPubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the audio broadcast code');
      log(`pubsim publishing audio as ${audioId}`);
      await runViewer('audio', MAX_RESILIENT_RETRIES, { id: audioId, expectAudio: true });
    }

    // R30 (docs/35 §12): the striped pass, on its own large-frame broadcast.
    // A third publisher for the same reason audio runs a second: the main
    // broadcast's small deltas are what every other pass is calibrated
    // against, and striping needs deltas past the burst threshold to engage
    // at all. The base pass above doubles as this pass's control in the
    // other direction — stripe-mode 'auto' against small frames must stay
    // unstriped, which its unchanged assertions already prove.
    if (STRIPE_VIEWER_PASS) {
      log('running the striped viewer pass (R30, large-frame fixture)');
      stripePublisherLive = true;
      const stripePubsim = launch('pubsim-large', PUBSIM_BIN, [
        '-url', relayUrl,
        '-insecure',
        '-fixture', 'large',
        '-duration', '180s',
      ]);
      const largeId = await waitForLine(
        stripePubsim, /GAWK_PUBSIM_ID=([A-Z0-9]{6})/, 20_000, 'the large-frame broadcast code');
      log(`pubsim publishing large frames as ${largeId}`);
      await runViewer('striped', MAX_RESILIENT_RETRIES, {
        id: largeId,
        stripe: 'on',
        expectStripe: true,
        // The large fixture's own SPS: 720p lands baseline level 3.1 where
        // the small clip is level 1.3. Pinned exactly, like the default —
        // a regenerated clip that shifts profile/level should fail loudly
        // here, not decode as a surprise.
        expectedCodec: 'avc1.42C01F',
      });
    }

    if (opsUrl) await assertRelaySide(opsUrl, 1 + (audioPublisherLive ? 1 : 0) + (stripePublisherLive ? 1 : 0));
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
