// The browser broadcaster's uplink, over Chrome's own QUIC stack (docs/75
// OD5). Not part of the app: tools/uplinklab/browser/sender.mjs builds this
// page, serves it and drives a headless Chrome at it, and the page replays a
// frame-size trace exactly as BroadcastPipeline.handleEncoded sends encoded
// frames — the same packetizer, parity, DatagramSender and keyframe stream —
// so what differs from the native lab sender is only the QUIC stack.
//
// `mode=carrier` is a prototype of what a browser Reliable mode would be: the
// native carrier's rules (docs/75 D3) as far as Chrome's API reaches. Chrome
// 151 has no stream priority (`sendOrder`), so the keyframe stream cannot be
// put ahead of the carrier as the engine does.
//
// It logs the native lab sender's lines (F, K, O, C, W, D, G, E, S) to the
// driver, timestamped on the host's wall clock so the receiver's log shares it.
//
// Since docs/75 RU9 the datagram mode is the shipping send queue: deltas and
// audio more than a GOP old are dropped (a D line for a delta), deltas give
// way to a late keyframe, and keyframes go through the shipping
// KeyframeSender (KQ: it had to wait; K's outcome may also be aborted,
// replaced or discarded). A dropped frame counts as resolved in the Q lines.
import { DatagramSender, webTransportInit } from '../src/transport/connection';
import { KeyframeSender } from '../src/transport/keyframe-sender';
import { packetizeDecoderConfig, packetizeFrameWithParity, packetizeStreamKeyframe } from '../src/transport/packetizer';
import { TYPE_AUDIO_FRAME, WIRE_VERSION, encodeCarrierPrologue, encodeCarrierRecord } from '../src/transport/wire';

const q = new URLSearchParams(location.search);
const MODE = q.get('mode') ?? 'datagram';
const SECONDS = Number(q.get('seconds') ?? 100);
const FPS = Number(q.get('fps') ?? 60);
const PARITY = Number(q.get('parity') ?? 2);
const CC = (q.get('cc') ?? 'low-latency') as WebTransportCongestionControl;
const RETIRE_GRACE_MS = 150;
// writes=nowait: a lab-only variant that hands every datagram of a frame to
// the writer at once instead of awaiting each write as DatagramSender does.
const NOWAIT = q.get('writes') === 'nowait';
// kforder=write|ack (carrier): Chrome has no stream priority, so the GOP's
// records wait until its keyframe stream's write has resolved, or its close
// (Chrome resolves close() on acknowledgement) — the browser's substitute for
// the engine's keyframe-first priority.
const KFORDER = q.get('kforder') ?? '';
// maxage / hwm: the spec's own knobs on the outgoing datagram queue, which
// the shipping page leaves at Chrome's defaults (null, 1). Probed for the
// BUGS.md fix (docs/75 F18).
const MAXAGE = q.get('maxage') ? Number(q.get('maxage')) : null;
const HWM = q.get('hwm') ? Number(q.get('hwm')) : null;

const pending: string[] = [];
const log = (line: string): void => void pending.push(line);
async function flush(): Promise<void> {
  if (pending.length === 0) return;
  await fetch('/log', { method: 'POST', body: pending.splice(0).join('\n') + '\n' });
}

// The host's CLOCK_REALTIME in ns. performance.now() is monotonic and fine;
// timeOrigin's alignment to the wall clock is not guaranteed to the
// millisecond, so it is measured against Date.now() (whole ms, floored —
// hence the half).
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

interface Carrier {
  gop: number;
  // Resolves once the stream is open and its prologue written; null if the
  // carrier was superseded while opening or the open failed.
  opened: Promise<WritableStreamDefaultWriter<Uint8Array> | null>;
  queue: { frame: number; buf: Uint8Array<ArrayBuffer> }[];
  draining: boolean;
  done: boolean;
  supersede: () => void;
  superseded: Promise<'superseded'>;
  // Settles once the GOP's keyframe is far enough along (KFORDER).
  keyframe: Promise<unknown>;
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
  if (MAXAGE !== null) wt.datagrams.outgoingMaxAge = MAXAGE;
  if (HWM !== null) wt.datagrams.outgoingHighWaterMark = HWM;
  log(`I,${wt.datagrams.maxDatagramSize},${MODE},${CC}`);
  log(`I,queue,${wt.datagrams.outgoingMaxAge},${wt.datagrams.outgoingHighWaterMark}`);
  // What the shipping page embeds in every keyframe stream: the current
  // decoder config (an H.264 avcC of typical size here).
  const config = packetizeDecoderConfig('avc1.64002a', new Uint8Array(40));
  // The shipping keyframe state machine (datagram mode): keyed by the
  // keyframe's frameId, which doubles as its GOP number.
  const kfProduced = new Map<number, bigint>();
  const keyframes = new KeyframeSender({
    now: () => performance.now(),
    gopMs: 500,
    events: {
      waiting: (id) => log(`KQ,${id},${nowNs()}`),
      handedOver: (id) => log(`KW,${id},${nowNs()}`),
      ended: (id, outcome) => log(`K,${id},${kfProduced.get(id) ?? 0n},${nowNs()},${outcome}`),
    },
  });
  keyframes.attach(() => wt.createUnidirectionalStream());
  let gop = 0;
  const sender = NOWAIT ? null! : new DatagramSender(wt, { gopBlocked: (g) => keyframes.deltasBlocked(g) });
  const rawWriter = NOWAIT ? wt.datagrams.writable.getWriter() : null!;
  let offered = 0;
  // Where a backlog sits: frames handed to DatagramSender against frames
  // whose writes have all resolved (Q lines, once a second).
  let framesQueued = 0;
  let framesResolved = 0;

  // The carrier prototype's keyframes: one uni stream per keyframe, fire and
  // forget, as the shipping page sent them before RU9 (the datagram mode uses
  // the shipping KeyframeSender instead).
  const sendKeyframe = (id: number, msg: Uint8Array<ArrayBuffer>): { written: Promise<unknown>; acked: Promise<unknown> } => {
    const t0 = nowNs();
    let written = (): void => {};
    const writtenP = new Promise<unknown>((r) => (written = () => r(undefined)));
    const acked = (async () => {
      let outcome = 'sent';
      try {
        const stream = await wt.createUnidirectionalStream();
        const writer = stream.getWriter();
        await writer.write(msg);
        // KW: the keyframe's data is handed over (the write resolved), before
        // its close() waits for the acknowledgement (K's second time).
        log(`KW,${id},${nowNs()}`);
        written();
        await writer.close();
      } catch {
        outcome = 'failed';
      }
      written();
      log(`K,${id},${t0},${nowNs()},${outcome}`);
    })();
    return { written: writtenP, acked };
  };

  // The carrier prototype (docs/75 D3): one uni stream per GOP; nothing of a
  // superseded GOP survives — queued records are shed, a carrier superseded
  // before it opened is never opened, a blocked write is abandoned and its
  // stream reset — and a retired carrier gets RETIRE_GRACE_MS after its FIN
  // to be acknowledged (Chrome's close() resolves on acknowledgement) before
  // it is reset.
  let latest = -1;
  let current: Carrier | null = null;
  const retire = async (c: Carrier): Promise<void> => {
    if (c.done) return;
    c.done = true;
    const writer = await c.opened;
    if (!writer) return;
    const tFin = nowNs();
    const closed = writer.close().then(
      () => 'acked',
      () => 'failed',
    );
    const outcome = await Promise.race([closed, sleep(RETIRE_GRACE_MS).then(() => 'expired')]);
    if (outcome === 'expired') void writer.abort().catch(() => {});
    log(`C,${c.gop},${tFin},${nowNs()},${outcome}`);
  };
  const drain = async (c: Carrier): Promise<void> => {
    c.draining = true;
    const writer = await c.opened;
    await Promise.race([c.keyframe, c.superseded]);
    while (c.queue.length > 0) {
      const item = c.queue.shift()!;
      if (!writer || c.done || c.gop < latest) {
        log(`D,${item.frame}`);
        continue;
      }
      const t0 = nowNs();
      const r = await Promise.race([
        writer.ready.then(() => writer.write(item.buf)).then(
          () => 'ok' as const,
          () => 'err' as const,
        ),
        c.superseded,
      ]);
      const t1 = nowNs();
      if (t1 - t0 > 1_000_000n) log(`W,${item.frame},${t0},${t1}`);
      if (r === 'superseded') {
        c.done = true;
        void writer.abort().catch(() => {});
        log(`C,${c.gop},${t1},${t1},abandoned`);
      } else if (r === 'err') {
        c.done = true;
      }
    }
    c.draining = false;
    if (c.gop < latest) void retire(c);
  };
  const rotate = (gop: number, keyframe: Promise<unknown>): void => {
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
    current = { gop, opened, queue: [], draining: false, done: false, supersede, superseded, keyframe };
  };
  const sendRecords = (frame: number, dgrams: Uint8Array<ArrayBuffer>[]): void => {
    const c = current;
    if (!c || c.done) {
      log(`D,${frame}`);
      return;
    }
    const parts = dgrams.map((d) => encodeCarrierRecord(d));
    const buf = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let off = 0;
    for (const p of parts) {
      buf.set(p, off);
      off += p.length;
    }
    c.queue.push({ frame, buf });
    if (!c.draining) void drain(c);
  };

  // The audio lane: a 20 ms Opus frame at ~64 kbps, through the same sender.
  const audio = new Uint8Array(180);
  audio[0] = WIRE_VERSION;
  audio[1] = TYPE_AUDIO_FRAME;
  const audioTimer = setInterval(() => {
    offered++;
    if (NOWAIT) void rawWriter.write(audio).catch(() => {});
    else void sender.send([audio], { kind: 'audio' }).catch(() => {});
  }, 20);
  const flushTimer = setInterval(() => void flush(), 500);
  const queueTimer = setInterval(() => log(`Q,${nowNs()},${framesQueued},${framesResolved}`), 1000);

  const payload = new Uint8Array(Math.max(...trace.map((t) => t.size))).fill(0x5a);
  const n = Math.floor(SECONDS * FPS);
  const start = performance.now() + 100;
  for (let i = 0; i < n; i++) {
    const wait = start + (i * 1000) / FPS - performance.now();
    if (wait > 0) await sleep(wait);
    const { size, key } = trace[i % trace.length];
    const frameId = i;
    const timestampUs = BigInt(Math.round((i * 1e6) / FPS));
    const data = payload.subarray(0, size);
    const t = nowNs();
    if (key) {
      const msg = packetizeStreamKeyframe({ frameId, timestampUs }, config, data);
      if (MODE !== 'carrier') {
        gop = frameId;
        kfProduced.set(frameId, t);
        keyframes.produce(frameId, msg);
        log(`F,${frameId},1,${size},0,0,${t},0`);
        continue;
      }
      let gate = (): void => {};
      const keyframeGate = new Promise<unknown>((r) => (gate = () => r(undefined)));
      rotate(frameId, KFORDER ? keyframeGate : Promise.resolve());
      const kf = sendKeyframe(frameId, msg);
      void (KFORDER === 'ack' ? kf.acked : kf.written).then(gate);
      log(`F,${frameId},1,${size},0,0,${t},0`);
      continue;
    }
    const { datagrams, parity } = packetizeFrameWithParity(
      { frameId, keyframe: false, timestampUs },
      data,
      PARITY,
      wt.datagrams.maxDatagramSize,
    );
    const all = parity.length > 0 ? [...datagrams, ...parity] : datagrams;
    if (MODE === 'carrier') {
      sendRecords(frameId, all);
    } else {
      offered += all.length;
      if (NOWAIT) {
        for (const d of all) void rawWriter.write(d).catch(() => {});
      } else {
        framesQueued++;
        sender.send(all, { kind: 'delta', gop }).then(
          (result) => {
            framesResolved++; // a dropped frame is resolved: it no longer waits
            if (result === 'dropped') log(`D,${frameId}`);
          },
          () => {
            framesResolved++; // refused, not waiting: Q counts only what still waits
            log(`E,${frameId},failed`);
          },
        );
      }
    }
    log(`F,${frameId},0,${size},${datagrams.length},${parity.length},${t},0`);
  }
  await sleep(1500);
  clearInterval(audioTimer);
  clearInterval(flushTimer);
  clearInterval(queueTimer);
  // The driver completes this line from Chrome's net-log (sender.mjs).
  log(`S?,${offered}`);
  await flush();
  wt.close();
  await fetch('/done', { method: 'POST' });
}

main().catch(async (e: unknown) => {
  await fetch('/done', { method: 'POST', body: `error: ${String(e)}` });
});
