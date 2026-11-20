// Striped deep-buffer decisions (docs/91 D8, D10): the gate, the goodput and
// live-rate measurement, and Auto's engage / grow / step-back policy against
// scripted goodput series.

import { describe, expect, it } from 'vitest';

import {
  DVR_STRIPE_ENGAGE_MS,
  DVR_STRIPE_MAX_WIDTH,
  DVR_STRIPE_REENGAGE_BACKOFF_MS,
  DVR_STRIPE_RETRY_BASE_MS,
  DVR_STRIPE_RETRY_MAX_MS,
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

  // Review round 2, finding 1: idle is no video bytes at all, on any
  // connection; bytes without a complete frame are starvation.
  it('is idle only when no video bytes arrived in the window', () => {
    const clock = { t: 0 };
    const meter = new DvrGoodputMeter(() => clock.t);
    feedGops(meter, clock, 4, 500);
    expect(meter.idle()).toBe(false);
    // A big keyframe trickling in: bytes, no complete frame.
    for (let i = 0; i < 6; i++) {
      clock.t += 500;
      meter.observeBytes(1200);
    }
    expect(meter.idle()).toBe(false);
    expect(meter.goodputBps()).toBe(0);
    clock.t += 2500;
    expect(meter.idle()).toBe(true);
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
  mode: StripeMode | ((tMs: number) => StripeMode);
  seconds: number;
  goodput: (width: number, tMs: number) => number;
  resyncAt?: (tMs: number, width: number) => boolean;
  // No video bytes at all on any connection: a broadcaster pause.
  idle?: (tMs: number) => boolean;
}): Run {
  const clock = { t: 0 };
  const policy = new DvrStripePolicy(() => clock.t);
  let active = 1;
  let resyncs = 0;
  const widths: number[] = [];
  for (let t = 0; t <= opts.seconds * 1000; t += TICK_MS) {
    clock.t = t;
    if (opts.resyncAt?.(t, active)) resyncs++;
    const width = policy.decide({
      allowed: true,
      mode: typeof opts.mode === 'function' ? opts.mode(t) : opts.mode,
      goodputBps: opts.goodput(active, t),
      liveRateBps: LIVE,
      resyncs,
      idle: opts.idle?.(t) ?? false,
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
    // Back at ×1 at the end: retries (below) never leave a wider width held.
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

  // Review round 1, finding 2: a broadcaster pause delivers nothing, which is
  // idle, not starved — the cached live rate must not turn it into
  // undersupply, an evaluation must not be judged on it, and it must not set
  // a step's baseline.
  it('never engages through a broadcaster pause', () => {
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: (_w, t) => (t < 20_000 ? LIVE : 0),
      idle: (t) => t >= 20_000,
    });
    expect(Math.max(...r.widths)).toBe(1);
    expect(r.phase).toBe('idle');
  });

  it('does not judge a step on a pause: the evaluation waits for frames again', () => {
    // A per-connection ceiling, engaged at ~10 s; the broadcaster then pauses
    // for 20 s, right across the first evaluation window.
    const paused = (t: number) => t >= 11_000 && t < 31_000;
    const r = simulate({
      mode: 'auto',
      seconds: 150,
      goodput: (w, t) => (paused(t) ? 0 : Math.min(LIVE, w * 4_000_000)),
      idle: paused,
    });
    expect(r.final).toBe(3);
    expect(r.phase).toBe('engaged');
  });

  it('does not let a pause before a step set its baseline', () => {
    // Undersupplied, then paused, then undersupplied again: the baseline is
    // the 4 Mbit/s actually measured, not an average with the pause's zeros
    // (which would make a step that did nothing look like it helped).
    const paused = (t: number) => t >= 5_000 && t < 9_000;
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: (_w, t) => (paused(t) ? 0 : 4_000_000),
      idle: paused,
    });
    expect(r.final).toBe(1);
    expect(r.phase).toBe('held');
  });

  // Review round 2, finding 1: on a starved link a window with no COMPLETE
  // frame (a big keyframe or a stalled GOP still arriving) is starvation, not
  // a pause — bytes are still coming in. It must count as zero goodput.
  it('engages on a starved link whose windows often hold no complete frame', () => {
    const r = simulate({
      mode: 'auto',
      seconds: 60,
      goodput: (w, t) => (w > 1 ? LIVE : t % 3000 < 2000 ? 0 : 3_000_000),
    });
    expect(Math.max(...r.widths)).toBe(2);
    expect(r.final).toBe(2);
  });

  it('does not postpone a step\'s evaluation forever: a flickering pause still gets judged', () => {
    // A full link (the step cannot help); once engaged, a one-tick pause
    // every 4 s keeps restarting the evaluation.
    const flicker = (t: number) => t > 12_000 && t % 4000 === 0;
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: (_w, t) => (flicker(t) ? 0 : 5_000_000),
      idle: flicker,
    });
    // Judged (not helped) by the postponement bound, well before the cool-down
    // could bring a retry.
    expect(Math.max(...r.widths)).toBe(2);
    expect(r.final).toBe(1);
    expect(r.phase).toBe('held');
  });

  // Review round 3, finding 2: a pause longer than the postponement bound
  // leaves no frame-bearing sample to judge on; judging an empty window would
  // read "not helped" and hold for the session. It waits for frames instead.
  it('does not hold the policy over a 90 s pause during an evaluation', () => {
    const paused = (t: number) => t >= 11_000 && t < 101_000;
    const r = simulate({
      mode: 'auto',
      seconds: 220,
      goodput: (w, t) => (paused(t) ? 0 : Math.min(LIVE, w * 4_000_000)),
      idle: paused,
    });
    expect(r.phase).toBe('engaged');
    expect(r.final).toBe(3);
  });

  // Review round 1, finding 3: goodput cannot exceed the live rate, so a step
  // from a near-live baseline can never show +20 %. Removing the shortfall
  // counts as helping; a full link still steps back.
  it('keeps a resync-only engagement that ended the skips', () => {
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: () => LIVE,
      resyncAt: (t, w) => w === 1 && t > 0 && t % 4000 === 0,
    });
    expect(r.final).toBe(2);
    expect(r.phase).toBe('engaged');
  });

  it('keeps a step that brought goodput from 85 % to the live rate', () => {
    const r = simulate({ mode: 'auto', seconds: 120, goodput: (w) => (w > 1 ? LIVE : 0.85 * LIVE) });
    expect(r.final).toBe(2);
    expect(r.phase).toBe('engaged');
  });

  it('still steps back on a full link whose skips continue at a flat near-live total', () => {
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: () => 0.97 * LIVE,
      resyncAt: (t) => t > 0 && t % 4000 === 0,
    });
    expect(Math.max(...r.widths)).toBe(2);
    expect(r.final).toBe(1);
    expect(r.phase).toBe('held');
  });

  it('does not engage on a single resync', () => {
    const r = simulate({ mode: 'auto', seconds: 60, goodput: () => LIVE, resyncAt: (t) => t === 5000 });
    expect(Math.max(...r.widths)).toBe(1);
  });

  it('engages on skips that recur slower than the engage hold (docs/91 §11.3)', () => {
    // ~92 % of live never reads as starved; a skip every 25 s is the shortfall.
    const r = simulate({
      mode: 'auto',
      seconds: 120,
      goodput: (w) => (w === 1 ? 0.92 * LIVE : LIVE),
      resyncAt: (t, w) => w === 1 && t > 0 && t % 25_000 === 0,
    });
    expect(r.final).toBe(2);
    expect(r.phase).toBe('engaged');
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
    const input = { allowed: true, mode: 'on' as const, goodputBps: LIVE, liveRateBps: LIVE, resyncs: 0, idle: false };
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
      width = policy.decide({ allowed: true, mode: 'auto', goodputBps: 4_000_000, liveRateBps: LIVE, resyncs: 0, idle: false });
      // The transport never reports the legs up.
    }
    expect(width).toBe(1);
    expect(policy.snapshot().phase).toBe('held');
  });

  it('returns to ×1 when the gate closes, and re-engages under On when it opens', () => {
    const clock = { t: 0 };
    const policy = new DvrStripePolicy(() => clock.t);
    const base = { mode: 'on' as const, goodputBps: LIVE, liveRateBps: LIVE, resyncs: 0, idle: false };
    expect(policy.decide({ ...base, allowed: true })).toBe(2);
    expect(policy.decide({ ...base, allowed: false })).toBe(1);
    expect(policy.decide({ ...base, allowed: true })).toBe(2);
  });
});

// Times (ms) at which the requested width changed, with the new width.
function changes(widths: number[]): { t: number; to: number }[] {
  const out: { t: number; to: number }[] = [];
  for (let i = 1; i < widths.length; i++) {
    if (widths[i] !== widths[i - 1]) out.push({ t: i * TICK_MS, to: widths[i] });
  }
  return out;
}

// docs/91 §11.3: `held` locked in a width that still lost GOPs for the rest
// of the session. It is now a cool-down: a still-undersupplied session retries
// the next width after DVR_STRIPE_RETRY_BASE_MS of non-idle time, doubling
// after each retry that fails again, reset by one that helps.
describe('DvrStripePolicy retries a held width (docs/91 §11.3)', () => {
  it('retries a full link after 120 s, fails, backs off to 240 s, and never holds a wider width', () => {
    const r = simulate({ mode: 'auto', seconds: 700, goodput: () => 5_000_000 });
    const c = changes(r.widths);
    // engage, back, retry, back, retry, back, …
    expect(c.slice(0, 6).map((x) => x.to)).toEqual([2, 1, 2, 1, 2, 1]);
    const near = (a: number, b: number) => expect(Math.abs(a - b)).toBeLessThanOrEqual(TICK_MS);
    near(c[2].t - c[1].t, DVR_STRIPE_RETRY_BASE_MS);
    near(c[4].t - c[3].t, 2 * DVR_STRIPE_RETRY_BASE_MS);
    expect(Math.max(...r.widths)).toBe(2);
    expect(r.final).toBe(1);
  });

  it('ends at ×3 when ×2 is near the live rate but still skips GOPs and ×3 removes the skips', () => {
    // ×1 starved; ×2 at 97 % of live with a hole skip every 8 s; ×3 at the
    // live rate, its first try still skipping (transition noise) and clean
    // after that — the lab's runs that held ×2 for the session.
    const r = simulate({
      mode: 'auto',
      seconds: 300,
      goodput: (w) => (w === 1 ? 0.6 * LIVE : w === 2 ? 0.97 * LIVE : LIVE),
      resyncAt: (t, w) => t > 0 && t % 8000 === 0 && (w === 2 || (w === 3 && t < 60_000)),
    });
    expect(changes(r.widths).map((x) => x.to)).toEqual([2, 3, 2, 3]);
    expect(r.final).toBe(3);
    expect(r.phase).toBe('engaged');
  });

  it('does not retry on the goodput dip at the edges of a pause', () => {
    // Full link until 60 s (held at ×1), then a link that carries the stream.
    // Every 40 s after that the broadcaster pauses for 8 s; the 2 s goodput
    // window reads short just before and after each pause.
    const paused = (t: number) => t >= 60_000 && t % 40_000 >= 20_000 && t % 40_000 < 28_000;
    const nearPause = (t: number) => t >= 60_000 && t % 40_000 >= 18_000 && t % 40_000 < 30_000;
    const r = simulate({
      mode: 'auto',
      seconds: 600,
      goodput: (_w, t) => (t < 60_000 ? 5_000_000 : nearPause(t) ? 0.5 * LIVE : LIVE),
      idle: paused,
    });
    expect(Math.max(...r.widths.slice(100_000 / TICK_MS))).toBe(1);
    expect(r.phase).toBe('held');
  });

  it('a retry abandoned by Off does not double the next cool-down', () => {
    // Full link: held at ×1 by ~25 s, retry ×2 after the 120 s cool-down;
    // Off during that retry, then Auto again engages afresh and fails.
    const offFrom = 150_000;
    const offTo = 155_000;
    const r = simulate({
      mode: (t) => (t >= offFrom && t < offTo ? 'off' : 'auto'),
      seconds: 600,
      goodput: () => 5_000_000,
    });
    const c = changes(r.widths).filter((x) => x.t >= offTo);
    // Fresh ×2, back to ×1, then the first retry after the BASE cool-down.
    expect(c.slice(0, 3).map((x) => x.to)).toEqual([2, 1, 2]);
    expect(Math.abs(c[2].t - c[1].t - DVR_STRIPE_RETRY_BASE_MS)).toBeLessThanOrEqual(TICK_MS);
  });

  it('does not advance the cool-down during a broadcaster pause', () => {
    // Held at ~23 s; a 100 s pause inside the cool-down pushes the retry out.
    const paused = (t: number) => t >= 40_000 && t < 140_000;
    const r = simulate({
      mode: 'auto',
      seconds: 400,
      goodput: (_w, t) => (paused(t) ? 0 : 5_000_000),
      idle: paused,
    });
    const c = changes(r.widths);
    expect(c[1].to).toBe(1);
    expect(c[2].to).toBe(2);
    expect(c[2].t - c[1].t).toBeGreaterThanOrEqual(DVR_STRIPE_RETRY_BASE_MS + 100_000 - TICK_MS);
  });

  it('resets the backoff after a retry that helps', () => {
    // A full link until 300 s (engage fails, first retry fails: cool-down now
    // 240 s); then a per-connection ceiling, so the next retry helps and
    // grows; from 500 s the link caps at 8 Mbit/s, so ×4 fails — and the
    // cool-down after it is back at the base, not 480 s.
    const r = simulate({
      mode: 'auto',
      seconds: 900,
      goodput: (w, t) => (t < 300_000 ? 5_000_000 : t < 500_000 ? Math.min(LIVE, w * 4_000_000) : Math.min(8_000_000, w * 4_000_000)),
    });
    const c = changes(r.widths);
    const toFour = c.findIndex((x) => x.to === 4);
    expect(toFour).toBeGreaterThan(0);
    expect(c[toFour + 1].to).toBe(3);
    expect(c[toFour + 2].to).toBe(4);
    expect(Math.abs(c[toFour + 2].t - c[toFour + 1].t - DVR_STRIPE_RETRY_BASE_MS)).toBeLessThanOrEqual(TICK_MS);
  });

  it(`caps the cool-down at ${DVR_STRIPE_RETRY_MAX_MS / 60_000} min`, () => {
    const r = simulate({ mode: 'auto', seconds: 4 * 3600, goodput: () => 5_000_000 });
    const c = changes(r.widths);
    let maxGap = 0;
    for (let i = 2; i < c.length; i += 2) maxGap = Math.max(maxGap, c[i].t - c[i - 1].t);
    expect(maxGap).toBeLessThanOrEqual(DVR_STRIPE_RETRY_MAX_MS + TICK_MS);
    expect(maxGap).toBeGreaterThanOrEqual(DVR_STRIPE_RETRY_MAX_MS - TICK_MS);
  });

  it('keeps On at ×2 or wider through a failed retry', () => {
    const r = simulate({ mode: 'on', seconds: 400, goodput: () => 5_000_000 });
    expect(Math.min(...r.widths)).toBe(2);
    expect(Math.max(...r.widths)).toBe(3);
  });
});
