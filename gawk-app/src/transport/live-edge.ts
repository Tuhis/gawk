// Live-edge drift estimation (R5 Q1, docs/15). Frame timestamps are stamped
// on the broadcaster's performance.now() clock at capture (capture.ts), so a
// viewer-side `delta = viewerNow − frameTimestamp` is (unknown clock offset) +
// (true capture→here latency). The minimum delta over a sliding window is the
// session-best baseline; the excess over it is pure accumulated lag — decoder
// backlog, reorder holds, queue growth — with the clock offset cancelled.
//
// The window is what makes this robust: consumer crystal oscillators skew tens
// of ppm (tens to hundreds of ms per hour), which would silently inflate a
// session-long baseline. Absolute latency (offset NOT cancelled) is Q2's job.
//
// Pure and timer-free: the clock is injected, everything is node-testable.

export const LIVE_EDGE_WINDOW_MS = 60_000;
export const LIVE_EDGE_BUCKET_MS = 1000;

interface Bucket {
  start: number;
  min: number;
}

// Sliding-window minimum over a monotonic clock, bucketed so memory stays
// O(window/bucket) regardless of observation rate.
export class WindowedMinTracker {
  private windowMs: number;
  private bucketMs: number;
  private buckets: Bucket[] = [];

  constructor(windowMs = LIVE_EDGE_WINDOW_MS, bucketMs = LIVE_EDGE_BUCKET_MS) {
    this.windowMs = windowMs;
    this.bucketMs = bucketMs;
  }

  observe(value: number, nowMs: number): void {
    this.evict(nowMs);
    const start = Math.floor(nowMs / this.bucketMs) * this.bucketMs;
    const last = this.buckets[this.buckets.length - 1];
    if (last && last.start === start) {
      if (value < last.min) last.min = value;
      return;
    }
    this.buckets.push({ start, min: value });
  }

  min(nowMs: number): number | null {
    this.evict(nowMs);
    if (this.buckets.length === 0) return null;
    let m = Infinity;
    for (const b of this.buckets) {
      if (b.min < m) m = b.min;
    }
    return m;
  }

  reset(): void {
    this.buckets = [];
  }

  private evict(nowMs: number): void {
    const cutoff = nowMs - this.windowMs;
    while (this.buckets.length > 0 && this.buckets[0].start + this.bucketMs <= cutoff) {
      this.buckets.shift();
    }
  }
}

// Per-frame drift over the windowed-min baseline. observe() takes the frame's
// capture timestamp (µs, VideoFrame.timestamp) at the moment the frame reaches
// the measurement point (decoder output — the paint that follows is at most
// one display interval later, a constant the baseline cancels anyway).
export class LiveEdgeTracker {
  private now: () => number;
  private minTracker = new WindowedMinTracker();
  private lastDeltaMs: number | null = null;

  constructor(now: () => number = () => performance.now()) {
    this.now = now;
  }

  observe(timestampUs: number): void {
    const nowMs = this.now();
    const deltaMs = nowMs - timestampUs / 1000;
    this.lastDeltaMs = deltaMs;
    this.minTracker.observe(deltaMs, nowMs);
  }

  // Current lag behind the session-best (ms, >= 0); null before any frame.
  driftMs(): number | null {
    if (this.lastDeltaMs === null) return null;
    const base = this.minTracker.min(this.now());
    if (base === null) return null;
    return Math.max(0, this.lastDeltaMs - base);
  }

  // A broadcaster restart moves timestamps to a new timeline; the old baseline
  // is meaningless against it.
  reset(): void {
    this.minTracker.reset();
    this.lastDeltaMs = null;
  }
}
