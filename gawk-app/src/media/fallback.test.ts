// FallbackController decision-core tests (docs/09, chunk I1). Written before
// the implementation per CODE-REVIEW.md. The controller is pure and
// timer-free: time is injected per record, so every scenario here is a
// deterministic outcome sequence.

import { describe, expect, it } from 'vitest';

import {
  COOLDOWN_MS,
  ERROR_FAIL_WINDOW_MS,
  FallbackController,
  MIN_SAMPLES,
  UP_PROBE_MAX_MS,
  UP_PROBE_MS,
  WINDOW_MS,
  type FallbackDecision,
} from './fallback';

interface Emitted {
  atMs: number;
  decision: Exclude<FallbackDecision, 'none'>;
}

// Feeds outcomes at a steady fps and collects non-'none' decisions. The
// reject pattern is spread evenly via an accumulator so a 30% ratio really
// is ~30% over any full window, not front-loaded.
class Harness {
  nowMs = 0;
  decisions: Emitted[] = [];
  readonly c: FallbackController;
  private rejectAcc = 0;

  constructor(c: FallbackController) {
    this.c = c;
  }

  feed(durationMs: number, opts: { fps?: number; rejectRatio?: number } = {}): void {
    const fps = opts.fps ?? 30;
    const rejectRatio = opts.rejectRatio ?? 0;
    const intervalMs = 1000 / fps;
    const frames = Math.round(durationMs / intervalMs);
    for (let i = 0; i < frames; i++) {
      this.nowMs += intervalMs;
      this.rejectAcc += rejectRatio;
      let accepted = true;
      if (this.rejectAcc >= 1) {
        this.rejectAcc -= 1;
        accepted = false;
      }
      const d = this.c.record(accepted, this.nowMs);
      if (d !== 'none') this.decisions.push({ atMs: this.nowMs, decision: d });
    }
  }

  idle(durationMs: number): void {
    this.nowMs += durationMs;
  }

  stepDowns(): Emitted[] {
    return this.decisions.filter((d) => d.decision === 'stepDown');
  }

  stepUps(): Emitted[] {
    return this.decisions.filter((d) => d.decision === 'stepUp');
  }
}

describe('FallbackController step-down trigger', () => {
  it('steps down exactly once under steady 30% rejection, then cools down', () => {
    const h = new Harness(new FallbackController());
    // 13s of steady 30% rejection: trigger fires once the window is full
    // (~WINDOW_MS in), and the cooldown + fresh window mean no second step
    // fits before 13s (needs ~WINDOW_MS + COOLDOWN_MS + WINDOW_MS).
    h.feed(13_000, { rejectRatio: 0.3 });
    expect(h.stepDowns()).toHaveLength(1);
    expect(h.stepUps()).toHaveLength(0);
    const at = h.stepDowns()[0].atMs;
    expect(at).toBeGreaterThanOrEqual(WINDOW_MS);
    expect(at).toBeLessThan(WINDOW_MS + 1000);
  });

  it('never decides before a full window has been observed (MIN_SAMPLES burst)', () => {
    const h = new Harness(new FallbackController());
    // A 100% rejection burst shorter than MIN_SAMPLES frames, then healthy:
    // by the time the window is full the burst is a small minority.
    h.feed(((MIN_SAMPLES - 5) * 1000) / 30, { rejectRatio: 1 });
    h.feed(8_000);
    expect(h.stepDowns()).toHaveLength(0);
  });

  it('is immune to a sub-second saturation spike inside a healthy stream', () => {
    const h = new Harness(new FallbackController());
    h.feed(10_000, { fps: 60 });
    h.feed(300, { fps: 60, rejectRatio: 1 }); // scene-change spike
    h.feed(10_000, { fps: 60 });
    expect(h.stepDowns()).toHaveLength(0);
  });

  it('lets old outcomes expire: early rejections outside the window never trigger', () => {
    const h = new Harness(new FallbackController());
    // 40% rejection for 2s would trigger if it were sustained over a full
    // window; diluted by the following healthy stream it must not.
    h.feed(2_000, { rejectRatio: 0.4 });
    h.feed(10_000);
    expect(h.stepDowns()).toHaveLength(0);
  });

  it('never decides at all below MIN_SAMPLES per window (very low fps)', () => {
    const h = new Harness(new FallbackController());
    // 3 fps: a full window holds ~12 outcomes < MIN_SAMPLES.
    h.feed(30_000, { fps: 3, rejectRatio: 1 });
    expect(h.decisions).toHaveLength(0);
  });

  it('discards outcomes during the cooldown; the next step needs a fresh full window', () => {
    const h = new Harness(new FallbackController());
    h.feed(30_000, { rejectRatio: 1 });
    const downs = h.stepDowns();
    expect(downs.length).toBeGreaterThanOrEqual(2);
    // Steps under steady overload are spaced by cooldown + a fresh window
    // (small margin for the discrete 30 fps feed grid).
    expect(downs[1].atMs - downs[0].atMs).toBeGreaterThanOrEqual(COOLDOWN_MS + WINDOW_MS - 50);
  });
});

describe('FallbackController step-up probe', () => {
  it('emits stepUp after a sustained healthy period, never stepDown', () => {
    const h = new Harness(new FallbackController());
    h.feed(UP_PROBE_MS + 2_000);
    expect(h.stepDowns()).toHaveLength(0);
    expect(h.stepUps()).toHaveLength(1);
    expect(h.stepUps()[0].atMs).toBeGreaterThanOrEqual(UP_PROBE_MS);
  });

  it('requires observed healthy frames — an idle stall does not count as health', () => {
    const h = new Harness(new FallbackController());
    h.feed(5_000);
    h.idle(UP_PROBE_MS); // no frames flowing
    h.feed(5_000);
    expect(h.stepUps()).toHaveLength(0);
  });

  it('resets the healthy streak on any rejection above the recovery ratio', () => {
    const h = new Harness(new FallbackController());
    h.feed(UP_PROBE_MS - 5_000);
    h.feed(1_000, { rejectRatio: 0.5 }); // pressure blip, below trigger
    h.feed(10_000);
    expect(h.stepUps()).toHaveLength(0);
  });

  it('doubles the probe interval when a step-down follows a step-up (failed probe)', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(UP_PROBE_MS + 2_000); // → stepUp
    expect(h.stepUps()).toHaveLength(1);
    h.feed(14_000, { rejectRatio: 1 }); // cooldown, then full window → stepDown
    expect(h.stepDowns()).toHaveLength(1);
    expect(c.probeIntervalMs).toBe(UP_PROBE_MS * 2);
    // The next probe respects the doubled interval.
    h.feed(UP_PROBE_MS + COOLDOWN_MS + 2_000);
    expect(h.stepUps()).toHaveLength(1);
    h.feed(UP_PROBE_MS + 2_000);
    expect(h.stepUps()).toHaveLength(2);
  });

  it('caps the probe interval at UP_PROBE_MAX_MS', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    let interval = UP_PROBE_MS;
    // Repeated up-probe → immediate overload cycles double toward the cap.
    for (let i = 0; i < 6; i++) {
      h.feed(interval + COOLDOWN_MS + 2_000);
      h.feed(14_000, { rejectRatio: 1 });
      interval = Math.min(interval * 2, UP_PROBE_MAX_MS);
      expect(c.probeIntervalMs).toBe(interval);
    }
    expect(c.probeIntervalMs).toBe(UP_PROBE_MAX_MS);
  });

  it('resets the probe interval after a probe survives the fail window', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(UP_PROBE_MS + 2_000); // → stepUp (probe #1)
    h.feed(14_000, { rejectRatio: 1 }); // probe fails → 60s interval
    expect(c.probeIntervalMs).toBe(UP_PROBE_MS * 2);
    h.feed(UP_PROBE_MS * 2 + COOLDOWN_MS + 2_000); // → stepUp (probe #2)
    expect(h.stepUps()).toHaveLength(2);
    // Probe #2 survives: healthy well past the fail window resolves it.
    h.feed(UP_PROBE_MS * 2 + 5_000);
    expect(c.probeIntervalMs).toBe(UP_PROBE_MS);
  });
});

describe('FallbackController step rejection latches', () => {
  it('suppresses repeated stepDown after stepRejected("down") until recovery', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(5_000, { rejectRatio: 1 }); // → stepDown at ~4s
    expect(h.stepDowns()).toHaveLength(1);
    c.stepRejected('down'); // pipeline: already at the floor
    h.feed(30_000, { rejectRatio: 1 });
    expect(h.stepDowns()).toHaveLength(1); // latched: no re-fire
    // Recovery still emits stepUp, which clears the floor latch…
    h.feed(UP_PROBE_MS + WINDOW_MS + 2_000);
    expect(h.stepUps()).toHaveLength(1);
    // …so renewed sustained overload can step down again.
    h.feed(14_000, { rejectRatio: 1 });
    expect(h.stepDowns()).toHaveLength(2);
  });

  it('suppresses repeated stepUp after stepRejected("up") until a stepDown', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(UP_PROBE_MS + 2_000); // → stepUp
    expect(h.stepUps()).toHaveLength(1);
    c.stepRejected('up'); // pipeline: already at the ceiling
    h.feed(UP_PROBE_MS * 3);
    expect(h.stepUps()).toHaveLength(1); // latched: no repeat probes
    // Overload clears the ceiling latch via the stepDown. The window is
    // already full of healthy outcomes here, so the trigger ratio is
    // reached ~1s in; keep the overload shorter than cooldown + window so
    // exactly one step fits.
    h.feed(6_000, { rejectRatio: 1 });
    expect(h.stepDowns()).toHaveLength(1);
  });

  it('does not count a rejected step-up as a probe for backoff purposes', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(UP_PROBE_MS + 2_000); // → stepUp (rejected below)
    c.stepRejected('up');
    h.feed(14_000, { rejectRatio: 1 }); // stepDown soon after — but no probe was applied
    expect(h.stepDowns()).toHaveLength(1);
    expect(c.probeIntervalMs).toBe(UP_PROBE_MS);
  });
});

describe('FallbackController noteReset', () => {
  it('clears the window and enters cooldown on a pipeline-initiated reset', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(3_500, { rejectRatio: 1 }); // almost a full window of rejections
    c.noteReset(h.nowMs);
    const resetAt = h.nowMs;
    h.feed(14_000, { rejectRatio: 1 });
    const downs = h.stepDowns();
    expect(downs).toHaveLength(1);
    // Nothing before reset + cooldown + a fresh full window.
    expect(downs[0].atMs).toBeGreaterThanOrEqual(resetAt + COOLDOWN_MS + WINDOW_MS - 50);
  });
});

describe('FallbackController encoder errors', () => {
  it('steps down on a first error, fails on a second inside the fail window', () => {
    const c = new FallbackController();
    expect(c.onEncoderError(1_000)).toBe('stepDown');
    expect(c.onEncoderError(1_000 + ERROR_FAIL_WINDOW_MS - 1)).toBe('fail');
  });

  it('steps down again for errors spaced beyond the fail window', () => {
    const c = new FallbackController();
    expect(c.onEncoderError(1_000)).toBe('stepDown');
    expect(c.onEncoderError(2_000 + ERROR_FAIL_WINDOW_MS)).toBe('stepDown');
  });

  it('fails on an error while latched at the floor', () => {
    const c = new FallbackController();
    c.stepRejected('down');
    expect(c.onEncoderError(1_000)).toBe('fail');
  });

  it('enters cooldown after an error step-down like any other step', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    expect(c.onEncoderError(h.nowMs)).toBe('stepDown');
    h.feed(COOLDOWN_MS - 1_000, { rejectRatio: 1 }); // discarded
    h.feed(WINDOW_MS - 1_000, { rejectRatio: 1 }); // window not yet full
    expect(h.stepDowns()).toHaveLength(0);
    h.feed(3_000, { rejectRatio: 1 });
    expect(h.stepDowns()).toHaveLength(1);
  });

  it('counts an error step-down as a failed probe when it follows a step-up', () => {
    const c = new FallbackController();
    const h = new Harness(c);
    h.feed(UP_PROBE_MS + 2_000); // → stepUp
    expect(h.stepUps()).toHaveLength(1);
    expect(c.onEncoderError(h.nowMs + 1_000)).toBe('stepDown');
    expect(c.probeIntervalMs).toBe(UP_PROBE_MS * 2);
  });
});
