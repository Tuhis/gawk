// A/V sync (docs/20 Decision 10, revised 2026-07-20 to video-master): skew
// measurement on synthetic clocks, the staleness bound, and the drift trim
// that is the only lever left once playback has started.

import { beforeEach, describe, expect, it } from 'vitest';

import {
  AudioRateController,
  RATE_TRIM_DEADBAND_MS,
  RATE_TRIM_GIVE_UP_MS,
  RATE_TRIM_MAX,
  audioClockAvailable,
  getAvSkewMs,
  getPlayheadAdvanceRatio,
  notePlayhead,
  observeVideoPresented,
  resetAvSync,
} from './av-sync';

// The module carries the reporting context's absolute epoch; in tests both
// contexts share performance.timeOrigin, so epoch = timeOrigin + localMs.
function epochFor(localMs: number): number {
  return performance.timeOrigin + localMs;
}

beforeEach(() => resetAvSync());

describe('av-sync playhead mapping', () => {
  it('has no audio clock until a playhead with real audio arrives', () => {
    expect(audioClockAvailable(0)).toBe(false);
    // Audio exists but nothing has played yet: still no clock.
    notePlayhead({ heardUs: null, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(false);

    notePlayhead({ heardUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(true);
  });

  it('computes skew as video − audio on the shared broadcaster clock', () => {
    // At local t=0 the speaker is playing broadcaster timestamp 1.000 s.
    notePlayhead({ heardUs: 1_000_000, atEpochMs: epochFor(0) }, 0);

    // A frame stamped 1.050 s presented right now: video is 50 ms ahead.
    expect(observeVideoPresented(1_050_000, 0)).toBeCloseTo(50, 3);
    expect(getAvSkewMs()).toBeCloseTo(50, 3);

    // A frame stamped 0.980 s: video is 20 ms behind.
    expect(observeVideoPresented(980_000, 0)).toBeCloseTo(-20, 3);

    // 100 ms later the playhead has advanced by 100 ms too, so the same
    // frame timestamp reads 100 ms more behind.
    expect(observeVideoPresented(1_050_000, 100)).toBeCloseTo(-50, 3);
  });

  it('perfect pacing holds skew at ~0 as both clocks advance', () => {
    notePlayhead({ heardUs: 0, atEpochMs: epochFor(0) }, 0);
    for (let t = 0; t <= 2000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
      const skew = observeVideoPresented(t * 1000, t);
      expect(Math.abs(skew!)).toBeLessThan(1);
    }
  });

  it('stops being a clock once reports go stale', () => {
    notePlayhead({ heardUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(600)).toBe(true);
    expect(audioClockAvailable(900)).toBe(false);
    expect(observeVideoPresented(1_000_000, 900)).toBeNull();
    expect(getAvSkewMs(900)).toBeNull();
  });

  it('reset clears the mapping (restart: the old anchor is a dead timeline)', () => {
    notePlayhead({ heardUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(audioClockAvailable(0)).toBe(true);
    resetAvSync();
    expect(audioClockAvailable(0)).toBe(false);
    expect(getAvSkewMs()).toBeNull();
  });
});

// A re-anchor (the audio jitter buffer under-runs, re-primes, and resumes at
// the live edge) moves the playhead discontinuously by hundreds of ms to
// seconds. The mapping used to slew toward each report at 20 ms/s, so it left
// the skew reading ~2 s and creeping for the ~100 s it took to reconverge (the
// field capture: 1939→2130 over 9.5 s = exactly 20 ms/s); finding 9 patched
// that with a snap above 250 ms, and finding 12 removed the smoothing
// altogether — every report is the anchor. These cases stay pinned because
// they are the ones that hurt.
describe('av-sync follows the playhead wherever it goes', () => {
  it('tracks a 2 s playhead jump in one report (was ~2000 ms of slew lag)', () => {
    // Converge: at local 5000 ms the speaker is playing broadcaster ts 5.000 s.
    for (let t = 0; t <= 5000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    expect(observeVideoPresented(5_000_000, 5000)!).toBeCloseTo(0, 0);

    // The audio buffer re-primes after a deep stall and resumes at the live
    // edge: the next report's playhead is 2 s ahead of where the mapping was.
    const t = 5250;
    const jumped = (t + 2000) * 1000;
    notePlayhead({ heardUs: jumped, atEpochMs: epochFor(t) }, t);

    // A frame at the new live edge reads ~0. Pre-fix the mapping corrected
    // only 5 ms of the 2 s gap, so it read ~1995 ms.
    expect(Math.abs(observeVideoPresented(jumped, t)!)).toBeLessThan(50);
  });

  it('does not let a frozen playhead ramp the skew at the slew rate', () => {
    for (let t = 0; t <= 5000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    // The worklet under-runs: its playhead freezes while the wall clock (and
    // the audio that eventually resumes) keep moving. Pre-fix, each stale
    // report dragged the mapping down 5 ms and the skew climbed 20 ms/s.
    const frozen = 5000 * 1000;
    for (let t = 5250; t <= 8000; t += 250) {
      notePlayhead({ heardUs: frozen, atEpochMs: epochFor(t) }, t);
    }
    // Audio resumes at the live edge; the very next report must re-align.
    const t = 8250;
    const resumed = (t - 30) * 1000; // 30 ms behind live, healthy
    notePlayhead({ heardUs: resumed, atEpochMs: epochFor(t) }, t);
    expect(observeVideoPresented(t * 1000, t)!).toBeCloseTo(30, 0);
  });

  it('stays within the arrival jitter it is handed', () => {
    notePlayhead({ heardUs: 0, atEpochMs: epochFor(0) }, 0);
    // Reports land with ±40 ms arrival jitter while the playhead advances at
    // 1×. Anchoring on each report puts that jitter straight into the reading
    // — bounded by the jitter itself, where smoothing it traded a bounded
    // noise for an unbounded bias (finding 12). The sink's getOutputTimestamp
    // path removes most of this jitter at the source anyway, and the trim's
    // 20 ms deadband absorbs the rest.
    for (let k = 1; k <= 40; k++) {
      const arrive = k * 250 + (k % 2 === 0 ? 40 : -40);
      notePlayhead({ heardUs: k * 250 * 1000, atEpochMs: epochFor(arrive) }, arrive);
    }
    const nowMs = 40 * 250;
    // A frame at the true current playhead reads within jitter of 0 — smoothed,
    // not snapped to a noisy instantaneous sample.
    expect(Math.abs(observeVideoPresented(nowMs * 1000, nowMs)!)).toBeLessThan(60);
  });
});

// The video-master guarantee (docs/20 Decision 10 revised, field finding 4):
// av-sync measures, and nothing more. It exports no way to reschedule video,
// so no audio state — fresh, stale, or absent — can move a video frame. That
// is the property the revision buys, and this pins it at the module surface.
describe('av-sync cannot reschedule video', () => {
  it('measures skew without exposing any video-side lever', async () => {
    notePlayhead({ heardUs: 500_000, atEpochMs: epochFor(0) }, 0);
    expect(observeVideoPresented(560_000, 0)).toBeCloseTo(60, 3);

    const surface = Object.keys(await import('./av-sync'));
    expect(surface).not.toContain('audioDisplayTargetMs');
    expect(surface).not.toContain('audioBaselineMs');
  });
});

// Drift is what remains after the start-time alignment (field finding 4):
// the worklet runs at 1×, so nothing else can move audio relative to video.
describe('AudioRateController', () => {
  const ctl = () => new AudioRateController();

  it('leaves small skew alone rather than modulating pitch for nothing', () => {
    const c = ctl();
    for (let t = 0; t <= 20_000; t += 250) c.update(RATE_TRIM_DEADBAND_MS - 5, t);
    expect(c.current()).toBe(1);
  });

  it('speeds up when audio is behind and slows when ahead, within the audible bound', () => {
    const behind = ctl();
    for (let t = 0; t <= 60_000; t += 250) behind.update(600, t);
    expect(behind.current()).toBeGreaterThan(1);
    expect(behind.current()).toBeLessThanOrEqual(1 + RATE_TRIM_MAX);

    const ahead = ctl();
    for (let t = 0; t <= 60_000; t += 250) ahead.update(-600, t);
    expect(ahead.current()).toBeLessThan(1);
    expect(ahead.current()).toBeGreaterThanOrEqual(1 - RATE_TRIM_MAX);
  });

  // The user requirement in one assertion: corrections are spread over a long
  // period. A step would be audible as a pitch jump.
  it('never steps — the rate itself is slew-limited', () => {
    const c = ctl();
    let prev = 1;
    for (let t = 250; t <= 10_000; t += 250) {
      const next = c.update(2000 - 1, t);
      expect(Math.abs(next - prev)).toBeLessThan(0.0005);
      prev = next;
    }
  });

  it('gives up on a skew too large to be drift, and returns toward 1x', () => {
    const c = ctl();
    for (let t = 0; t <= 60_000; t += 250) c.update(500, t);
    expect(c.current()).toBeGreaterThan(1);
    // A re-anchor jump: audio-buffer's flush owns this, not a rate grind.
    for (let t = 60_000; t <= 120_000; t += 250) c.update(RATE_TRIM_GIVE_UP_MS + 1, t);
    expect(c.current()).toBeCloseTo(1, 5);
  });

  it('holds at 1x while skew is unmeasurable', () => {
    const c = ctl();
    for (let t = 0; t <= 10_000; t += 250) c.update(null, t);
    expect(c.current()).toBe(1);
  });
});

// docs/20 field finding 13 (2026-07-26). Two halves of one root: the skew
// metric is the ONLY long-run determinant of where audio sits (alignment is a
// start-time decision and the trim integrates from there), so both what it
// measures and when it is allowed to be believed are load-bearing.
describe('av-sync skew is only believed while it is a measurement', () => {
  // The trim consumes `getAvSkewMs()` off the ~2 Hz stats tick, but the value
  // is only written where a frame is PRESENTED. Throttle presentation — a
  // hidden tab, an occluded window, worker rAF stalling — and audio keeps
  // reporting while the video side goes silent, so the last skew freezes and
  // the trim integrates it open-loop. The field capture that found this had
  // `renderedFps: 0` for 6.5 s with `avSkewMs` pinned at -61.5; at that error
  // the trim adds ~0.25 ms of real delay per second, forever, with nothing
  // measuring the result.
  it('stops reporting a skew once video presentation stalls', () => {
    notePlayhead({ heardUs: 1_000_000, atEpochMs: epochFor(0) }, 0);
    expect(observeVideoPresented(1_000_000, 0)).toBeCloseTo(0, 3);
    expect(getAvSkewMs(0)).toBeCloseTo(0, 3);

    // Audio stays healthy — it is video that stopped being presented.
    notePlayhead({ heardUs: 2_000_000, atEpochMs: epochFor(1000) }, 1000);
    expect(getAvSkewMs(1000)).toBeCloseTo(0, 3); // still fresh enough

    notePlayhead({ heardUs: 3_000_000, atEpochMs: epochFor(2000) }, 2000);
    expect(audioClockAvailable(2000)).toBe(true);
    expect(getAvSkewMs(2000)).toBeNull();

    // A presentation resumes: the metric is live again.
    observeVideoPresented(3_000_000, 2000);
    expect(getAvSkewMs(2000)).toBeCloseTo(0, 3);
  });

  // The setpoint lock. `observeVideoPresented` compares against the sample the
  // sink says is AT THE LISTENER; if it were handed the worklet's write
  // position instead (which is `outputLatency` ahead of the speaker), a
  // perfectly aligned stream would read -outputLatency and this loop would
  // walk audio that far late — 100 ms on a 120 ms device, 280 ms on a 300 ms
  // one — while the overlay showed a clean zero.
  it('leaves a stream aligned at the speaker exactly where it is', () => {
    const controller = new AudioRateController();
    let errorMs = 0; // positive = audio heard behind the picture
    let rate = 1;
    for (let t = 0; t <= 900_000; t += 250) {
      const videoTsMs = t;
      notePlayhead({ heardUs: (videoTsMs - errorMs) * 1000, atEpochMs: epochFor(t) }, t);
      rate = controller.update(observeVideoPresented(videoTsMs * 1000, t), t);
      // Audio advances at `rate` while video advances at 1x, so any rate error
      // integrates straight into lip sync.
      errorMs += (1 - rate) * 250;
    }
    expect(Math.abs(errorMs)).toBeLessThan(1);
  });
});

// docs/20 field finding 12 (the BUGS.md entry): `avSkewMs` read in the
// thousands on long/stressed sessions while audio was near-correct. Driving
// the real module reproduces the recorded ramp exactly — a playhead advancing
// at 0.934x of wall time yields 1986 ms in 30 s, the accelerated capture's
// figure — which means the number was reporting how far the audio TIMELINE had
// fallen behind, three separable pieces of which were the estimator's own
// error rather than anything at the speaker.
describe('av-sync skew is a measurement, not an extrapolation', () => {
  // Piece 1: the mapping slew-limited itself toward each report at 20 ms/s, so
  // any playhead motion faster than that (the buffer skipping a hole, a
  // re-prime jumping to live) left the mapping behind — a standing over-report
  // for as long as the motion continued. The report is exact; smoothing an
  // exact measurement can only add error.
  it('anchors on each report instead of slewing toward it', () => {
    // Audio that is exactly in sync but whose playhead moves in jumps (stall,
    // then skip to live) rather than smoothly. Read the metric AT each report,
    // where the true lateness is known exactly and the mapping has just been
    // handed it — so any deviation is the estimator's own, not the 4 Hz
    // sampling limit (extrapolating across a jump that has not been reported
    // yet is inherent, and is why this is measured at the report and not
    // between two).
    let worst = 0;
    for (let t = 0; t <= 30_000; t += 250) {
      const trueLatenessMs = (t % 1000) / 20; // 0 → 37.5 ms sawtooth
      notePlayhead({ heardUs: (t - trueLatenessMs) * 1000, atEpochMs: epochFor(t) }, t);
      const skew = observeVideoPresented(t * 1000, t);
      if (t > 0) worst = Math.max(worst, Math.abs((skew ?? 0) - trueLatenessMs));
    }
    // Pre-fix the slew could only move 5 ms per report, so it trailed the
    // sawtooth by up to ~20 ms — a standing error no consumer could see,
    // bound, or attribute, on top of whatever the true skew was.
    expect(worst).toBeLessThan(1);
  });

  // Piece 2: with no report, the mapping kept extrapolating at 1x for the whole
  // 1500 ms staleness window — so a congested main thread (exactly the "stressed
  // session" in the report) could have the metric inventing up to 1.5 s of skew
  // out of an assumption. The worklet reports at 4 Hz; past a few intervals the
  // position is not known, and "unknown" is a better answer than a guess.
  it('stops reporting once it has not heard from the playhead', () => {
    for (let t = 0; t <= 2000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    expect(observeVideoPresented(2500 * 1000, 2500)).not.toBeNull(); // 500 ms: fine
    expect(observeVideoPresented(2900 * 1000, 2900)).toBeNull(); // 900 ms: a guess
  });

  // Piece 3 is not an estimator error at all — a starving worklet really does
  // fall behind, and the metric really should say so. What was missing is any
  // way to tell that reading apart from a steady lip-sync offset, which is why
  // three findings have argued over one number. The advance ratio is the
  // discriminator: ~1 means the audio timeline is keeping up and the skew is
  // lip sync; below 1 means the skew is accumulating starvation debt.
  it('reports how fast the audio timeline is advancing', () => {
    expect(getPlayheadAdvanceRatio()).toBeNull(); // no data yet

    for (let t = 0; t <= 3000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    expect(getPlayheadAdvanceRatio()).toBeCloseTo(1, 2);

    // The storm from the capture: the worklet is dry ~7 % of the time.
    resetAvSync();
    for (let t = 0; t <= 30_000; t += 250) {
      notePlayhead({ heardUs: t * 0.934 * 1000, atEpochMs: epochFor(t) }, t);
    }
    expect(getPlayheadAdvanceRatio()).toBeCloseTo(0.934, 2);
    // …and the skew it produces is the debt that ratio predicts, not a
    // constant offset: ~66 ms per second, the recorded 1986 ms over 30 s.
    expect(observeVideoPresented(30_000 * 1000, 30_000)!).toBeCloseTo(1980, -2);
  });

  it('forgets the advance history on a reset', () => {
    for (let t = 0; t <= 3000; t += 250) {
      notePlayhead({ heardUs: t * 1000, atEpochMs: epochFor(t) }, t);
    }
    resetAvSync();
    expect(getPlayheadAdvanceRatio()).toBeNull();
  });
});
