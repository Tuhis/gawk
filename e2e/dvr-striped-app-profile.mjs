// Manual tool: the production web viewer in Most stable (deep buffer) under a
// shaped downlink, with Striping seeded Auto/On/Off (R87 SG4, docs/91 §8).
//
// NOT part of any CI tier. tools/dvrlab/app-run.sh drives it in the
// Playwright image on the dvrlab network, against relay-up.sh's relay:
//
//   RELAY=https://dvrlab-relay:4433 OPS=http://dvrlab-relay:9100 ID=ABC123 \
//     STRIPE=auto SECS=300 CERT=/path/relay.pem OUT=/path/run-dir \
//     node dvr-striped-app-profile.mjs
//
// Same approach as run.mjs's deep-striped pass: vite preview serves
// gawk-app/dist, settings are seeded into localStorage before the app runs,
// and stats come from Copy diagnostics (clipboard stubbed). The relay is
// accepted by serverCertificateHashes: the lab cert (ECDSA, 7 days) qualifies,
// and its hash is taken over the DER here.
//
// Every SAMPLE_MS it captures diagnostics and the relay's /statusz and
// metrics. Diagnostics carry the last ~10 s of 500 ms samples, so the
// captures overlap and timeline.csv is the stitched 500 ms history.
// Outputs in OUT: samples.csv (one row per capture), timeline.csv,
// statusz.jsonl, metrics-before.txt / metrics-after.txt, console.log,
// diagnostics-last.json, summary.json.

import { spawn } from 'node:child_process';
import { createHash, X509Certificate } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';
import { chromium } from 'playwright-core';

const HERE = dirname(fileURLToPath(import.meta.url));
const { RELAY, OPS, ID, CERT } = process.env;
if (!RELAY || !OPS || !ID || !CERT) {
  console.error('need RELAY, OPS, ID and CERT');
  process.exit(2);
}
const STRIPE = process.env.STRIPE ?? 'auto';
const SECS = Number(process.env.SECS ?? 120);
const SAMPLE_MS = Number(process.env.SAMPLE_MS ?? 5000);
const OUT = resolve(process.env.OUT ?? 'dvr-striped-out');
const APP_DIR = resolve(HERE, process.env.APP_DIR ?? '../gawk-app');
const PORT = Number(process.env.PREVIEW_PORT ?? 4173);
const APP_URL = `http://127.0.0.1:${PORT}`;
mkdirSync(OUT, { recursive: true });

const log = (m) => console.log(`[dvr-app] ${m}`);

function chromePath() {
  if (process.env.CHROME) return process.env.CHROME;
  for (const d of readdirSync('/ms-playwright').filter((d) => /^chromium-\d+$/.test(d))) {
    for (const sub of ['chrome-linux64', 'chrome-linux']) {
      const p = join('/ms-playwright', d, sub, 'chrome');
      if (existsSync(p)) return p;
    }
  }
  throw new Error('no Chromium under /ms-playwright; set CHROME');
}

function certHashHex(pemPath) {
  return createHash('sha256').update(new X509Certificate(readFileSync(pemPath)).raw).digest('hex');
}

async function waitForHttp(url, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      // not up yet
    }
    await sleep(200);
  }
  throw new Error(`timeout waiting for ${url}`);
}

async function pollFor(fn, timeoutMs, intervalMs, what) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await sleep(intervalMs);
  }
  throw new Error(`timeout waiting for ${what}`);
}

const metricsText = async () => (await fetch(`${OPS}/metrics`)).text();
function metricSum(text, name) {
  const re = new RegExp(`^${name}(?:\\{[^}]*\\})? (\\S+)`, 'gm');
  let sum = 0;
  for (const m of text.matchAll(re)) sum += Number(m[1]);
  return sum;
}

// This broadcast's DVR sessions from /statusz, primary first then legs.
function dvrSessions(statusz) {
  const acc = [];
  const walk = (o) => {
    if (Array.isArray(o)) o.forEach(walk);
    else if (o && typeof o === 'object') {
      if (Array.isArray(o.subscriberDetails)) {
        for (const s of o.subscriberDetails) if (s.dvr) acc.push(s);
      } else {
        Object.values(o).forEach(walk);
      }
    }
  };
  walk(statusz);
  return acc.sort((a, b) => (a.stripeLeg ? 1 : 0) - (b.stripeLeg ? 1 : 0) || (a.stripeMember ?? 0) - (b.stripeMember ?? 0));
}

function rowValue(page, label) {
  return page
    .locator('dt')
    .filter({ hasText: new RegExp(`^${label}$`) })
    .locator('xpath=following-sibling::dd')
    .textContent();
}

async function captureDiagnostics(page) {
  const before = await page.evaluate(() => window.__gawkClipboard.length);
  await page.getByRole('button', { name: /Copy diagnostics|Copied/ }).click();
  await pollFor(() => page.evaluate((n) => window.__gawkClipboard.length > n, before), 5000, 100, 'diagnostics');
  return JSON.parse(await page.evaluate(() => window.__gawkClipboard.at(-1)));
}

const preview = spawn(join(APP_DIR, 'node_modules', '.bin', 'vite'),
  ['preview', '--port', String(PORT), '--strictPort', '--host', '127.0.0.1'],
  { cwd: APP_DIR, stdio: ['ignore', 'pipe', 'pipe'] });
preview.stdout.on('data', (c) => appendFileSync(join(OUT, 'preview.log'), c));
preview.stderr.on('data', (c) => appendFileSync(join(OUT, 'preview.log'), c));

const browser = await chromium.launch({ executablePath: chromePath(), headless: true, args: ['--no-sandbox'] });
let exitCode = 0;
try {
  await waitForHttp(APP_URL, 30_000);
  const certHash = certHashHex(CERT);
  log(`relay ${RELAY} id ${ID} stripe ${STRIPE} secs ${SECS} cert ${certHash.slice(0, 16)}…`);

  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  await context.addInitScript(({ serverUrl, hash, stripeMode, dvrBufferMs }) => {
    localStorage.setItem('gawk.serverUrl', serverUrl);
    localStorage.setItem('gawk.certHashHex', hash);
    localStorage.setItem('gawk:viewer-delivery', 'deep');
    localStorage.setItem('gawk:stripe-mode', stripeMode);
    window.__GAWK_CONFIG__ = { requirePublishSecret: false, termsVersion: 'e2e', relayUrl: serverUrl };
    // DVR_BUFFER_MS: the deployment's Deep buffer (runtime config dvrBufferMs);
    // the relay grants at most its -dvr-window (RELAY_ARGS in app-run.sh).
    if (dvrBufferMs) window.__GAWK_CONFIG__.dvrBufferMs = dvrBufferMs;
    localStorage.setItem('gawk:terms-accepted', 'e2e');
    window.__gawkClipboard = [];
    Object.defineProperty(Navigator.prototype, 'clipboard', {
      configurable: true,
      get: () => ({ writeText: (t) => (window.__gawkClipboard.push(t), Promise.resolve()) }),
    });
  }, { serverUrl: RELAY, hash: certHash, stripeMode: STRIPE, dvrBufferMs: Number(process.env.DVR_BUFFER_MS) || 0 });
  const page = await context.newPage();
  const consoleLog = join(OUT, 'console.log');
  writeFileSync(consoleLog, '');
  page.on('console', (m) => appendFileSync(consoleLog, `${new Date().toISOString()} [${m.type()}] ${m.text()}\n`));
  page.on('pageerror', (e) => appendFileSync(consoleLog, `${new Date().toISOString()} [pageerror] ${e}\n`));

  const mBefore = await metricsText();
  writeFileSync(join(OUT, 'metrics-before.txt'), mBefore);
  const relayResyncs0 = metricSum(mBefore, 'gawk_relay_dvr_resyncs_total');
  const relayTransitions0 = metricSum(mBefore, 'gawk_relay_stripe_transitions_total');

  const t0 = Date.now();
  await page.goto(`${APP_URL}/#/view/${ID}`);
  const overlay = page.locator('[role="dialog"][aria-label="Stream stats"]');
  await pollFor(async () => {
    if (await overlay.isVisible()) return true;
    await page.keyboard.press('Control+Alt+Shift+D');
    await sleep(300);
    return overlay.isVisible();
  }, 20_000, 500, 'the stats overlay');
  await pollFor(async () => {
    const v = (await rowValue(page, 'Completed'))?.trim();
    return /^\d+$/.test(v) && Number(v) > 0;
  }, 30_000, 1000, 'the first completed frame');
  log(`first frame after ${((Date.now() - t0) / 1000).toFixed(1)} s`);

  const cols = ['t_s', 'width', 'phase', 'conn_bytes', 'gap_resyncs', 'completed', 'decoded', 'received_fps',
    'decoder_fps', 'rendered_fps', 'dup_frames', 'late_frames', 'leg_dials', 'leg_deaths', 'dvr_capable',
    'relay_resyncs', 'relay_transitions', 'relay_sessions'];
  const csv = join(OUT, 'samples.csv');
  writeFileSync(csv, cols.join(',') + '\n');
  const timeline = new Map(); // wall ms (rounded to 100) → stats
  const rows = [];
  let diag = null;
  for (let next = Date.now(); Date.now() - t0 < SECS * 1000; ) {
    next += SAMPLE_MS;
    diag = await captureDiagnostics(page);
    const capturedAt = Date.parse(diag.capturedAt);
    const lastT = diag.samples.at(-1)?.tMs ?? 0;
    for (const s of diag.samples) {
      timeline.set(Math.round((capturedAt - (lastT - s.tMs) - t0) / 100) * 100, s.stats);
    }
    const s = diag.samples.at(-1).stats;
    const metrics = await metricsText().catch(() => '');
    const statusz = await (await fetch(`${OPS}/statusz`)).json().catch(() => null);
    const sessions = statusz ? dvrSessions(statusz) : [];
    appendFileSync(join(OUT, 'statusz.jsonl'), JSON.stringify({ t_s: (Date.now() - t0) / 1000, sessions }) + '\n');
    const row = {
      t_s: ((Date.now() - t0) / 1000).toFixed(1),
      width: s.dvrStripeWidth ?? '',
      phase: s.dvrStripePhase ?? '',
      conn_bytes: (s.connectionBytesReceived ?? []).join('|'),
      gap_resyncs: s.reorderGapResyncs ?? 0,
      completed: s.framesCompleted ?? 0,
      decoded: s.decodedFrames ?? 0,
      received_fps: (s.receivedFps ?? 0).toFixed(1),
      decoder_fps: (s.decoderFps ?? 0).toFixed(1),
      rendered_fps: (s.renderedFps ?? 0).toFixed(1),
      dup_frames: s.dvrDuplicateFrames ?? 0,
      late_frames: s.dvrLateFramesDropped ?? 0,
      leg_dials: s.stripeLegDials ?? 0,
      leg_deaths: s.stripeLegDeaths ?? 0,
      dvr_capable: s.stripeDvrCapable,
      relay_resyncs: metrics ? metricSum(metrics, 'gawk_relay_dvr_resyncs_total') - relayResyncs0 : '',
      relay_transitions: metrics ? metricSum(metrics, 'gawk_relay_stripe_transitions_total') - relayTransitions0 : '',
      relay_sessions: sessions
        .map((x) => `${x.stripeLeg ? `L${x.stripeMember}` : 'P'}:r${x.dvrResyncs ?? 0}:lag${x.dvrLagMs ?? 0}` +
          `:loss${(100 * (x.pathPacketsLost ?? 0) / Math.max(x.pathPacketsSent ?? 0, 1)).toFixed(2)}`)
        .join('|'),
    };
    rows.push(row);
    appendFileSync(csv, cols.map((c) => row[c]).join(',') + '\n');
    log(`t=${row.t_s}s ×${row.width} ${row.phase} bytes=[${row.conn_bytes}] gapResyncs=${row.gap_resyncs} ` +
      `fps rx/dec/ren=${row.received_fps}/${row.decoder_fps}/${row.rendered_fps} dup=${row.dup_frames} ` +
      `late=${row.late_frames} relayResyncs=${row.relay_resyncs} sessions=${row.relay_sessions}`);
    await sleep(Math.max(0, next - Date.now()));
  }
  writeFileSync(join(OUT, 'diagnostics-last.json'), JSON.stringify(diag, null, 2));

  const tl = [...timeline.entries()].sort((a, b) => a[0] - b[0]);
  writeFileSync(join(OUT, 'timeline.csv'), 't_s,width,phase,gap_resyncs,received_fps,decoder_fps,dup_frames,late_frames\n' +
    tl.map(([t, s]) => [(t / 1000).toFixed(1), s.dvrStripeWidth ?? '', s.dvrStripePhase ?? '', s.reorderGapResyncs ?? 0,
      (s.receivedFps ?? 0).toFixed(1), (s.decoderFps ?? 0).toFixed(1), s.dvrDuplicateFrames ?? 0,
      s.dvrLateFramesDropped ?? 0].join(',')).join('\n') + '\n');

  const mAfter = await metricsText();
  writeFileSync(join(OUT, 'metrics-after.txt'), mAfter);

  // Width transitions from the 500 ms timeline.
  const transitions = [];
  let prevW = null;
  let prevP = null;
  for (const [t, s] of tl) {
    const w = s.dvrStripeWidth ?? 1;
    const p = s.dvrStripePhase ?? '';
    if (w !== prevW || p !== prevP) transitions.push({ t_s: t / 1000, width: w, phase: p });
    prevW = w;
    prevP = p;
  }
  const engaged = tl.find(([, s]) => (s.dvrStripeWidth ?? 1) >= 2);
  const engageS = engaged ? engaged[0] / 1000 : null;
  // Counters at the first capture at or after engagement + 10 s settle, so
  // "after" is what striping holds, not the run-up to it.
  const settleS = engageS === null ? null : engageS + 10;
  const at = (tS) => rows.find((r) => Number(r.t_s) >= tS) ?? null;
  const last = rows.at(-1);
  const steady = rows.filter((r) => Number(r.t_s) >= (settleS ?? 30));
  const median = (k) => {
    const v = steady.map((r) => Number(r[k])).sort((a, b) => a - b);
    return v.length ? v[Math.floor(v.length / 2)] : null;
  };
  const base = settleS === null ? null : at(settleS);
  const summary = {
    stripe: STRIPE,
    secs: SECS,
    id: ID,
    stripeDvrCapable: last.dvr_capable,
    timeToEngageS: engageS,
    widthTransitions: transitions,
    finalWidth: Number(last.width),
    finalPhase: last.phase,
    maxWidth: Math.max(...tl.map(([, s]) => s.dvrStripeWidth ?? 1)),
    gapResyncsTotal: Number(last.gap_resyncs),
    relayResyncsTotal: metricSum(mAfter, 'gawk_relay_dvr_resyncs_total') - relayResyncs0,
    relayStripeTransitions: metricSum(mAfter, 'gawk_relay_stripe_transitions_total') - relayTransitions0,
    afterEngagePlus10s: base && {
      fromS: Number(base.t_s),
      gapResyncs: Number(last.gap_resyncs) - Number(base.gap_resyncs),
      relayResyncs: Number(last.relay_resyncs) - Number(base.relay_resyncs),
    },
    // For a run that never engaged: the same window as Auto's "after".
    after30s: (() => {
      const b = at(30);
      return b && {
        gapResyncs: Number(last.gap_resyncs) - Number(b.gap_resyncs),
        relayResyncs: Number(last.relay_resyncs) - Number(b.relay_resyncs),
      };
    })(),
    duplicates: Number(last.dup_frames),
    lateDropped: Number(last.late_frames),
    legDials: Number(last.leg_dials),
    legDeaths: Number(last.leg_deaths),
    medianFpsSteady: { received: median('received_fps'), decoder: median('decoder_fps'), rendered: median('rendered_fps') },
    connectionBytesFinal: last.conn_bytes,
    relaySessionsFinal: last.relay_sessions,
  };
  writeFileSync(join(OUT, 'summary.json'), JSON.stringify(summary, null, 2) + '\n');
  log(`summary: ${JSON.stringify(summary)}`);
  await context.close();
} catch (err) {
  console.error(`[dvr-app] FAILED: ${err.stack ?? err}`);
  exitCode = 1;
} finally {
  await browser.close();
  preview.kill('SIGTERM');
}
process.exit(exitCode);
