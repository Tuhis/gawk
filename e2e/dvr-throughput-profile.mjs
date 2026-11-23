// Manual tool: deep-buffer goodput per connection (R87 SG0, docs/91 §4).
//
// Opens K deep-buffer subscriptions (?delivery=reliable&buffer=) to one
// broadcast from one browser, each its own WebTransport session, and logs
// every session's cumulative bytes once a second. With K = 1 against K = 2..4
// on the same path it answers docs/91's question: does a second connection
// add throughput (a per-connection ceiling) or split it (a full link)?
//
// NOT part of any CI tier. tools/dvrlab/run.sh drives it in the Playwright
// image against the lab relay; it works against any relay the browser trusts:
//
//   RELAY=https://relay:4433 ID=ABC123 K=2 SECS=90 OUT=sessions.csv \
//     node dvr-throughput-profile.mjs
//
// Firefox only, deliberately: the field case (docs/91 §2) is Firefox. The
// lab's relay certificate comes from a local CA: PROFILE names a Firefox
// profile whose NSS store trusts it (tools/dvrlab/build.sh), and Firefox
// only speaks HTTP/3 to a server behind such a root with the pref below off.

import { firefox } from 'playwright-core';
import fs from 'node:fs';
import http from 'node:http';

const { RELAY, ID } = process.env;
if (!RELAY || !ID) {
  console.error('need RELAY and ID (a live broadcast code)');
  process.exit(2);
}
const K = Number(process.env.K || 1);
const SECS = Number(process.env.SECS || 60);
const BUFFER = Number(process.env.BUFFER || 3000);
const OUT = process.env.OUT || 'sessions.csv';
const PORT = Number(process.env.PAGE_PORT || 18765);

// WebTransport needs a secure context; http://localhost is one.
const srv = http.createServer((_, res) => {
  res.setHeader('content-type', 'text/html');
  res.end('<!doctype html><title>dvr-throughput-profile</title>');
});
await new Promise((r) => srv.listen(PORT, '127.0.0.1', r));

const prefs = { 'network.http.http3.disable_when_third_party_roots_found': false };
const ctx = process.env.PROFILE
  ? await firefox.launchPersistentContext(process.env.PROFILE, { headless: true, firefoxUserPrefs: prefs })
  : await (await firefox.launch({ headless: true, firefoxUserPrefs: prefs })).newContext();
const page = await ctx.newPage();
page.on('console', (m) => console.error('[page]', m.text()));
await page.goto(`http://localhost:${PORT}/`);
console.error(`firefox K=${K} secs=${SECS}`);

const { rows, errs } = await page.evaluate(async ({ url, k, secs }) => {
  const bytes = new Array(k).fill(0);
  const errs = new Array(k).fill('');
  const sessions = [];
  const drain = async (i, readable) => {
    const r = readable.getReader();
    for (;;) {
      const { value, done } = await r.read();
      if (done) return;
      bytes[i] += value.byteLength;
    }
  };
  for (let i = 0; i < k; i++) {
    try {
      const wt = new WebTransport(url);
      await wt.ready;
      sessions.push(wt);
      (async () => {
        const r = wt.incomingUnidirectionalStreams.getReader();
        for (;;) {
          const { value, done } = await r.read();
          if (done) return;
          drain(i, value).catch((e) => { errs[i] = String(e); });
        }
      })().catch((e) => { errs[i] = String(e); });
      drain(i, wt.datagrams.readable).catch(() => {});
    } catch (e) {
      errs[i] = String(e);
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  const rows = [];
  const t0 = performance.now();
  for (let s = 1; s <= secs; s++) {
    await new Promise((r) => setTimeout(r, t0 + s * 1000 - performance.now()));
    for (let i = 0; i < k; i++) rows.push([s, i, bytes[i]]);
  }
  for (const wt of sessions) wt.close();
  return { rows, errs };
}, { url: `${RELAY}/subscribe/${ID}?delivery=reliable&buffer=${BUFFER}`, k: K, secs: SECS });

fs.writeFileSync(OUT, 't_s,session,bytes\n' + rows.map((r) => r.join(',')).join('\n') + '\n');
if (errs.some(Boolean)) console.error('errors:', errs);
await ctx.close();
srv.close();
