// The uplink lab's viewer half (docs/73). Replays a frame-arrival trace
// measured at the relay through the SHIPPING reorder buffer and adaptive
// playout controller under a fake clock, then through a model of
// PacedPresentationSink on a 60 Hz vsync, and reports what the viewer sees.
//
// Not part of the suite: it only runs with UPLINK_TRACE or UPLINK_BATCH set.
//   UPLINK_TRACE=trace.json UPLINK_PLAYOUT=adaptive UPLINK_OUT=view.json \
//     npx vitest run lab/uplink-replay.lab.test.ts
// UPLINK_BATCH names a file of "trace playout out" lines, replayed in turn.

import { readFileSync, writeFileSync } from 'node:fs';
import { it } from 'vitest';

import {
  DECODE_LEAD_MS,
  getPlayoutOffsetMs,
  resetPlayoutController,
  setPlayoutMode,
  updatePlayoutController,
} from '../src/transport/playout';
import { ReorderBuffer, resetGraceController, updateGraceController } from '../src/transport/reorder-buffer';
import { getLossAllowanceFrames } from '../src/transport/resilient';

interface TraceFrame {
  id: number;
  key: boolean;
  ts: number; // capture (send) time, ms since run start
  sched: number; // nominal frame time, ms
  at: number; // arrival at the relay, ms since run start
}

const single = process.env.UPLINK_TRACE;
const batch = process.env.UPLINK_BATCH;

it.runIf(!!single || !!batch)('replays uplink traces through the viewer', () => {
  const jobs: [string, string, string | undefined][] = batch
    ? readFileSync(batch, 'utf8')
        .split('\n')
        .filter((l) => l.trim())
        .map((l) => l.trim().split(/\s+/) as [string, string, string])
    : [[single!, process.env.UPLINK_PLAYOUT ?? 'adaptive', process.env.UPLINK_OUT]];
  for (const [trace, playout, outPath] of jobs) {
    replay(trace, playout as 'off' | 'adaptive', outPath);
  }
});

function replay(trace: string, playout: 'off' | 'adaptive', outPath: string | undefined): void {
  const frames: TraceFrame[] = JSON.parse(readFileSync(trace, 'utf8'));
  const legBms = Number(process.env.UPLINK_LEG_B_MS ?? '0');
  const decodeMs = Number(process.env.UPLINK_DECODE_MS ?? '2');
  const skipMs = Number(process.env.UPLINK_SKIP_MS ?? '10000');
  const vsyncMs = 1000 / 60;

  setPlayoutMode('off');
  resetPlayoutController();
  resetGraceController();
  setPlayoutMode(playout);

  const clock = { t: 0 };
  // Decoded frames waiting for the sink, in decode order.
  const decoding: { id: number; ts: number; readyAt: number }[] = [];
  const rb = new ReorderBuffer(
    (f) => {
      decoding.push({ id: f.frameId, ts: Number(f.timestampUs) / 1000, readyAt: clock.t + decodeMs });
    },
    () => clock.t,
  );

  // The sink: `immediate` (live edge) or `held` with targets (adaptive).
  let immediate: { id: number; ts: number } | null = null;
  const held: { id: number; ts: number; target: number }[] = [];
  const presented: { id: number; ts: number; at: number }[] = [];
  let sinkDropped = 0;
  const offsets: { t: number; offset: number }[] = [];

  const arrivals = frames
    .map((f) => ({ ...f, at: f.at + legBms }))
    .sort((a, b) => a.at - b.at);
  const end = arrivals[arrivals.length - 1].at + 1500;

  let ai = 0;
  let nextReorderTick = 0;
  let nextStats = 0;
  let nextVsync = 0;
  // Discrete-event loop at 0.25 ms resolution: plenty for 16 ms vsync and
  // the millisecond-scale arrival spread under test.
  for (let t = 0; t <= end; t += 0.25) {
    clock.t = t;
    while (ai < arrivals.length && arrivals[ai].at <= t) {
      const f = arrivals[ai++];
      const msg = {
        frameId: f.id,
        timestampUs: BigInt(Math.round(f.sched * 1000)),
        data: new Uint8Array(1),
      };
      if (f.key) rb.pushKeyframe({ ...msg, config: null });
      else rb.pushDelta(msg);
    }
    if (t >= nextReorderTick) {
      rb.tick();
      nextReorderTick += 16;
    }
    if (t >= nextStats) {
      const j = rb.arrivalJitterMs();
      updatePlayoutController(j, t);
      updateGraceController(j, t);
      offsets.push({ t, offset: getPlayoutOffsetMs() });
      nextStats += 500;
    }
    // Decoder output → sink.
    while (decoding.length && decoding[0].readyAt <= t) {
      const d = decoding.shift()!;
      const offset = getPlayoutOffsetMs();
      const base = rb.arrivalBaselineMs();
      if (playout === 'adaptive' && offset > 0 && base !== null) {
        held.push({ id: d.id, ts: d.ts, target: d.ts + base + offset });
        while (held.length > 3) {
          held.shift();
          sinkDropped++;
        }
      } else {
        if (immediate) sinkDropped++;
        immediate = { id: d.id, ts: d.ts };
      }
    }
    if (t >= nextVsync) {
      const dueBy = t + vsyncMs / 2;
      if (immediate) {
        presented.push({ ...immediate, at: t });
        immediate = null;
      } else {
        let due = 0;
        while (due < held.length && held[due].target <= dueBy) due++;
        if (due > 0) {
          const ds = held.splice(0, due);
          const w = ds.pop()!;
          sinkDropped += ds.length;
          presented.push({ id: w.id, ts: w.ts, at: t });
        }
      }
      nextVsync += vsyncMs;
    }
  }

  // Metrics over [skip, end): what is on screen at every vsync.
  // The window is fixed by the source, not by what reached the screen: from
  // the skip to the last capture. Time with nothing new on screen —
  // including before the first frame and after the last — is a hold, so a
  // viewer who saw nothing is 100 % stalled rather than an empty
  // measurement. "Shown" counts the frames captured at least 1 s before the
  // window closes (past any offset the controller can reach).
  const windowEnd = frames[frames.length - 1].sched;
  const durS = (windowEnd - skipMs) / 1000;
  const shown = presented.filter((p) => p.at >= skipMs && p.at < windowEnd);
  const lat = shown.map((p) => p.at - p.ts).sort((a, b) => a - b);
  const q = (p: number) =>
    lat.length ? lat[Math.min(lat.length - 1, Math.floor((lat.length - 1) * p))] : null;
  // A hitch is the same source frame on screen for longer than one source
  // interval — measured between consecutive distinct presented frames.
  const edges = [skipMs, ...shown.map((p) => p.at), windowEnd];
  const holds: number[] = [];
  for (let i = 1; i < edges.length; i++) holds.push(edges[i] - edges[i - 1]);
  const hitch = (ms: number) => holds.filter((h) => h >= ms).length;
  // Hitch lengths, per minute: a recovery-sized catch-up (50–250 ms) reads
  // very differently from a freeze to the next keyframe (250 ms and up).
  const band = (lo: number, hi: number) => (holds.filter((h) => h >= lo && h < hi).length * 60) / durS;
  const stallMs = holds.filter((h) => h > 2.5 * vsyncMs).reduce((a, h) => a + h, 0);
  const firstId = Math.round((skipMs * 60) / 1000);
  const lastId = Math.round(((windowEnd - 1000) * 60) / 1000);
  const expected = lastId - firstId + 1;
  const shownIds = new Set(shown.map((p) => p.id).filter((id) => id >= firstId && id <= lastId));
  const offs = offsets.filter((o) => o.t >= skipMs).map((o) => o.offset);
  const stats = rb.getStats();
  const out = {
    playout,
    allowance: getLossAllowanceFrames(),
    seconds: durS,
    shownPct: (100 * shownIds.size) / expected,
    latencyMs: { p50: q(0.5), p95: q(0.95), p99: q(0.99), max: lat.length ? lat[lat.length - 1] : null },
    hitchesPerMin: {
      ge50: (hitch(50) * 60) / durS,
      ge100: (hitch(100) * 60) / durS,
      ge250: (hitch(250) * 60) / durS,
    },
    hitchBandsPerMin: {
      '50-100': band(50, 100),
      '100-250': band(100, 250),
      '250-500': band(250, 500),
      '500+': band(500, Infinity),
    },
    stallPct: (100 * stallMs) / (durS * 1000),
    longestHoldMs: Math.max(...holds),
    offsetMs: {
      mean: offs.reduce((a, b) => a + b, 0) / Math.max(1, offs.length),
      max: Math.max(...offs),
      last: offs[offs.length - 1],
    },
    gapResyncs: stats.gapResyncs,
    skippedWithinAllowance: stats.framesSkippedWithinAllowance,
    sinkDropped,
    decodeLeadMs: DECODE_LEAD_MS,
    // Per second of the whole run: the playout offset and the median
    // on-screen latency — how long a loss episode's cost outlives it.
    timeline: Array.from({ length: Math.floor(end / 1000) }, (_, s) => {
      const inSec = presented.filter((p) => p.at >= s * 1000 && p.at < (s + 1) * 1000);
      const l = inSec.map((p) => p.at - p.ts).sort((a, b) => a - b);
      return {
        s,
        offset: offsets.find((o) => o.t >= s * 1000)?.offset ?? null,
        latP50: l.length ? l[Math.floor(l.length / 2)] : null,
        shown: inSec.length,
      };
    }),
  };
  if (outPath) writeFileSync(outPath, JSON.stringify(out, null, 1));
  else console.log(JSON.stringify(out));
}
