// Striped deep-buffer decisions (docs/91 D8, D10): the gate, the goodput and
// live-rate measurement, and Auto's engage / grow / step-back policy against
// scripted goodput series.

import { describe, expect, it } from 'vitest';

import {
  DVR_STRIPE_ENGAGE_MS,
  DVR_STRIPE_MAX_WIDTH,
  DVR_STRIPE_REENGAGE_BACKOFF_MS,
  DvrGoodputMeter,
  DvrStripePolicy,
  dvrStripeAllowed,
} from './dvr-stripe';
import { CAP_PARITY_CHUNKS, CAP_STRIPED_DELIVERY, CAP_STRIPED_DVR } from './parity';
import type { StripeMode } from './stripe';

describe('dvrStripeAllowed', () => {
  const all = CAP_PARITY_CHUNKS | CAP_STRIPED_DELIVERY | CAP_STRIPED_DVR;
  const cases: { name: string; deep: boolean; dvrGranted: boolean; capFlags: number; mode: StripeMode; want: boolean }[] = [
    { name: 'deep, granted, capable, auto', deep: true, dvrGranted: true, capFlags: all, mode: 'auto', want: true },
    { name: 'deep, granted, capable, on', deep: true, dvrGranted: true, capFlags: CAP_STRIPED_DVR, mode: 'on', want: true },
    { name: 'Striping off', deep: true, dvrGranted: true, capFlags: all, mode: 'off', want: false },
    { name: 'relay without CAP_STRIPED_DVR', deep: true, dvrGranted: true, capFlags: CAP_STRIPED_DELIVERY, mode: 'on', want: false },
    { name: 'ring denied', deep: true, dvrGranted: false, capFlags: all, mode: 'on', want: false },
    { name: 'not a deep-buffer session', deep: false, dvrGranted: false, capFlags: all, mode: 'on', want: false },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(dvrStripeAllowed(c)).toBe(c.want);
    });
  }
});

// A 60 fps stream with a 30-frame (0.5 s) GOP: 40 KB keyframes, 4 KB deltas.
const FPS = 60;
const GOP = 30;
const KF_BYTES = 40_000;
const DELTA_BYTES = 4_000;
const TRUE_RATE_BPS = ((KF_BYTES + (GOP - 1) * DELTA_BYTES) * 8) / (GOP / FPS);

// Feeds whole GOPs at the given wall-clock pace; `keep` picks which GOPs arrive.
function feedGops(
  meter: DvrGoodputMeter,
  clock: { t: number },
  gops: number,
  wallMsPerGop: number,
  keep: (gop: number) => boolean = () => true,
): void {
  for (let g = 0; g < gops; g++) {
    for (let i = 0; i < GOP; i++) {
      const frameId = g * GOP + i;
      clock.t += wallMsPerGop / GOP;
      if (!keep(g)) continue;
      const ts = BigInt(Math.round((frameId * 1e6) / FPS));
      meter.observeFrame(frameId, ts, i === 0 ? KF_BYTES : DELTA_BYTES, i === 0);
    }
  }
}

describe('DvrGoodputMeter', () => {
  it('measures the live rate from the stream itself and goodput from the wall clock', () => {
    const clock = { t: 0 };
    const meter = new DvrGoodputMeter(() => clock.t);
    // Delivered at half speed: goodput half the live rate.
    feedGops(meter, clock, 20, 1000);
    expect(meter.liveRateBps()).toBeCloseTo(TRUE_RATE_BPS, -3);
    expect(meter.goodputBps()! / TRUE_RATE_BPS).toBeCloseTo(0.5, 1);
  });

  it('keeps the live rate honest when GOPs are skipped — the case it exists for', () => {
    const clock = { t: 0 };
    const meter = new DvrGoodputMeter(() => clock.t);
    // Real time, but every third GOP never arrives (a relay resync's shape).
    feedGops(meter, clock, 30, 500, (g) => g % 3 !== 2);
    expect(meter.liveRateBps()).toBeCloseTo(TRUE_RATE_BPS, -3);
    expect(meter.goodputBps()! / TRUE_RATE_BPS).toBeLessThan(0.9);
  });

  it('counts a GOP served twice once (docs/91 D11)', () => {
    const clock = { t: 0 };
    const once = new DvrGoodputMeter(() => clock.t);
    feedGops(once, clock, 8, 500);
    const single = once.goodputBps();
    const clock2 = { t: 0 };
    const twice = new DvrGoodputMeter(() => clock2.t);
    for (let g = 0; g < 8; g++) {
      for (let i = 0; i < GOP; i++) {
        const frameId = g * GOP + i;
        clock2.t += 500 / GOP;
        const ts = BigInt(Math.round((frameId * 1e6) / FPS));
        twice.observeFrame(frameId, ts, i === 0 ? KF_BYTES : DELTA_BYTES, i === 0);
        twice.observeFrame(frameId, ts, i === 0 ? KF_BYTES : DELTA_BYTES, i === 0);
      }
    }
    expect(twice.goodputBps()).toBeCloseTo(single!, 0);
  });

  it('keeps the last live rate through a window with no keyframe-to-keyframe run', () => {
    const clock = { t: 0 };
    const meter = new DvrGoodputMeter(() => clock.t);
    feedGops(meter, clock, 6, 500);
    const rate = meter.liveRateBps();
    clock.t += 60_000;
    expect(meter.liveRateBps()).toBe(rate);
    meter.reset();
    expect(meter.liveRateBps()).toBeNull();
  });
});

// --- The policy, against scripted goodput -------------------------------------

const LIVE = 10_000_000;
const TICK_MS = 500;

interface Run {
  widths: number[]; // the requested width at every tick
  final: number;
  phase: string;
}

// Drives the policy at the stats cadence. `goodput(width)` is the path: what
// the aggregate carries at a given number of connections. The transport is
// modelled as always bringing the requested legs up at once.
function simulate(opts: {
  mode: StripeMode;
  seconds: number;
  goodput: (width: number, tMs: number) => number;
  resyncAt?: (tMs: number) => boolean;
}): Run {
  const clock = { t: 0 };
  const policy = new DvrStripePolicy(() => clock.t);
  let active = 1;
  let resyncs = 0;
  const widths: number[] = [];
  for (let t = 0; t <= opts.seconds * 1000; t += TICK_MS) {
    clock.t = t;
    if (opts.resyncAt?.(t)) resyncs++;
    const width = policy.decide({
      allowed: true,
      mode: opts.mode,
      goodputBps: opts.goodput(active, t),
      liveRateBps: LIVE,
      resyncs,
    });
    widths.push(width);
    if (width !== active) {
      active = width;
      policy.noteActive(width);
    }
  }
  return { widths, final: widths.at(-1)!, phase: policy.snapshot().phase };
}

describe('DvrStripePolicy (docs/91 D10)', () => {
  it('stays unstriped on a path that carries the stream', () => {
    const r = simulate({ mode: 'auto', seconds: 120, goodput: () => LIVE });
    expect(Math.max(...r.widths)).toBe(1);
  });

  it('grows to ×3 on a per-connection ceiling, each step having helped', () => {
    // Each connection carries 4 Mbit/s; the link has room for the stream.
    const r = simulate({ mode: 'auto', seconds: 120, goodput: (w) => Math.min(LIVE, w * 4_000_000) });
    expect(r.final).toBe(3);
    expect(r.phase).toBe('engaged');
    // Monotonic: it never stepped back on the way.
    for (let i = 1; i < r.widths.length; i++) expect(r.widths[i]).toBeGreaterThanOrEqual(r.widths[i - 1]);
  });

  it('steps back to ×1 and holds when the link itself is full', () => {
    const r = simulate({ mode: 'auto', seconds: 180, goodput: () => 5_000_000 });
    expect(Math.max(...r.widths)).toBe(2);
    expect(r.final).toBe(1);
    expect(r.phase).toBe('held');
    // Held for the session: once back at ×1 it never tries again.
    const back = r.widths.lastIndexOf(2) + 1;
    expect(r.widths.slice(back).every((w) => w === 1)).toBe(true);
  });

  it('stops at ×2 when the second connection helped and the third did not', () => {
    // 4 Mbit/s per connection, 7 Mbit/s for the link.
    const r = simulate({ mode: 'auto', seconds: 180, goodput: (w) => Math.min(7_000_000, w * 4_000_000) });
    expect(Math.max(...r.widths)).toBe(3);
    expect(r.final).toBe(2);
    expect(r.phase).toBe('held');
  });

  it(`never exceeds ×${DVR_STRIPE_MAX_WIDTH}`, () => {
    const r = simulate({ mode: 'auto', seconds: 300, goodput: (w) => w * 1_000_000 });
    expect(Math.max(...r.widths)).toBe(DVR_STRIPE_MAX_WIDTH);
  });

  it('engages on recurring relay resyncs even when goodput looks fine', () => {
    const r = simulate({
      mode: 'auto',
      seconds: 60,
      goodput: (w) => (w > 1 ? LIVE * 1.3 : LIVE),
      resyncAt: (t) => t > 0 && t % 4000 === 0,
    });
    expect(r.widths.indexOf(2)).toBeGreaterThan(0);
    expect(r.widths.indexOf(2) * TICK_MS).toBeGreaterThanOrEqual(DVR_STRIPE_ENGAGE_MS);
  });

  it('does not engage on a single resync', () => {
    const r = simulate({ mode: 'auto', seconds: 60, goodput: () => LIVE, resyncAt: (t) => t === 5000 });
    expect(Math.max(...r.widths)).toBe(1);
  });

  it('On engages ×2 at join, without evidence, and grows by the same rule', () => {
    const clean = simulate({ mode: 'on', seconds: 60, goodput: () => LIVE });
    expect(clean.widths[0]).toBe(2);
    expect(clean.final).toBe(2);
    const capped = simulate({ mode: 'on', seconds: 120, goodput: (w) => Math.min(LIVE, w * 2_500_000) });
    expect(capped.final).toBe(4);
  });

  it('On never returns below ×2 when a growth step does not help', () => {
    const r = simulate({ mode: 'on', seconds: 120, goodput: () => 5_000_000 });
    expect(Math.max(...r.widths)).toBe(3);
    expect(r.final).toBe(2);
  });

  it('Off never stripes', () => {
    const r = simulate({ mode: 'off', seconds: 120, goodput: () => 1_000_000 });
    expect(Math.max(...r.widths)).toBe(1);
  });

  it('backs off after a leg death, then asks for the same width again', () => {
    const clock = { t: 0 };
    const policy = new DvrStripePolicy(() => clock.t);
    const input = { allowed: true, mode: 'on' as const, goodputBps: LIVE, liveRateBps: LIVE, resyncs: 0 };
    expect(policy.decide(input)).toBe(2);
    policy.noteActive(2);
    clock.t = 1000;
    policy.noteActive(1); // the transport fell back
    expect(policy.decide(input)).toBe(1);
    clock.t = 1000 + DVR_STRIPE_REENGAGE_BACKOFF_MS;
    expect(policy.decide(input)).toBe(2);
  });

  it('treats a step whose legs never come up as one that did not help', () => {
    const clock = { t: 0 };
    const policy = new DvrStripePolicy(() => clock.t);
    let width = 1;
    for (clock.t = 0; clock.t <= 40_000; clock.t += TICK_MS) {
      width = policy.decide({ allowed: true, mode: 'auto', goodputBps: 4_000_000, liveRateBps: LIVE, resyncs: 0 });
      // The transport never reports the legs up.
    }
    expect(width).toBe(1);
    expect(policy.snapshot().phase).toBe('held');
  });

  it('returns to ×1 when the gate closes, and re-engages under On when it opens', () => {
    const clock = { t: 0 };
    const policy = new DvrStripePolicy(() => clock.t);
    const base = { mode: 'on' as const, goodputBps: LIVE, liveRateBps: LIVE, resyncs: 0 };
    expect(policy.decide({ ...base, allowed: true })).toBe(2);
    expect(policy.decide({ ...base, allowed: false })).toBe(1);
    expect(policy.decide({ ...base, allowed: true })).toBe(2);
  });
});
