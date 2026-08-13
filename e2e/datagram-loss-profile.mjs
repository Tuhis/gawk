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
//
// R30 (docs/35 §8): GAWK_FF_STRIPE=N additionally dials this subscription as
// a STRIPED viewer — a primary plus N legs (?stripe=N&leg=j), with the 0x10
// StripeState suppression sent on the primary — and attributes every arrival
// to the connection that produced it. Expectations stay frame-global
// (chunkCount is never rewritten); each leg's share is derived as
// {d : d mod N == j}, so per-connection loss AND the `mismapped` protocol
// assertion both come from the wire alone. GAWK_FF_STRIPE unset or 0 is
// byte-identical to the original instrument. This striped mode IS R30's
// acceptance instrument (docs/35 §2): striped ~0 % on >8-chunk frames while
// an unstriped control still reproduces the threshold.

import { firefox } from 'playwright-core';

const PAGE = process.env.GAWK_FF_PAGE || 'http://localhost:5173/';
const RELAY = process.env.GAWK_FF_RELAY || 'https://api.gawk.ioio.fi:4433';
const ID = process.env.GAWK_FF_ID || 'G8FHDN';
const SECS = Number(process.env.SECS || 90);
const STRIPE = Number(process.env.GAWK_FF_STRIPE || 0);

async function runInPage({ relay, secs, stripe }) {
  const frames = new Map(); // frameId -> { count, idx:Set, par:Set, sizes:Map, at:Map(ordinal->conn) }
  // conn 0 is the primary; 1..stripe are legs (leg j dials ?stripe=N&leg=j-1).
  // docs/35 §14: a striping session set shares a viewer-minted ?owner= token —
  // the relay rejects tokenless legs (400) and ignores 0x10 on an unowned
  // primary. The unstriped control dials with no params, byte-identical.
  const owner = Array.from(crypto.getRandomValues(new Uint8Array(8)), (b) =>
    b.toString(16).padStart(2, '0'),
  ).join('');
  const conns = [];
  const connOf = (i) => {
    const url =
      stripe > 0
        ? i === 0
          ? `${relay}?owner=${owner}`
          : `${relay}?stripe=${stripe}&leg=${i - 1}&owner=${owner}`
        : relay;
    return new WebTransport(url, { requireUnreliable: true, congestionControl: 'low-latency' });
  };
  const total = stripe > 0 ? stripe + 1 : 1;
  for (let i = 0; i < total; i++) conns.push(connOf(i));
  await Promise.all(conns.map((c) => c.ready));
  let refresh = null;
  if (stripe > 0) {
    // The 0x10 suppression on the primary, and — docs/35 §14 — the same
    // bytes on every leg as its liveness-lease heartbeat: a leg silent for
    // StripeLegLease (20 s) is reaped as orphaned, and this instrument runs
    // longer than that. Level state, re-sent at 1 Hz like the client.
    const writers = conns.map((c) => c.datagrams.writable.getWriter());
    const st = new Uint8Array([0x01, 0x10, 0x01, stripe, 0x00]);
    const send = () => writers.forEach((w) => void w.write(st).catch(() => {}));
    send();
    refresh = setInterval(send, 1000);
  }
  const mismapped = [];
  const perConnGot = new Array(total).fill(0);
  const t0 = performance.now();
  const readConn = async (connIdx) => {
    const reader = conns[connIdx].datagrams.readable.getReader();
    while (performance.now() - t0 < secs * 1000) {
      const { value, done } = await reader.read();
      if (done) break;
      if (!value || value.length < 13) continue;
      const dv = new DataView(value.buffer, value.byteOffset, value.byteLength);
      const type = value[1];
      let id, ordinal, count;
      if (type === 1) {
        id = dv.getUint32(4);
        count = dv.getUint16(10);
        ordinal = dv.getUint16(8);
      } else if (type === 14) {
        id = dv.getUint32(2);
        count = dv.getUint16(7);
        ordinal = count + value[6];
      } else continue;
      let f = frames.get(id);
      if (!f) { f = { count, idx: new Set(), par: new Set(), sizes: new Map(), at: new Map() }; frames.set(id, f); }
      if (type === 1) { f.idx.add(ordinal); f.sizes.set(ordinal, value.byteLength); }
      else f.par.add(value[6]);
      f.at.set(ordinal, connIdx);
      perConnGot[connIdx]++;
      if (stripe > 0 && connIdx > 0 && ordinal % stripe !== connIdx - 1) {
        // A delta datagram on a leg whose share does not cover it: a protocol
        // violation, not loss — the mapping must be receiver-derivable.
        if (mismapped.length < 20) mismapped.push({ conn: connIdx, id, ordinal });
      }
    }
    try { reader.releaseLock(); } catch { /* fine */ }
  };
  await Promise.all(conns.map((_, i) => readConn(i)));
  if (refresh) clearInterval(refresh);
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
  // Striped attribution (R30): each leg's expectation is derived from the
  // frame-global count — |{d : d mod N == leg}| — never told by the sender.
  let perConn = null;
  let primaryDeltas = 0;
  if (stripe > 0) {
    perConn = Array.from({ length: stripe }, (_, j) => ({ leg: j, exp: 0, got: 0 }));
    for (const id of ids) {
      const f = frames.get(id);
      if (!f.count) continue;
      const pexp = Math.min(2, f.count);
      for (let d = 0; d < f.count + pexp; d++) {
        perConn[d % stripe].exp++;
      }
      for (const [ordinal, conn] of f.at) {
        if (conn === 0) primaryDeltas++;
        else if (ordinal % stripe === conn - 1) perConn[conn - 1].got++;
      }
    }
    for (const c of perConn) c.lossPct = c.exp > 0 ? +(((c.exp - c.got) / c.exp) * 100).toFixed(2) : 0;
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
    stripe, perConn, mismapped, primaryDeltas,
  };
  for (const c of conns) { try { c.close(); } catch { /* already gone */ } }
  return out;
}

const browser = await firefox.launch({ headless: true });
const page = await browser.newPage();
await page.goto(PAGE, { waitUntil: 'domcontentloaded' });
console.log(`firefox ${browser.version()} — ${SECS}s on ${ID}${STRIPE > 0 ? ` (striped ×${STRIPE})` : ''}`);
const r = await page.evaluate(runInPage, {
  relay: `${RELAY.replace(/\/$/, '')}/subscribe/${ID}`,
  secs: SECS,
  stripe: STRIPE,
});
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
if (r.stripe > 0) {
  console.log(`\nSTRIPED ×${r.stripe} — per-leg loss against the derived share (d mod N):`);
  for (const c of r.perConn) console.log(`  leg ${c.leg}  got ${c.got}/${c.exp}  loss ${c.lossPct}%`);
  console.log(`primary delta datagrams while suppressed: ${r.primaryDeltas} (in-flight overlap only — should be ~0)`);
  if (r.mismapped.length > 0) {
    console.log(`MISMAPPED (protocol violation — a leg received an ordinal outside its share):`, JSON.stringify(r.mismapped));
  } else {
    console.log('mismapped: 0 — the receiver-derived mapping held for every arrival');
  }
}
