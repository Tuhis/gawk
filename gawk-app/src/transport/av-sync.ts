// R15 N5 (docs/20 Decision 10): A/V sync — "good-enough", in three layers.
//
// 1. Skew is measured ALWAYS: avSkewMs = (video timestamp presented) −
//    (audio timestamp at the playhead), both on the broadcaster's clock
//    (Decision 3), so the metric is a subtraction and not a negotiation.
// 2. Live-edge mode (playout 'off'): video is never delayed. The audio
//    jitter buffer is the only adaptive knob (audio-buffer.ts).
// 3. Paced modes ('fixed'/'adaptive'): audio becomes the master clock for
//    video display targets — slew-limited so targets never step, with the
//    reorder buffer's arrival baseline as the fallback the moment audio is
//    absent, stale, or the context is suspended.
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

// The local time (this context's performance.now() ms) at which a frame with
// this broadcaster timestamp should be displayed to sit with the audio.
// Null whenever audio can't be the master — the caller then uses the arrival
// baseline.
export function audioDisplayTargetMs(
  frameTimestampUs: number,
  offsetMs: number,
  nowMs: number = performance.now(),
): number | null {
  if (!audioClockAvailable(nowMs) || mapping === null) return null;
  // Where the speaker is now, extrapolated from the anchor.
  const playheadNowUs = mapping.anchorUs + (nowMs - mapping.anchorLocalMs) * 1000;
  // A frame ahead of the playhead waits exactly that long, plus the playout
  // offset the mode asked for.
  return nowMs + (frameTimestampUs - playheadNowUs) / 1000 + offsetMs;
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
