// A/V sync — "good-enough", **video-master** (docs/20 Decision 10, revised
// 2026-07-20; the original made audio the master in paced modes).
//
// Video is the master clock and is never rescheduled for audio's sake. Audio
// is the medium with slack to give: one Opus packet per datagram, no
// chunking, no reassembly, no keyframe wait and a trivial decode, so it
// arrives materially earlier than video, which pays all of those plus the
// playout offset. So audio waits for video, not the reverse.
//
// 1. Skew is measured ALWAYS: avSkewMs = (video timestamp presented) −
//    (audio timestamp **at the listener**), both on the broadcaster's clock
//    (Decision 3), so the metric is a subtraction and not a negotiation.
//    Positive = audio behind video.
//
//    "At the listener" is load-bearing, not pedantry (docs/20 field finding
//    13). The worklet's playhead is the sample it is *writing* into the output
//    buffer, which the device plays `outputLatency` later; measuring there
//    makes a perfectly synced stream read −outputLatency, and step 3's trim
//    then dutifully drives that to zero — i.e. walks audio `outputLatency`
//    late and calls it success. The sink converts to the heard position
//    before reporting, because it is the only context that owns the
//    AudioContext; everything here is in listener terms.
// 2. Alignment is set ONCE, at the start of playback: audio-buffer.ts holds
//    the first chunk until the video presentation schedule says it is due
//    (docs/20 field finding 4). After that the worklet consumes exactly
//    sampleRate samples per second at 1×, so *no amount of buffering can
//    change when a given sample is heard* — holding chunks longer only
//    changes queue depth. Alignment is a start-time decision.
// 3. Which leaves drift (clock + soundcard, tens of ppm ≈ 100 ms/hour) to a
//    slow playback-rate trim: AudioRateController below turns measured skew
//    into a sub-audible rate, never a step, never a skip.
//
// Module state, like playout.ts: the pipeline reads it live, and the main
// thread pushes playhead reports in (the sink owns an AudioContext, which
// cannot exist in a worker). Cross-context clocks are handled by carrying
// absolute epoch ms and rebasing here — the same discipline viewer.ts uses
// for the nested transport worker's TimeSync samples.

import { timeOriginMs } from './time-sync';

// A playhead report older than this is not a clock: audio stalled, the tab
// slept, or the sink died. Video falls back to the arrival baseline.
//
// It also bounds how far the mapping may be **extrapolated**, which is the
// part that had to shrink (docs/20 field finding 12). The worklet reports at
// 4 Hz; past a couple of intervals its position is not known, and at 1500 ms
// the module would invent up to 1.5 s of skew out of an assumption — on a
// congested main thread, which is exactly the "stressed session" the field
// report came from. Three report intervals leaves room for ordinary scheduling
// jitter and calls anything beyond it unknown, which is a better answer than a
// guess with no error bar.
const PLAYHEAD_STALE_MS = 750;
// How long the advance ratio looks back. Long enough to span several 4 Hz
// reports (so one dry quantum does not read as a stall), short enough to track
// a storm as it develops.
const PLAYHEAD_ADVANCE_WINDOW_MS = 1000;
// Below this much of the window, the ratio is computed from too short a span
// to mean anything and reads null.
const PLAYHEAD_ADVANCE_MIN_SPAN_MS = 500;
// How long a presentation sample stays a measurement. `lastSkewMs` is written
// only where a frame is actually presented, but it is *read* on the stats tick
// and fed to the drift trim — so when presentation stalls (a hidden tab, an
// occluded window, worker rAF throttled) while audio keeps playing, the trim
// would integrate a frozen number open-loop, adding real, permanent delay
// nothing is measuring (docs/20 field finding 13). Comfortably above one
// stats tick (~500 ms) and any single dropped frame, well below the point
// where the error would matter.
const PRESENTATION_STALE_MS = 1000;

export interface PlayheadReport {
  // Broadcaster-clock µs of the audio **reaching the listener** at atEpochMs —
  // not the worklet's write position, which is outputLatency ahead of it (see
  // the header). Null before the first audio actually reaches the speaker.
  heardUs: number | null;
  // performance.timeOrigin + performance.now() in the *reporting* context, for
  // the moment heardUs is at the listener.
  atEpochMs: number;
}

interface Mapping {
  // At local time anchorLocalMs (this context's performance.now()), the
  // speaker was playing broadcaster timestamp anchorUs.
  anchorLocalMs: number;
  anchorUs: number;
}

let mapping: Mapping | null = null;
let lastReportLocalMs: number | null = null;
let lastSkewMs: number | null = null;
// When lastSkewMs was written, i.e. when a frame was last presented.
let lastSkewAtMs: number | null = null;
// A ~1 s trail of reports, for the advance ratio. Two entries is the steady
// state at 4 Hz over the window; the array never exceeds a handful.
const advanceTrail: { localMs: number; heardUs: number }[] = [];
let advanceRatio: number | null = null;

// Converts a reporting context's absolute epoch ms into this context's
// performance.now() domain. Worker and main thread have different
// timeOrigins; without this the mapping is off by their creation gap.
function toLocalMs(epochMs: number): number {
  return epochMs - timeOriginMs();
}

// Feeds one ~4 Hz report from the audio sink. Cheap and allocation-free —
// this runs on the same channel as the stats flow, in reverse.
//
// The report is **ground truth**, so it becomes the anchor as it stands
// (docs/20 field finding 12). It used to be low-passed toward the mapping's
// own prediction at 20 ms/s, with a snap above 250 ms added by finding 9 — but
// smoothing an exact measurement can only add error, and the error it added
// was one-directional for as long as the playhead kept moving faster than the
// cap: a buffer skipping holes, a re-prime jumping to live. That left a
// standing over-report (~33 ms in the skip pattern the regression test drives)
// that no consumer could see or bound, on top of the finding-9 case the snap
// had already patched around. Between reports the worklet drains at exactly
// 1×, so extrapolation over one report interval is worth ~1 ms; past
// PLAYHEAD_STALE_MS it is worth nothing and the clock reads unavailable.
export function notePlayhead(report: PlayheadReport, nowMs: number = performance.now()): void {
  const localMs = toLocalMs(report.atEpochMs);
  lastReportLocalMs = nowMs;
  if (report.heardUs === null) {
    // Audio exists but nothing has played yet: no clock to be master of.
    mapping = null;
    advanceTrail.length = 0;
    advanceRatio = null;
    return;
  }
  mapping = { anchorLocalMs: localMs, anchorUs: report.heardUs };
  noteAdvance(localMs, report.heardUs);
}

// Tracks how fast the audio *timeline* is moving against the wall clock, which
// is what separates the two things `avSkewMs` has always conflated (docs/20
// field finding 12): a ratio at ~1 means the audio is playing normally and any
// skew is a genuine lip-sync offset; a ratio below 1 means the worklet is
// starving and the skew is accumulating starvation debt at exactly
// (1 − ratio) per second — the 0.934 that produced the field capture's 1986 ms
// over 30 s. Deliberately reported rather than used to suppress the skew: when
// audio really has fallen behind, that reading is true and hiding it would be
// the same mistake in the other direction.
function noteAdvance(localMs: number, heardUs: number): void {
  advanceTrail.push({ localMs, heardUs });
  // Keep the oldest sample that is still at least a window old, so the span
  // straddles PLAYHEAD_ADVANCE_WINDOW_MS rather than falling short of it.
  while (
    advanceTrail.length > 2 &&
    localMs - advanceTrail[1]!.localMs >= PLAYHEAD_ADVANCE_WINDOW_MS
  ) {
    advanceTrail.shift();
  }
  const oldest = advanceTrail[0]!;
  const spanMs = localMs - oldest.localMs;
  advanceRatio =
    spanMs >= PLAYHEAD_ADVANCE_MIN_SPAN_MS ? (heardUs - oldest.heardUs) / 1000 / spanMs : null;
}

// How fast the audio timeline advanced against the wall clock over the last
// ~1 s: 1 = playing normally, 0 = frozen. Null until there is enough span to
// say. Read it beside avSkewMs — together they say whether a skew is a stable
// lip-sync offset or a debt still being accrued.
export function getPlayheadAdvanceRatio(): number | null {
  return advanceRatio;
}

// The audio clock is only a master while it is fresh. Everything else falls
// back to the arrival baseline — exactly today's behavior.
export function audioClockAvailable(nowMs: number = performance.now()): boolean {
  return (
    mapping !== null && lastReportLocalMs !== null && nowMs - lastReportLocalMs <= PLAYHEAD_STALE_MS
  );
}

// avSkewMs at the moment a video frame is presented: positive = video ahead
// of audio (the perceptually forgiving direction, and the expected sign in
// live-edge mode, where video is undelayed and audio carries a jitter
// buffer). Null when there is no audio clock.
export function observeVideoPresented(
  frameTimestampUs: number,
  nowMs: number = performance.now(),
): number | null {
  if (!audioClockAvailable(nowMs) || mapping === null) {
    lastSkewMs = null;
    lastSkewAtMs = null;
    return null;
  }
  const heardNowUs = mapping.anchorUs + (nowMs - mapping.anchorLocalMs) * 1000;
  lastSkewMs = (frameTimestampUs - heardNowUs) / 1000;
  lastSkewAtMs = nowMs;
  return lastSkewMs;
}

// The last measured skew, or null once it is too old to be one. The staleness
// bound matters because the consumer is the drift trim: a frozen reading is
// not a small error, it is an open loop (docs/20 field finding 13).
export function getAvSkewMs(nowMs: number = performance.now()): number | null {
  if (lastSkewAtMs === null) return null;
  return nowMs - lastSkewAtMs > PRESENTATION_STALE_MS ? null : lastSkewMs;
}

// Broadcaster restart / reconnect / audio flush: the old anchor belongs to a
// dead timeline (docs/20 Decision 8).
export function resetAvSync(): void {
  mapping = null;
  lastReportLocalMs = null;
  lastSkewMs = null;
  lastSkewAtMs = null;
  advanceTrail.length = 0;
  advanceRatio = null;
}

// ── Drift trim (docs/20 Decision 10 revised) ──────────────────────────────
//
// Alignment is fixed at playback start, so the only lever left is playback
// *rate*. Clock and soundcard crystals differ by tens of ppm, which is ~100 ms
// of drift per hour — slow, but a two-hour session ends visibly out of lip
// sync without correction.
//
// The constraint is that the correction must be inaudible: no steps, no
// skips, no resampling artifacts. Pitch shift is imperceptible well below
// 0.5% (a semitone is ~6%), so the trim stays inside ±0.4% and slews there
// over seconds. That absorbs 100 ms of skew in ~25 s at full trim — far
// slower than the drift accumulates, which is the whole point.

// Skew below this is left alone: hunting around zero would modulate pitch for
// no perceptible gain.
export const RATE_TRIM_DEADBAND_MS = 20;
// Maximum |rate − 1|. Chosen for inaudibility, not for correction speed.
export const RATE_TRIM_MAX = 0.004;
// How fast the trim itself may change (per second). Slewing the *rate* keeps
// even the onset of a correction from being heard as a pitch step.
export const RATE_TRIM_SLEW_PER_S = 0.0008;
// Skew this large is not drift — it's a re-anchor (restart, reconnect, a
// device change). The trim gives up rather than grinding at max rate for
// minutes; audio-buffer.ts's flush path owns that case.
export const RATE_TRIM_GIVE_UP_MS = 2000;

export class AudioRateController {
  private rate = 1;

  // Feed the latest measured skew (positive = audio behind video). Returns
  // the rate the sink should play at.
  update(skewMs: number | null, nowMs: number): number {
    const dtMs = this.lastAtMs === null ? 0 : Math.max(0, nowMs - this.lastAtMs);
    this.lastAtMs = nowMs;
    if (skewMs === null || Math.abs(skewMs) > RATE_TRIM_GIVE_UP_MS) {
      return this.slewToward(1, dtMs);
    }
    if (Math.abs(skewMs) <= RATE_TRIM_DEADBAND_MS) return this.slewToward(1, dtMs);
    // Audio behind (skew > 0) ⇒ play faster to catch up, and vice versa.
    // Proportional inside the clamp: a big skew earns the full trim, a small
    // one a gentle nudge that settles instead of overshooting.
    const desired = 1 + Math.max(-RATE_TRIM_MAX, Math.min(RATE_TRIM_MAX, skewMs / 250_000));
    return this.slewToward(desired, dtMs);
  }

  private lastAtMs: number | null = null;

  private slewToward(desired: number, dtMs: number): number {
    const maxStep = (RATE_TRIM_SLEW_PER_S * dtMs) / 1000;
    const delta = desired - this.rate;
    this.rate += Math.max(-maxStep, Math.min(maxStep, delta));
    return this.rate;
  }

  current(): number {
    return this.rate;
  }

  reset(): void {
    this.rate = 1;
    this.lastAtMs = null;
  }
}
