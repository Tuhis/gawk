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
//    (audio timestamp at the playhead), both on the broadcaster's clock
//    (Decision 3), so the metric is a subtraction and not a negotiation.
//    Positive = audio behind video.
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
const PLAYHEAD_STALE_MS = 1500;
// How fast the audio-derived mapping may move (ms of correction per second).
// Slew-limiting is what keeps video targets from stepping when the audio
// clock is re-anchored underneath us.
const MAPPING_SLEW_MS_PER_S = 20;

export interface PlayheadReport {
  // Broadcaster-clock µs at the sink's play position; null before the first
  // audio actually reaches the speaker.
  playheadUs: number | null;
  // performance.timeOrigin + performance.now() in the *reporting* context.
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

// Converts a reporting context's absolute epoch ms into this context's
// performance.now() domain. Worker and main thread have different
// timeOrigins; without this the mapping is off by their creation gap.
function toLocalMs(epochMs: number): number {
  return epochMs - timeOriginMs();
}

// Feeds one ~4 Hz report from the audio sink. Cheap and allocation-free —
// this runs on the same channel as the stats flow, in reverse.
export function notePlayhead(report: PlayheadReport, nowMs: number = performance.now()): void {
  const localMs = toLocalMs(report.atEpochMs);
  lastReportLocalMs = nowMs;
  if (report.playheadUs === null) {
    // Audio exists but nothing has played yet: no clock to be master of.
    mapping = null;
    return;
  }
  const next: Mapping = { anchorLocalMs: localMs, anchorUs: report.playheadUs };
  if (mapping === null) {
    mapping = next;
    return;
  }
  // Slew-limit: correct toward the new anchor rather than jumping to it, so
  // a re-anchored audio clock can't step every video target at once.
  const predictedUs = mapping.anchorUs + (localMs - mapping.anchorLocalMs) * 1000;
  const errorMs = (report.playheadUs - predictedUs) / 1000;
  const elapsedS = Math.max(0, (localMs - mapping.anchorLocalMs) / 1000);
  const maxCorrectionMs = Math.max(1, MAPPING_SLEW_MS_PER_S * elapsedS);
  const correctionMs = Math.max(-maxCorrectionMs, Math.min(maxCorrectionMs, errorMs));
  mapping = {
    anchorLocalMs: localMs,
    anchorUs: predictedUs + correctionMs * 1000,
  };
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
    return null;
  }
  const playheadNowUs = mapping.anchorUs + (nowMs - mapping.anchorLocalMs) * 1000;
  lastSkewMs = (frameTimestampUs - playheadNowUs) / 1000;
  return lastSkewMs;
}

export function getAvSkewMs(): number | null {
  return lastSkewMs;
}

// Broadcaster restart / reconnect / audio flush: the old anchor belongs to a
// dead timeline (docs/20 Decision 8).
export function resetAvSync(): void {
  mapping = null;
  lastReportLocalMs = null;
  lastSkewMs = null;
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
