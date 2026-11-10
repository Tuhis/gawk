// The browser carrier with the keyframe at the head of its GOP's stream
// (docs/88 §5, option K3), a lab-only variant of gawk-app/lab/uplink-sender.lab.ts's
// carrier prototype. Not part of the app: build.sh builds it beside that page
// and sender.mjs serves it when UPLINK_SENDER_DIR points at its output.
//
// The prototype (docs/75 F18) sends each keyframe on a uni stream of its own
// and the GOP's deltas as records on a second stream. Chrome has no stream
// priority, so the two share the link; the keyframe gate (kforder=ack) holds
// the records until the keyframe stream's close() resolves, which costs a
// round trip per GOP. Here there is one stream per GOP and its first bytes are
// the keyframe, so QUIC's in-order delivery puts the keyframe first without
// waiting for anything.
//
// The relay as shipped forwards a keyframe-flagged VideoChunk without caching
// it (hub.HandleDatagram), so the keyframe goes here as VideoChunk records:
// the same bytes in the same order as a StreamFrame at the head of the stream
// would be, plus 22 bytes a chunk (about 2 % of a keyframe). That is what the
// lab can measure without a relay change; docs/88 K3 is the wire form a
// relay would ingest. The receiver logs those records as R lines, and
// analyze.py takes a keyframe whose F line declares chunks as complete when
// they have all arrived.
//
// Lifecycle: the prototype's rules (docs/75 D3: nothing of a superseded GOP
// survives — queued records shed, a blocked write abandoned and its stream
// reset; a retired stream gets RETIRE_GRACE_MS after its FIN to be
// acknowledged before it is reset), plus RU9's keyframe rule mapped onto the
// GOP stream: a keyframe produced while the current stream's keyframe has not
// been handed over waits in a single slot (a newer one replaces it) and its
// GOP's deltas are dropped, so a link slower than a keyframe per GOP cannot
// supersede every keyframe before it lands (docs/38 F-12).
//
// Log lines as the prototype's (F, K, O, C, W, D, G, S) plus KQ (a keyframe
// waited) and KW (its records handed over). K's acknowledgement time is the
// GOP stream's close() — the keyframe's own arrival is in the receiver's log.
import { webTransportInit } from '../../../gawk-app/src/transport/connection';
import { packetizeDecoderConfig, packetizeFrame, packetizeFrameWithParity, packetizeStreamKeyframe } from '../../../gawk-app/src/transport/packetizer';
import { TYPE_AUDIO_FRAME, WIRE_VERSION, encodeCarrierPrologue, encodeCarrierRecord } from '../../../gawk-app/src/transport/wire';

const q = new URLSearchParams(location.search);
const SECONDS = Number(q.get('seconds') ?? 100);
const FPS = Number(q.get('fps') ?? 60);
const PARITY = Number(q.get('parity') ?? 2);
const CC = (q.get('cc') ?? 'low-latency') as WebTransportCongestionControl;
const RETIRE_GRACE_MS = 150;
// retire=f12 (docs/88 §5.3, the recommended rule): a retired GOP stream is
// FINned and never reset for its deltas' sake — its queued records are shed,
// a write blocked on it finishes, and it is aborted only by RU9's rule (still
// unacknowledged KEYFRAME_ABORT_AGE_MS after it opened, when a newer keyframe
// arrives). A new GOP stream starts only when the current one's keyframe has
// been handed over and fewer than two GOP streams are in Chrome
// (keyframe-sender.ts's limits). Default (d3): docs/75 D3's prototype rules.
const F12 = q.get('retire') === 'f12';
const KEYFRAME_ABORT_AGE_MS = 2000;

const pending: string[] = [];
const log = (line: string): void => void pending.push(line);
async function flush(): Promise<void> {
  if (pending.length === 0) return;
  await fetch('/log', { method: 'POST', body: pending.splice(0).join('\n') + '\n' });
}

// As the prototype page: the host's CLOCK_REALTIME in ns.
function wallClock(): () => bigint {
  let sum = 0;
  let n = 0;
  const end = performance.now() + 200;
  while (performance.now() < end) {
    sum += Date.now() - (performance.timeOrigin + performance.now());
    n++;
  }
  const baseUs = BigInt(Math.round((performance.timeOrigin + sum / n + 0.5) * 1000));
  return () => baseUs * 1000n + BigInt(Math.round(performance.now() * 1e6));
}
const nowNs = wallClock();
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

const concat = (parts: Uint8Array[]): Uint8Array<ArrayBuffer> => {
  const buf = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    buf.set(p, off);
    off += p.length;
  }
  return buf;
};

interface Item {
  frame: number;
  key: boolean;
  buf: Uint8Array<ArrayBuffer>;
}

interface Carrier {
  gop: number;
  openedAt: number;
  settled: boolean;
  writer: WritableStreamDefaultWriter<Uint8Array> | null;
  opened: Promise<WritableStreamDefaultWriter<Uint8Array> | null>;
  queue: Item[];
  draining: boolean;
  done: boolean;
  keyHandedOver: boolean;
  onHandedOver: () => void;
  supersede: () => void;
  superseded: Promise<'superseded'>;
}

async function main(): Promise<void> {
  const trace = (await (await fetch('/trace')).text())
    .split('\n')
    .filter((l) => l.trim() !== '')
    .map((l) => {
      const [size, flags = ''] = l.split(',');
      return { size: Math.max(1, Number(size)), key: flags.includes('K') };
    });
  const wt = new WebTransport(q.get('url')!, { ...webTransportInit({ certHashHex: q.get('hash')! }), congestionControl: CC });
  await wt.ready;
  const mds = wt.datagrams.maxDatagramSize;
  log(`I,${mds},carrier-inline,${CC}`);
  const config = packetizeDecoderConfig('avc1.64002a', new Uint8Array(40));
  const audioWriter = wt.datagrams.writable.getWriter();
  let offered = 0;

  let latest = -1;
  let current: Carrier | null = null;
  // RU9's single pending slot: a keyframe that waits for the current one's
  // handover. Its GOP's deltas are dropped while it waits.
  let waiter: { gop: number; t: bigint; item: Item } | null = null;
  const kfProduced = new Map<number, bigint>();

  // Every GOP stream opened and not yet settled (acknowledged, failed or
  // aborted): what keyframe-sender.ts calls "in Chrome".
  const inChrome = new Set<Carrier>();
  const settle = (c: Carrier, outcome: string): void => {
    if (c.settled) return;
    c.settled = true;
    inChrome.delete(c);
    log(`K,${c.gop},${kfProduced.get(c.gop) ?? 0n},${nowNs()},${outcome}`);
    tryStart();
  };
  const retire = async (c: Carrier): Promise<void> => {
    if (c.done) return;
    c.done = true;
    const writer = await c.opened;
    if (!writer) return;
    const tFin = nowNs();
    if (F12) {
      const outcome = await writer.close().then(
        () => 'acked',
        () => 'failed',
      );
      log(`C,${c.gop},${tFin},${nowNs()},${outcome}`);
      settle(c, outcome === 'acked' ? 'sent' : c.settled ? 'aborted' : 'failed');
      return;
    }
    const closed = writer.close().then(
      () => 'acked',
      () => 'failed',
    );
    const outcome = await Promise.race([closed, sleep(RETIRE_GRACE_MS).then(() => 'expired')]);
    if (outcome === 'expired') void writer.abort().catch(() => {});
    log(`C,${c.gop},${tFin},${nowNs()},${outcome}`);
    log(`K,${c.gop},${kfProduced.get(c.gop) ?? 0n},${nowNs()},${outcome === 'acked' ? 'sent' : outcome}`);
  };
  const drain = async (c: Carrier): Promise<void> => {
    c.draining = true;
    const writer = await c.opened;
    while (c.queue.length > 0) {
      const item = c.queue.shift()!;
      if (!writer || c.done || c.gop < latest) {
        if (!item.key) log(`D,${item.frame}`);
        continue;
      }
      const t0 = nowNs();
      const r = await Promise.race([
        writer.ready.then(() => writer.write(item.buf)).then(
          () => 'ok' as const,
          () => 'err' as const,
        ),
        // A keyframe's own write is never abandoned: the next keyframe waits
        // for its handover instead (the slot above).
        item.key || F12 ? new Promise<never>(() => {}) : c.superseded,
      ]);
      const t1 = nowNs();
      if (item.key) {
        log(`KW,${item.frame},${t1}`);
        c.keyHandedOver = true;
        c.onHandedOver();
      }
      if (t1 - t0 > 1_000_000n) log(`W,${item.frame},${t0},${t1}`);
      if (r === 'superseded') {
        c.done = true;
        void writer.abort().catch(() => {});
        log(`C,${c.gop},${t1},${t1},abandoned`);
        log(`K,${c.gop},${kfProduced.get(c.gop) ?? 0n},${t1},abandoned`);
      } else if (r === 'err') {
        c.done = true;
        if (F12) settle(c, 'failed');
      }
    }
    c.draining = false;
    if (c.gop < latest) void retire(c);
  };
  const start = (gop: number, item: Item): void => {
    latest = gop;
    const old = current;
    if (old) {
      old.supersede();
      if (!old.draining) void retire(old);
    }
    let supersede = (): void => {};
    const superseded = new Promise<'superseded'>((r) => (supersede = () => r('superseded')));
    const t0 = nowNs();
    const opened = (async () => {
      const stream = await wt.createUnidirectionalStream();
      if (gop < latest) {
        void stream.abort().catch(() => {});
        log(`G,${gop}`);
        return null;
      }
      const writer = stream.getWriter();
      await writer.write(encodeCarrierPrologue());
      log(`O,${gop},${t0},${nowNs()}`);
      return writer;
    })().catch(() => null);
    const c: Carrier = {
      gop,
      openedAt: performance.now(),
      settled: false,
      writer: null,
      opened,
      queue: [item],
      draining: false,
      done: false,
      keyHandedOver: false,
      onHandedOver: () => {
        if (F12) {
          tryStart();
          return;
        }
        if (waiter && current === c) {
          const w = waiter;
          waiter = null;
          start(w.gop, w.item);
        }
      },
      supersede,
      superseded,
    };
    current = c;
    inChrome.add(c);
    void opened.then((w) => {
      c.writer = w;
      if (!w && F12) settle(c, 'failed');
    });
    void drain(c);
  };
  // F12: start the waiting keyframe once the current stream's keyframe is
  // handed over and fewer than two GOP streams are in Chrome.
  const canStart = (): boolean =>
    !current || current.done || (current.keyHandedOver && inChrome.size < 2);
  function tryStart(): void {
    if (!F12 || !waiter || !canStart()) return;
    const w = waiter;
    waiter = null;
    start(w.gop, w.item);
  }
  const produceKeyframe = (gop: number, t: bigint, item: Item): void => {
    kfProduced.set(gop, t);
    if (F12) {
      // RU9's abort check first: every stream still unacknowledged 2 s after
      // it opened is aborted, and an abort discards the waiter.
      let aborted = false;
      for (const c of [...inChrome]) {
        if (performance.now() - c.openedAt < KEYFRAME_ABORT_AGE_MS) continue;
        c.done = true;
        void c.writer?.abort().catch(() => {});
        settle(c, 'aborted');
        aborted = true;
      }
      if (aborted && waiter) {
        log(`K,${waiter.gop},${kfProduced.get(waiter.gop) ?? 0n},${nowNs()},discarded`);
        waiter = null;
      }
      if (canStart()) {
        start(gop, item);
        return;
      }
      if (waiter) log(`K,${waiter.gop},${kfProduced.get(waiter.gop) ?? 0n},${nowNs()},replaced`);
      waiter = { gop, t, item };
      log(`KQ,${gop},${nowNs()}`);
      return;
    }
    if (current && !current.keyHandedOver && !current.done) {
      if (waiter) log(`K,${waiter.gop},${kfProduced.get(waiter.gop) ?? 0n},${nowNs()},replaced`);
      waiter = { gop, t, item };
      log(`KQ,${gop},${nowNs()}`);
      return;
    }
    start(gop, item);
  };
  // Once one delta of a GOP is dropped the rest of that GOP follows it: the
  // viewer cannot decode them (RU9's rule).
  let droppingGop = -1;
  const sendRecords = (frame: number, gop: number, dgrams: Uint8Array<ArrayBuffer>[]): void => {
    const c = current;
    // Deltas of a GOP whose keyframe waits, or of a stream already retired,
    // are dropped.
    if (!c || c.done || c.gop !== gop || droppingGop === gop) {
      droppingGop = gop;
      log(`D,${frame}`);
      return;
    }
    c.queue.push({ frame, key: false, buf: concat(dgrams.map((d) => encodeCarrierRecord(d))) });
    if (!c.draining) void drain(c);
  };

  // The audio lane: as the prototype, a 20 ms Opus-sized datagram.
  const audio = new Uint8Array(180);
  audio[0] = WIRE_VERSION;
  audio[1] = TYPE_AUDIO_FRAME;
  const audioTimer = setInterval(() => {
    offered++;
    void audioWriter.write(audio).catch(() => {});
  }, 20);
  const flushTimer = setInterval(() => void flush(), 500);

  const payload = new Uint8Array(Math.max(...trace.map((t) => t.size))).fill(0x5a);
  const n = Math.floor(SECONDS * FPS);
  const t0 = performance.now() + 100;
  let gop = 0;
  for (let i = 0; i < n; i++) {
    const wait = t0 + (i * 1000) / FPS - performance.now();
    if (wait > 0) await sleep(wait);
    const { size, key } = trace[i % trace.length];
    const frameId = i;
    const timestampUs = BigInt(Math.round((i * 1e6) / FPS));
    const data = payload.subarray(0, size);
    const t = nowNs();
    if (key) {
      gop = frameId;
      // The StreamFrame's bytes (header, embedded config, payload), as
      // keyframe-flagged chunks: see the header comment.
      const msg = packetizeStreamKeyframe({ frameId, timestampUs }, config, data);
      const chunks = packetizeFrame({ frameId, keyframe: true, timestampUs }, msg, mds);
      produceKeyframe(frameId, t, { frame: frameId, key: true, buf: concat(chunks.map((d) => encodeCarrierRecord(d))) });
      log(`F,${frameId},1,${size},${chunks.length},0,${t},0`);
      continue;
    }
    const { datagrams, parity } = packetizeFrameWithParity({ frameId, keyframe: false, timestampUs }, data, PARITY, mds);
    sendRecords(frameId, gop, parity.length > 0 ? [...datagrams, ...parity] : datagrams);
    log(`F,${frameId},0,${size},${datagrams.length},${parity.length},${t},0`);
  }
  await sleep(1500);
  clearInterval(audioTimer);
  clearInterval(flushTimer);
  log(`S?,${offered}`);
  await flush();
  wt.close();
  await fetch('/done', { method: 'POST' });
}

main().catch(async (e: unknown) => {
  await fetch('/done', { method: 'POST', body: `error: ${String(e)}` });
});
