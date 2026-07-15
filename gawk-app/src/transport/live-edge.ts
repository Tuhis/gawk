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

// R12 T1 (docs/17): windowed quantile, the sibling of WindowedMinTracker and
// shared by the arrival-jitter metric and the adaptive playout controller —
// measurement and control read the same estimator by design. Each bucket
// holds a fixed-width histogram of (value − bucket min); memory stays
// O(window/bucket × bins) regardless of observation rate. Quantiles are
// accurate to one bin (values past the range clamp into the top bin).
export const QUANTILE_BIN_MS = 4;
export const QUANTILE_RANGE_MS = 500;

interface QuantileBucket {
  start: number;
  min: number; // lower edge of bin 0
  counts: number[];
  total: number;
}

export class WindowedQuantileTracker {
  private windowMs: number;
  private bucketMs: number;
  private binMs: number;
  private numBins: number;
  private buckets: QuantileBucket[] = [];

  constructor(
    windowMs = LIVE_EDGE_WINDOW_MS,
    bucketMs = LIVE_EDGE_BUCKET_MS,
    binMs = QUANTILE_BIN_MS,
    rangeMs = QUANTILE_RANGE_MS,
  ) {
    this.windowMs = windowMs;
    this.bucketMs = bucketMs;
    this.binMs = binMs;
    this.numBins = Math.ceil(rangeMs / binMs) + 1; // +1: the clamp bin
  }

  observe(value: number, nowMs: number): void {
    this.evict(nowMs);
    const start = Math.floor(nowMs / this.bucketMs) * this.bucketMs;
    let bucket = this.buckets[this.buckets.length - 1];
    if (!bucket || bucket.start !== start) {
      bucket = { start, min: value, counts: new Array<number>(this.numBins).fill(0), total: 0 };
      this.buckets.push(bucket);
    }
    if (value < bucket.min) this.rebase(bucket, value);
    const idx = Math.min(Math.floor((value - bucket.min) / this.binMs), this.numBins - 1);
    bucket.counts[idx]++;
    bucket.total++;
  }

  // The q-th quantile's bin lower edge over the window; null when empty.
  quantile(q: number, nowMs: number): number | null {
    this.evict(nowMs);
    let total = 0;
    const bins: { value: number; count: number }[] = [];
    for (const b of this.buckets) {
      total += b.total;
      for (let i = 0; i < this.numBins; i++) {
        if (b.counts[i] > 0) bins.push({ value: b.min + i * this.binMs, count: b.counts[i] });
      }
    }
    if (total === 0) return null;
    bins.sort((a, b) => a.value - b.value);
    const targetRank = Math.max(1, Math.ceil(q * total));
    let cum = 0;
    for (const bin of bins) {
      cum += bin.count;
      if (cum >= targetRank) return bin.value;
    }
    return bins[bins.length - 1].value;
  }

  reset(): void {
    this.buckets = [];
  }

  // A value below the bucket's current min shifts bin 0 down (edges stay on
  // the original grid); counts pushed past the end merge into the clamp bin.
  private rebase(bucket: QuantileBucket, value: number): void {
    const shift = Math.ceil((bucket.min - value) / this.binMs);
    bucket.min -= shift * this.binMs;
    const counts = new Array<number>(this.numBins).fill(0);
    for (let i = 0; i < this.numBins; i++) {
      counts[Math.min(i + shift, this.numBins - 1)] += bucket.counts[i];
    }
    bucket.counts = counts;
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
