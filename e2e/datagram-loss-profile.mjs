// Which datagrams of a frame go missing, and why — measured from the wire.
//
// R29 finding 4 (docs/34). A VideoChunk header carries chunkIndex + chunkCount
// and a parity header carries parityIndex, so a viewer can reconstruct each
// frame's entire arrival set with no source anchor: expected is what the
// headers say, received is what arrived. That turns "is this link lossy" into
// three separate, discriminating questions answered in one run:
//
//   loss vs chunk INDEX  — uniform means ordinary loss; concentrated at low
//                          indices means something sheds the head of a burst
//   loss vs frame SIZE   — flat means per-packet loss; rising with burst length
//                          means a queue that a long burst overruns
//   parity loss          — parity is written LAST (broadcaster.ts), so a spared
//                          parity stream is the tail of the burst surviving
//
// Not wired into CI: it needs a live broadcast, and docs/25 defers Firefox.
// The relay enforces -allowed-origins, so GAWK_FF_PAGE must be an origin the
// fleet allows (a dev server on localhost:5173 qualifies on the reference
// fleet); a local -dev-cert relay is no substitute because Firefox refuses the
// dev cert outright (Mozilla bug 1873263).
//
//   GAWK_FF_PAGE=http://localhost:5173/ GAWK_FF_RELAY=https://relay:4433 \
//   GAWK_FF_ID=ABC123 SECS=100 node datagram-loss-profile.mjs

import { firefox } from 'playwright-core';

const PAGE = process.env.GAWK_FF_PAGE || 'http://localhost:5173/';
const RELAY = process.env.GAWK_FF_RELAY || 'https://api.gawk.ioio.fi:4433';
const ID = process.env.GAWK_FF_ID || 'G8FHDN';
const SECS = Number(process.env.SECS || 90);

async function runInPage({ relay, secs }) {
  const wt = new WebTransport(relay, { requireUnreliable: true, congestionControl: 'low-latency' });
  const dg = wt.datagrams;
  await wt.ready;
  const frames = new Map(); // frameId -> { count, idx:Set, par:Set, sizes:Map }
  const reader = dg.readable.getReader();
  const t0 = performance.now();
  while (performance.now() - t0 < secs * 1000) {
    const { value, done } = await reader.read();
    if (done) break;
    if (!value || value.length < 13) continue;
    const dv = new DataView(value.buffer, value.byteOffset, value.byteLength);
    const type = value[1];
    if (type === 1) {
      const id = dv.getUint32(4);
      let f = frames.get(id);
      if (!f) { f = { count: dv.getUint16(10), idx: new Set(), par: new Set(), sizes: new Map() }; frames.set(id, f); }
      f.idx.add(dv.getUint16(8));
      f.sizes.set(dv.getUint16(8), value.byteLength);
    } else if (type === 14) {
      const id = dv.getUint32(2);
      let f = frames.get(id);
      if (!f) { f = { count: dv.getUint16(7), idx: new Set(), par: new Set(), sizes: new Map() }; frames.set(id, f); }
      f.par.add(value[6]);
    }
  }
  const el = (performance.now() - t0) / 1000;
  // Trim the window edges: those frames are legitimately partial.
  const ids = [...frames.keys()].sort((a, b) => a - b).slice(2, -2);
  let expC = 0, gotC = 0, expP = 0, gotP = 0;
  const lostAt = new Map();     // absolute chunk index -> lost count
  const sentAt = new Map();     // absolute chunk index -> expected count
  let lastLost = 0, lastSent = 0, otherLost = 0, otherSent = 0;
  const sizeSeen = new Map();   // payload size -> count received
  for (const id of ids) {
    const f = frames.get(id);
    if (!f.count) continue;
    expC += f.count;
    gotC += f.idx.size;
    const pexp = Math.min(2, f.count);
    expP += pexp;
    gotP += f.par.size;
    for (let i = 0; i < f.count; i++) {
      sentAt.set(i, (sentAt.get(i) || 0) + 1);
      const isLast = i === f.count - 1;
      if (isLast) lastSent++; else otherSent++;
      if (!f.idx.has(i)) {
        lostAt.set(i, (lostAt.get(i) || 0) + 1);
        if (isLast) lastLost++; else otherLost++;
      }
    }
    for (const [, sz] of f.sizes) sizeSeen.set(sz, (sizeSeen.get(sz) || 0) + 1);
  }
  // Microburst test: if a frame's chunks go out back-to-back and overrun a
  // queue on the path, a BIGGER burst should lose proportionally more.
  const bySize = new Map(); // chunkCount -> {frames, exp, got}
  for (const id of ids) {
    const f = frames.get(id);
    if (!f.count) continue;
    const b = bySize.get(f.count) || { frames: 0, exp: 0, got: 0 };
    b.frames++; b.exp += f.count; b.got += f.idx.size;
    bySize.set(f.count, b);
  }
  const perSize = [...bySize.entries()].sort((a, b) => a[0] - b[0])
    .filter(([, b]) => b.frames >= 25)
    .map(([n, b]) => ({ n, frames: b.frames, lossPct: +(((b.exp - b.got) / b.exp) * 100).toFixed(2) }));

  const perIndex = [];
  for (const [i, sent] of [...sentAt.entries()].sort((a, b) => a[0] - b[0])) {
    if (sent < 50) continue;
    perIndex.push({ i, sent, lost: lostAt.get(i) || 0, lossPct: +(((lostAt.get(i) || 0) / sent) * 100).toFixed(2) });
  }
  const out = {
    elapsed: +el.toFixed(1),
    frames: ids.length,
    chunkLossPct: +(((expC - gotC) / expC) * 100).toFixed(2),
    parityLossPct: +(((expP - gotP) / expP) * 100).toFixed(2),
    expChunks: expC, gotChunks: gotC, expParity: expP, gotParity: gotP,
    lastChunkLossPct: +((lastLost / lastSent) * 100).toFixed(2),
    otherChunkLossPct: +((otherLost / otherSent) * 100).toFixed(2),
    perIndex,
    perSize,
    topSizes: [...sizeSeen.entries()].sort((a, b) => b[1] - a[1]).slice(0, 6),
  };
  try { wt.close(); } catch { /* already gone */ }
  return out;
}

const browser = await firefox.launch({ headless: true });
const page = await browser.newPage();
await page.goto(PAGE, { waitUntil: 'domcontentloaded' });
console.log(`firefox ${browser.version()} — ${SECS}s on ${ID}`);
const r = await page.evaluate(runInPage, { relay: `${RELAY.replace(/\/$/, '')}/subscribe/${ID}`, secs: SECS });
await browser.close();

console.log(`\nframes=${r.frames}  elapsed=${r.elapsed}s`);
console.log(`CHUNK  loss ${r.chunkLossPct}%  (${r.gotChunks}/${r.expChunks})`);
console.log(`PARITY loss ${r.parityLossPct}%  (${r.gotParity}/${r.expParity})   <- the asymmetry test`);
console.log(`last chunk of frame: ${r.lastChunkLossPct}%   every other chunk: ${r.otherChunkLossPct}%   <- size test`);
console.log('\nloss by chunk index (head-eviction test — expect a downward slope if real):');
for (const p of r.perIndex) console.log(`  idx ${String(p.i).padStart(2)}  sent ${String(p.sent).padStart(5)}  lost ${String(p.lost).padStart(4)}  ${p.lossPct}%`);
console.log('\nloss by frame size (microburst test — expect loss to RISE with burst size):');
for (const p of r.perSize) console.log(`  ${String(p.n).padStart(2)} chunks/frame  frames ${String(p.frames).padStart(4)}  ${p.lossPct}%`);
console.log('\nmost common datagram sizes (received):', JSON.stringify(r.topSizes));
