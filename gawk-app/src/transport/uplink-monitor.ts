// Whether to tell the browser broadcaster "Your upload can't keep up"
// (docs/75 RU9, §6). The native monitor's rule exactly
// (gawk-broadcast-desktop/crates/engine/src/uplink.rs), over the browser's
// own counts: pure arithmetic over one sample a second, no clocks.
//
// A second is bad on any stale drop, video or audio (the engine's "frames
// dropped at send"), on two or more keyframe streams superseded or failed
// (keyframes run about two a second, so that is essentially every one
// running late), or on a sent rate under 75 % of the encode rate above
// 20 fps. Five bad seconds raise the notice — one slow keyframe is weather,
// not climate — and fifteen good ones clear it, because a flapping notice is
// worse than a sticky one.
//
// It is not a ladder signal: nothing here reaches FallbackController. The
// notice asks the broadcaster to choose a lower quality instead.

export const UPLINK_WARN_AFTER_BAD_SECONDS = 5;
export const UPLINK_CLEAR_AFTER_GOOD_SECONDS = 15;

// One second's reading. The counters are cumulative; the rates are over the
// second just ended.
export interface UplinkSample {
  staleDropped: number;
  keyframeStreamsSuperseded: number;
  keyframeStreamsFailed: number;
  encoderFps: number;
  sentFps: number;
}

export class UplinkMonitor {
  private last: UplinkSample | null = null;
  private badStreak = 0;
  private goodStreak = 0;
  private warned = false;

  // One 1 Hz sample; returns whether the notice shows.
  observe(s: UplinkSample): boolean {
    const prev = this.last;
    this.last = { ...s };
    if (!prev) return this.warned; // first sample: no deltas yet

    const kfPressure =
      Math.max(0, s.keyframeStreamsSuperseded - prev.keyframeStreamsSuperseded) +
      Math.max(0, s.keyframeStreamsFailed - prev.keyframeStreamsFailed);
    const dropped = Math.max(0, s.staleDropped - prev.staleDropped);
    const bad = kfPressure >= 2 || dropped > 0 || (s.encoderFps > 20 && s.sentFps < s.encoderFps * 0.75);

    if (bad) {
      this.badStreak++;
      this.goodStreak = 0;
      if (this.badStreak >= UPLINK_WARN_AFTER_BAD_SECONDS) this.warned = true;
    } else {
      this.goodStreak++;
      this.badStreak = 0;
      if (this.goodStreak >= UPLINK_CLEAR_AFTER_GOOD_SECONDS) this.warned = false;
    }
    return this.warned;
  }
}
