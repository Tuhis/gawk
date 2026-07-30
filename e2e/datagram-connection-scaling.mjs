// Is the ~8-packet buffer PER-CONNECTION or SHARED?  (R29 finding 5, docs/34)
//
// That is the whole question behind "would interleaving across several
// WebTransport connections help". Interleaving splits one frame's burst N ways
// without changing aggregate traffic, so it helps if and only if the buffer
// that overflows belongs to a connection.
//
// The discriminator: open N connections to the SAME broadcast at once and watch
// per-connection loss. Each connection carries a full copy of the stream, so
// aggregate traffic is N x while per-connection burst shape is unchanged.
//
//   loss roughly CONSTANT as N grows  -> per-connection buffer. N x the traffic
//                                        through a shared bottleneck would have
//                                        hurt; it did not, so each connection
//                                        has its own headroom, and interleaving
//                                        (which REDUCES per-connection bursts)
//                                        should help.
//   loss RISES with N                 -> shared bottleneck. Interleaving moves
//                                        the same packets through the same
//                                        queue and buys nothing.

// Companion to datagram-loss-profile.mjs, which established the threshold this
// one asks about. Same constraints: needs a live broadcast, and GAWK_FF_PAGE
// must be an origin the fleet's -allowed-origins accepts.
//
//   GAWK_FF_PAGE=http://localhost:5173/ GAWK_FF_RELAY=https://relay:4433 \
//   GAWK_FF_ID=ABC123 SECS=60 node datagram-connection-scaling.mjs

import { firefox } from 'playwright-core';

const PAGE = process.env.GAWK_FF_PAGE || 'http://localhost:5173/';
const RELAY = process.env.GAWK_FF_RELAY || 'https://api.gawk.ioio.fi:4433';
const ID = process.env.GAWK_FF_ID || 'G8FHDN';
const SECS = Number(process.env.SECS || 60);

async function runInPage({ relay, n, secs }) {
  async function one() {
    const wt = new WebTransport(relay, { requireUnreliable: true, congestionControl: 'low-latency' });
    const dg = wt.datagrams;
    await wt.ready;
    return { wt, reader: dg.readable.getReader(), frames: new Map(), parityGot: 0 };
  }
  const conns = await Promise.all(Array.from({ length: n }, one));
  const t0 = performance.now();
  await Promise.all(
    conns.map(async (c) => {
      while (performance.now() - t0 < secs * 1000) {
        const { value, done } = await c.reader.read();
        if (done) break;
        if (!value || value.length < 13) continue;
        const dv = new DataView(value.buffer, value.byteOffset, value.byteLength);
        if (value[1] === 1) {
          const id = dv.getUint32(4);
          let f = c.frames.get(id);
          if (!f) { f = { count: dv.getUint16(10), got: 0 }; c.frames.set(id, f); }
          f.got++;
        } else if (value[1] === 14) {
          c.parityGot++;
        }
      }
    }),
  );
  const el = (performance.now() - t0) / 1000;
  const per = conns.map((c) => {
    const ids = [...c.frames.keys()].sort((a, b) => a - b).slice(2, -2);
    let exp = 0, got = 0, big = 0, bigExp = 0, bigGot = 0;
    for (const id of ids) {
      const f = c.frames.get(id);
      if (!f.count) continue;
      exp += f.count; got += f.got;
      // Frames past the measured threshold are where loss lives; reporting them
      // separately keeps a quiet stretch of small frames from hiding a change.
      if (f.count > 8) { big++; bigExp += f.count; bigGot += f.got; }
    }
    return {
      frames: ids.length,
      lossPct: +(((exp - got) / exp) * 100).toFixed(2),
      bigFrames: big,
      bigLossPct: bigExp ? +(((bigExp - bigGot) / bigExp) * 100).toFixed(2) : null,
      chunkRate: +(got / el).toFixed(0),
    };
  });
  for (const c of conns) { try { c.wt.close(); } catch { /* already gone */ } }
  return { n, elapsed: +el.toFixed(1), per };
}

const browser = await firefox.launch({ headless: true });
const page = await browser.newPage();
await page.goto(PAGE, { waitUntil: 'domcontentloaded' });
console.log(`firefox ${browser.version()} — ${SECS}s per level on ${ID}`);
const results = [];
for (const n of [1, 2, 4]) {
  const r = await page.evaluate(runInPage, { relay: `${RELAY.replace(/\/$/, '')}/subscribe/${ID}`, n, secs: SECS });
  results.push(r);
  const avg = (f) => (r.per.reduce((a, p) => a + f(p), 0) / r.per.length).toFixed(2);
  console.log(
    `N=${r.n}  per-conn loss ${avg((p) => p.lossPct)} %  (>8-chunk frames ${avg((p) => p.bigLossPct ?? 0)} %)` +
      `  chunks/s each ${avg((p) => p.chunkRate)}  aggregate ${(r.per.reduce((a, p) => a + p.chunkRate, 0)).toFixed(0)}/s`,
  );
  for (const p of r.per) console.log('   ', JSON.stringify(p));
}
await browser.close();

const loss = results.map((r) => r.per.reduce((a, p) => a + p.lossPct, 0) / r.per.length);
console.log('\n=== VERDICT ===');
console.log(`  per-connection loss at N=1,2,4: ${loss.map((l) => l.toFixed(2) + '%').join('  ')}`);
console.log(
  loss[2] > loss[0] * 1.6
    ? '  → rises with N: the bottleneck is SHARED. Interleaving would not help.'
    : '  → flat under N× traffic: the bottleneck looks PER-CONNECTION. Interleaving is worth pursuing.',
);
