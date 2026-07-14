// Diagnostics export (R9 M7, docs/13 D8): a bounded rolling window of stat
// samples that a stats overlay can serialize to JSON for the clipboard — the
// remote-troubleshooting story ("paste the blob into the chat") in place of
// any client→server metrics push. Also derives client-side rates (e.g.
// bitrate) from cumulative counters across the window.

export interface DiagnosticsSample<T> {
  // Milliseconds, monotonic within the buffer (performance.now by default).
  t: number;
  stats: T;
}

export const DIAGNOSTICS_CAPACITY = 20; // ~10 s at the 500 ms stats cadence

export class DiagnosticsBuffer<T> {
  private samples: DiagnosticsSample<T>[] = [];
  private capacity: number;
  private now: () => number;

  constructor(capacity = DIAGNOSTICS_CAPACITY, now: () => number = () => performance.now()) {
    this.capacity = capacity;
    this.now = now;
  }

  push(stats: T): void {
    this.samples.push({ t: this.now(), stats });
    if (this.samples.length > this.capacity) {
      this.samples.splice(0, this.samples.length - this.capacity);
    }
  }

  latest(): T | null {
    return this.samples.at(-1)?.stats ?? null;
  }

  // Per-second rate of a cumulative counter across the buffered window; null
  // when there are fewer than two samples, the selector yields no numbers, or
  // the counter went backwards (a pipeline restart/reconnect resets counters —
  // a negative "rate" would be nonsense).
  rate(selector: (stats: T) => number | null | undefined): number | null {
    if (this.samples.length < 2) return null;
    const first = this.samples[0];
    const last = this.samples[this.samples.length - 1];
    const a = selector(first.stats);
    const b = selector(last.stats);
    if (typeof a !== 'number' || typeof b !== 'number' || b < a) return null;
    const dt = (last.t - first.t) / 1000;
    if (dt <= 0) return null;
    return (b - a) / dt;
  }

  // The JSON blob for the clipboard: caller-provided context (surface,
  // settings, codec…), environment, and the sample history with timestamps
  // rebased to the first sample.
  build(context: Record<string, unknown>): string {
    const t0 = this.samples[0]?.t ?? 0;
    return JSON.stringify(
      {
        ...context,
        userAgent: typeof navigator !== 'undefined' ? navigator.userAgent : null,
        capturedAt: new Date().toISOString(),
        samples: this.samples.map((s) => ({ tMs: Math.round(s.t - t0), stats: s.stats })),
      },
      null,
      2,
    );
  }
}
