// Manual tool: does Firefox's `incomingHighWaterMark` govern datagram dropping?
//
// R29 finding 3 (docs/34). Firefox exposes only the pre-rename attribute — the
// spec-named `incomingMaxBufferedDatagrams`, which the receive algorithm
// compares the queue against before dropping from its head, is absent — and its
// default is 1 datagram against a delta frame's burst of ~10. Setting the
// legacy name succeeds and reads back, so a readback proves storage and nothing
// more. This measures whether it proves anything else.
//
// NOT part of any CI tier. docs/25 defers Firefox, and this needs a live
// broadcast; it exists so the question is re-answerable when Firefox ships the
// real attribute, rather than being re-derived from the spec each time.
//
//   GAWK_FF_RELAY=https://relay:4433 GAWK_FF_ID=ABC123 \
//   GAWK_FF_PAGE=https://app.example  node firefox-datagram-buffer.mjs
//
// GAWK_FF_PAGE matters: the relay enforces -allowed-origins, so the probe has
// to run IN the app's origin. A localhost page is rejected before a session
// exists, and a local `-dev-cert` relay is not a way around it either —
// Firefox's serverCertificateHashes is an additional check rather than a
// replacement (Mozilla bug 1873263), so it refuses the dev cert outright.
//
// Method: two subscribers to the same broadcast, in one browser, over the same
// seconds, differing only in the high-water mark. Identical traffic, so any
// difference in what they receive is the knob. The reader is deliberately
// slowed — a depth-1 queue cannot survive a slow consumer and a deep one
// should — and an A/A round runs first to size the noise, because two sessions
// are never identical and a difference under that floor means nothing.
//
// The measurement is chunks against parity. The producer writes each frame's
// parity LAST (`broadcaster.ts`), so head-of-queue eviction sheds data chunks
// and spares parity: chunks/parity falling is the signature, and it reproduces
// on demand here whenever the reader is stalled.

import { firefox } from 'playwright-core';

const PAGE = process.env.GAWK_FF_PAGE;
const RELAY = process.env.GAWK_FF_RELAY;
const ID = process.env.GAWK_FF_ID;
if (!PAGE || !RELAY || !ID) {
  console.error('need GAWK_FF_PAGE, GAWK_FF_RELAY and GAWK_FF_ID (a live broadcast code)');
  process.exit(2);
}
const SUBSCRIBE = `${RELAY.replace(/\/$/, '')}/subscribe/${ID}`;
const SECS = Number(process.env.SECS || 20);
const DEEP = Number(process.env.DEEP || 8192);
// Stalling the reader is what makes a shallow queue matter at all: on a fast
// consumer the depth-1 queue is refilled between arrivals and the effect the
// tool is looking for never appears.
const STALL_EVERY = Number(process.env.STALL_EVERY || 25);
const STALL_MS = Number(process.env.STALL_MS || 10);

const T0 = Date.now();
const note = (m) => console.log(`[${((Date.now() - T0) / 1000).toFixed(1)}s] ${m}`);

async function runInPage({ relay, hwm, startAt, secs, stallEvery, stallMs }) {
  const wt = new WebTransport(relay, { requireUnreliable: true, congestionControl: 'low-latency' });
  const dg = wt.datagrams;
  const before = dg.incomingHighWaterMark;
  dg.incomingHighWaterMark = hwm; // before ready — the earliest a page can
  await wt.ready;
  const wait = startAt - Date.now();
  if (wait > 0) await new Promise((r) => setTimeout(r, wait));
  const counts = {};
  let total = 0;
  let stalls = 0;
  const reader = dg.readable.getReader();
  const t0 = performance.now();
  while (performance.now() - t0 < secs * 1000) {
    const { value, done } = await reader.read();
    if (done) break;
    if (!value || value.length < 2) continue;
    counts[value[1]] = (counts[value[1]] || 0) + 1;
    total++;
    if (stallEvery > 0 && total % stallEvery === 0) {
      stalls++;
      const until = performance.now() + stallMs;
      while (performance.now() < until) { /* stand in for decode + render work */ }
    }
  }
  const el = (performance.now() - t0) / 1000;
  const out = {
    hwmBefore: before,
    hwmFinal: dg.incomingHighWaterMark,
    maxDatagramSize: dg.maxDatagramSize,
    stalls,
    chunkRate: +((counts[1] || 0) / el).toFixed(1),
    parityRate: +((counts[14] || 0) / el).toFixed(1),
    chunksPerParity: counts[14] ? +((counts[1] || 0) / counts[14]).toFixed(3) : null,
  };
  try { wt.close(); } catch { /* already gone */ }
  return out;
}

const browser = await firefox.launch({ headless: true });
const opts = { secs: SECS, stallEvery: STALL_EVERY, stallMs: STALL_MS };

async function subscriber(hwm) {
  const page = await browser.newPage();
  await page.goto(PAGE, { waitUntil: 'domcontentloaded' });
  return { page, run: (startAt) => page.evaluate(runInPage, { relay: SUBSCRIBE, hwm, startAt, ...opts }) };
}

async function pair(label, hwmA, hwmB) {
  const a = await subscriber(hwmA);
  const b = await subscriber(hwmB);
  note(`${label}: sampling ${SECS}s`);
  const startAt = Date.now() + 3000;
  const [ra, rb] = await Promise.all([a.run(startAt), b.run(startAt)]);
  await a.page.close();
  await b.page.close();
  const delta = ((rb.chunkRate - ra.chunkRate) / ra.chunkRate) * 100;
  console.log(`  A hwm=${ra.hwmFinal}`, JSON.stringify(ra));
  console.log(`  B hwm=${rb.hwmFinal}`, JSON.stringify(rb));
  console.log(`  B vs A chunk rate: ${delta >= 0 ? '+' : ''}${delta.toFixed(1)} %`);
  return delta;
}

console.log(`firefox ${browser.version()} — ${SECS}s per side, stall ${STALL_MS}ms every ${STALL_EVERY}`);
const noise = await pair('A/A control (both shallow) — the noise floor', 1, 1);
const first = await pair(`A/B (1 vs ${DEEP})`, 1, DEEP);
const second = await pair(`A/B repeat (1 vs ${DEEP})`, 1, DEEP);
await browser.close();

console.log('\n=== VERDICT ===');
console.log(`  noise floor (A/A):     ${noise.toFixed(1)} %`);
console.log(`  deep buffer effect #1: ${first.toFixed(1)} %`);
console.log(`  deep buffer effect #2: ${second.toFixed(1)} %`);
// A real effect has to survive BOTH tests: the repeats must agree on a
// direction, and the size must clear the noise floor. Sign agreement is the
// load-bearing half — a difference that flips between repeats is noise however
// large it looks, and comparing magnitudes alone called +0.6 % / −0.3 % an
// effect on the very run that established there is none.
const agree = Math.sign(first) === Math.sign(second);
const big = Math.min(Math.abs(first), Math.abs(second)) > Math.abs(noise) * 2;
console.log(
  agree && big
    ? '  → consistent and above the floor: the attribute DOES affect delivery — re-check docs/34 finding 3.'
    : '  → noise (repeats disagree or sit under the floor): the attribute does NOT govern dropping here.',
);
