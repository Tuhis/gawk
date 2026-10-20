#!/usr/bin/env node
// The uplink lab's browser sender (docs/75 OD5): the native lab sender's
// command line, run by a headless Chrome on gawk-app/lab/uplink-sender.lab.ts,
// so run.sh and matrix.py drive it unchanged (SENDER_BIN).
//
//   sender.mjs --url https://127.0.0.1:4600/lab --out send.csv --trace t.csv
//              --seconds 100 [--mode datagram|carrier] [--cc low-latency]
//
// It serves the built page (build.sh → $UPLINK_OUT/chrome-sender), the trace
// and a log sink on 127.0.0.1, waits for the receiver's certificate hash
// (run.sh: <out dir>/cert.hash, for serverCertificateHashes), runs Chrome
// with a net-log, and completes the page's last line (S) from the net-log:
// Chrome 151 exposes no WebTransport stats to the page. --cc is the
// WebTransport congestionControl hint; Chrome runs CUBIC_BYTES for every
// value (its net-log, QUIC_CONGESTION_CONTROL_CONFIGURED), which the S line
// does not record, so check it there when it matters.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import readline from 'node:readline';

const HERE = path.dirname(new URL(import.meta.url).pathname);
const OUT = process.env.UPLINK_OUT ?? path.join(HERE, '..', 'out');
const PAGE_DIR = process.env.UPLINK_SENDER_DIR ?? path.join(OUT, 'chrome-sender');

const args = { mode: 'datagram', cc: 'low-latency', seconds: '100', fps: '60', parity: '2', writes: 'await', kforder: '', maxage: '', hwm: '' };
for (let i = 2; i < process.argv.length; i += 2) {
  const k = process.argv[i].replace(/^--/, '');
  if (k === 'datagram-buffer' || k === 'id-file') continue; // native-sender knobs with no browser analogue
  args[k] = process.argv[i + 1];
}
for (const k of ['url', 'out', 'trace']) if (!args[k]) throw new Error(`--${k} is required`);
// The native sender's controller names mean nothing to a browser: anything
// but a WebTransport hint gets the shipping one.
if (!['default', 'throughput', 'low-latency'].includes(args.cc)) args.cc = 'low-latency';

function chromePath() {
  if (process.env.CHROME) return process.env.CHROME;
  const base = path.join(os.homedir(), '.cache', 'ms-playwright');
  const dirs = fs.existsSync(base) ? fs.readdirSync(base).filter((d) => /^chromium-\d+$/.test(d)).sort() : [];
  for (const d of dirs.reverse()) {
    const p = path.join(base, d, 'chrome-linux64', 'chrome');
    if (fs.existsSync(p)) return p;
  }
  throw new Error('no Chrome: set CHROME, or install Chrome for Testing with playwright');
}

async function waitFor(file, ms) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (fs.existsSync(file) && fs.statSync(file).size > 0) return fs.readFileSync(file, 'utf8').trim();
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`no ${file}`);
}

// Counts what the S line needs from a net-log, one event per line — the lab's
// own WebTransport session only (its QUIC_SESSION_WEBTRANSPORT_CLIENT_ALIVE
// event names the URL), because Chrome opens QUIC sessions of its own to
// Google's update and variations hosts during a run. Also the spacing of the
// session's packets within a burst (gaps under 5 ms).
async function netlogStats(file, port) {
  const rl = readline.createInterface({ input: fs.createReadStream(file) });
  let ids = null;
  const lab = new Set();
  const s = { sent: 0, retx: 0, bytes: 0, msg: 0, cc: '', gaps: [], sessions: 0 };
  let last = null;
  for await (const line of rl) {
    if (!ids) {
      const c = JSON.parse(line.replace(/,\s*$/, '') + '}').constants.logEventTypes;
      ids = {
        session: c.QUIC_SESSION,
        wt: c.QUIC_SESSION_WEBTRANSPORT_CLIENT_ALIVE,
        sent: c.QUIC_SESSION_PACKET_SENT,
        msg: c.QUIC_SESSION_MESSAGE_FRAME_SENT,
        cc: c.QUIC_CONGESTION_CONTROL_CONFIGURED,
      };
      continue;
    }
    if (!line.startsWith('{')) continue;
    let e;
    try {
      e = JSON.parse(line.replace(/,\s*$/, ''));
    } catch {
      continue; // the last line of a log cut at exit
    }
    if (e.type === ids.session && e.params?.host) {
      if (e.params.host !== '127.0.0.1') s.sessions++; // Chrome's own sessions to other hosts
      continue;
    }
    if (e.type === ids.wt && typeof e.params?.url === 'string' && e.params.url.includes(`//127.0.0.1:${port}/`)) {
      lab.add(e.source.id);
      continue;
    }
    if (!lab.has(e.source?.id)) continue;
    if (e.type === ids.sent && e.params) {
      s.sent++;
      s.bytes += e.params.size ?? 0;
      if (e.params.transmission_type && e.params.transmission_type !== 'NOT_RETRANSMISSION') s.retx++;
      const t = Number(e.params.sent_time_us);
      if (last !== null && t - last < 5000) s.gaps.push(t - last);
      last = t;
    } else if (e.type === ids.msg) {
      s.msg++;
    } else if (e.type === ids.cc && e.params) {
      s.cc = e.params.congestion_control_type;
    }
  }
  if (lab.size === 0) throw new Error(`no WebTransport session to 127.0.0.1:${port} in ${file}`);
  return s;
}

// CPU each kind of Chrome process used over the run, from /proc (Linux): the
// browser, its network service, the busiest renderer (the lab page's) — and
// the busiest thread of the network service and of that renderer, which says
// whether a ceiling is one saturated thread.
function chromeCpu(rootPid, wallS) {
  const kids = new Map();
  const info = new Map();
  const ticksOf = (stat) => {
    const f = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
    return { ppid: Number(f[1]), ticks: Number(f[11]) + Number(f[12]) };
  };
  for (const d of fs.readdirSync('/proc')) {
    if (!/^\d+$/.test(d)) continue;
    try {
      const { ppid, ticks } = ticksOf(fs.readFileSync(`/proc/${d}/stat`, 'utf8'));
      const cmd = fs.readFileSync(`/proc/${d}/cmdline`, 'utf8');
      info.set(Number(d), { ticks, cmd });
      if (!kids.has(ppid)) kids.set(ppid, []);
      kids.get(ppid).push(Number(d));
    } catch {
      // gone between readdir and read
    }
  }
  const pct = (ticks) => ticks / wallS; // CLK_TCK is 100 on Linux: ticks / 100 / s × 100 %
  const busiestThread = (pid) => {
    let best = { pct: 0, name: '' };
    try {
      for (const t of fs.readdirSync(`/proc/${pid}/task`)) {
        const st = fs.readFileSync(`/proc/${pid}/task/${t}/stat`, 'utf8');
        const p = pct(ticksOf(st).ticks);
        if (p > best.pct) best = { pct: p, name: st.slice(st.indexOf('(') + 1, st.lastIndexOf(')')).replace(/[ ,]/g, '_') };
      }
    } catch {
      // gone
    }
    return best;
  };
  const out = { browser: 0, network: 0, renderer: 0, gpu: 0, rendererPid: 0, networkPid: 0 };
  const walk = (pid) => {
    const p = info.get(pid);
    if (p) {
      const kind = p.cmd.includes('network.mojom.NetworkService') ? 'network'
        : p.cmd.includes('--type=renderer') ? 'renderer'
        : p.cmd.includes('--type=gpu-process') ? 'gpu'
        : p.cmd.includes('--type=') ? null : 'browser';
      if (kind === 'renderer') {
        if (pct(p.ticks) > out.renderer) Object.assign(out, { renderer: pct(p.ticks), rendererPid: pid });
      } else if (kind === 'network') {
        out.network += pct(p.ticks);
        out.networkPid = pid;
      } else if (kind) {
        out[kind] += pct(p.ticks);
      }
    }
    for (const k of kids.get(pid) ?? []) walk(k);
  };
  walk(rootPid);
  out.networkThread = busiestThread(out.networkPid);
  out.rendererThread = busiestThread(out.rendererPid);
  return out;
}

const dir = path.dirname(path.resolve(args.out));
const hash = await waitFor(path.join(dir, 'cert.hash'), 10000);
const log = fs.createWriteStream(args.out);
let finish;
const finished = new Promise((r) => (finish = r));
const types = { '.html': 'text/html', '.js': 'text/javascript' };
const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x');
  if (req.method === 'POST') {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (u.pathname === '/log') log.write(body);
      else if (u.pathname === '/done') finish(body);
      res.end();
    });
    return;
  }
  // Cross-origin isolated, so performance.now() is precise to 5 µs, not 100.
  res.setHeader('Cross-Origin-Opener-Policy', 'same-origin');
  res.setHeader('Cross-Origin-Embedder-Policy', 'require-corp');
  if (u.pathname === '/trace') return res.end(fs.readFileSync(args.trace));
  const file = path.join(PAGE_DIR, path.normalize(u.pathname).replace(/^(\.\.[/\\])+/, ''));
  if (!file.startsWith(PAGE_DIR) || !fs.existsSync(file)) {
    res.statusCode = 404;
    return res.end();
  }
  res.setHeader('Content-Type', types[path.extname(file)] ?? 'application/octet-stream');
  res.end(fs.readFileSync(file));
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const qs = new URLSearchParams({ url: args.url, hash, mode: args.mode, cc: args.cc, seconds: args.seconds, fps: args.fps, parity: args.parity, writes: args.writes, kforder: args.kforder, maxage: args.maxage, hwm: args.hwm });
const netlog = path.join(dir, 'netlog.json');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'uplinklab-chrome-'));
const chrome = spawn(
  chromePath(),
  [
    '--headless=new',
    '--no-first-run',
    '--no-sandbox',
    '--disable-background-timer-throttling',
    '--disable-renderer-backgrounding',
    '--disable-background-networking',
    '--disable-component-update',
    `--user-data-dir=${profile}`,
    `--log-net-log=${netlog}`,
    `http://127.0.0.1:${server.address().port}/lab/uplink-sender.html?${qs}`,
  ],
  { stdio: ['ignore', 'ignore', 'ignore'] },
);
const t0 = Date.now();
const timeout = setTimeout(() => finish('error: timeout'), (Number(args.seconds) + 60) * 1000);
const result = await finished;
clearTimeout(timeout);
const cpu = chromeCpu(chrome.pid, (Date.now() - t0) / 1000);
await new Promise((r) => {
  chrome.once('exit', r);
  chrome.kill('SIGTERM');
});
server.close();
fs.rmSync(profile, { recursive: true, force: true });
await new Promise((r) => log.end(r));
if (result) {
  console.error(`browser sender: ${result}`);
  process.exit(1);
}

// S: rtt and cwnd are not logged by Chrome (0); "lost" is the packets Chrome
// sent as retransmissions; datagram frames sent come from the net-log, the
// datagrams offered from the page.
const st = await netlogStats(netlog, Number(new URL(args.url).port));
const lines = fs.readFileSync(args.out, 'utf8').split('\n');
const i = lines.findIndex((l) => l.startsWith('S?,'));
const offered = i >= 0 ? Number(lines[i].split(',')[1]) : 0;
const s = `S,0,0,${st.sent},${st.retx},0,0,${st.msg},${offered},${st.bytes}`;
if (i >= 0) lines[i] = s;
else lines.push(s);
const pc = (xs, p) => (xs.length ? xs.sort((a, b) => a - b)[Math.floor((xs.length - 1) * p)] : 0);
lines.splice(
  lines.length - 1,
  0,
  `I,cc,${st.cc}`,
  `I,gap_us,${pc(st.gaps, 0.1)},${pc(st.gaps, 0.5)},${pc(st.gaps, 0.9)},${st.gaps.length}`,
  `I,cpu,${cpu.browser.toFixed(1)},${cpu.network.toFixed(1)},${cpu.renderer.toFixed(1)},${cpu.gpu.toFixed(1)}`,
  `I,thread,${cpu.networkThread.pct.toFixed(1)},${cpu.networkThread.name},${cpu.rendererThread.pct.toFixed(1)},${cpu.rendererThread.name}`,
  `I,other_quic_sessions,${st.sessions}`,
);
fs.writeFileSync(args.out, lines.join('\n'));
if (!process.env.KEEP_NETLOG) fs.rmSync(netlog, { force: true });
console.error(`browser sender: done, ${st.cc}, ${st.sent} packets, ${st.msg} datagram frames of ${offered} offered`);
