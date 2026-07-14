// Client side of the TimeSync clock-sync protocol (R5 Q2, docs/15). Both the
// broadcaster and the viewer run one of these over their existing session:
// ping the relay every TIME_SYNC_INTERVAL_MS, and from each echoed reply take
// an NTP-style sample mapping the local performance.now() timeline onto the
// relay's monotonic clock:
//
//   rtt      = t1 − t0
//   offsetUs = serverTimeUs − (t0 + rtt/2)      (relayUs ≈ localUs + offsetUs)
//
// The lowest-RTT sample in a rolling window wins — queuing delay is what makes
// the out/back asymmetry (the one unmeasurable error) large, so the fastest
// exchange is the most symmetric one. Error is bounded by that sample's rtt/2.
//
// Pure and injectable: the send function and the clock come from the caller,
// so everything here runs in node tests.

import {
  TIME_SYNC_SIZE,
  TYPE_TIME_SYNC,
  encodeTimeSync,
  parseTimeSync,
} from './wire';

export const TIME_SYNC_INTERVAL_MS = 2000;
export const TIME_SYNC_SAMPLE_WINDOW = 8;
// How often the broadcaster re-publishes its ClockMapping (skew refresh).
export const CLOCK_MAPPING_INTERVAL_MS = 5000;

export interface TimeSyncStats {
  // relayClockUs ≈ localPerformanceUs + offsetUs (signed).
  offsetUs: bigint;
  // Round-trip of the winning sample — also a self-owned RTT for this leg,
  // independent of WebTransport.getStats() (which no browser ships today —
  // Chromium removed its pre-spec impl in 152; see docs/13 D7).
  rttMs: number;
}

interface Sample {
  offsetUs: bigint;
  rttUs: bigint;
}

export class TimeSyncEstimator {
  private samples: Sample[] = [];

  record(t0Us: bigint, serverTimeUs: bigint, t1Us: bigint): void {
    if (t1Us < t0Us) return; // impossible exchange (bogus/forged echo)
    const rttUs = t1Us - t0Us;
    const offsetUs = serverTimeUs - (t0Us + rttUs / 2n);
    this.samples.push({ offsetUs, rttUs });
    if (this.samples.length > TIME_SYNC_SAMPLE_WINDOW) this.samples.shift();
  }

  best(): TimeSyncStats | null {
    if (this.samples.length === 0) return null;
    let best = this.samples[0];
    for (const s of this.samples) {
      if (s.rttUs < best.rttUs) best = s;
    }
    return { offsetUs: best.offsetUs, rttMs: Number(best.rttUs) / 1000 };
  }
}

// A millisecond performance-clock reading as integer microseconds.
export function nowUs(nowMs: number = performance.now()): bigint {
  return BigInt(Math.round(nowMs * 1000));
}

// Owns the ping cadence and the estimator. The caller feeds every received
// datagram through handleDatagram(); TimeSync replies are consumed (returns
// true) so they never reach the video path.
export class TimeSyncClient {
  private send: (dgram: Uint8Array<ArrayBuffer>) => void;
  private now: () => number;
  private estimator = new TimeSyncEstimator();
  private timer: number | null = null;

  constructor(send: (dgram: Uint8Array<ArrayBuffer>) => void, now: () => number = () => performance.now()) {
    this.send = send;
    this.now = now;
  }

  start(intervalMs = TIME_SYNC_INTERVAL_MS): void {
    this.ping();
    // Bare setInterval so this runs unchanged inside workers.
    this.timer = setInterval(() => this.ping(), intervalMs) as unknown as number;
  }

  stop(): void {
    if (this.timer !== null) {
      clearInterval(this.timer);
      this.timer = null;
    }
  }

  // Returns true when the datagram was a TimeSync message (consumed here —
  // well-formed or not); false means "not mine, route it on".
  handleDatagram(dgram: Uint8Array): boolean {
    if (dgram.length < 2 || dgram[1] !== TYPE_TIME_SYNC) return false;
    if (dgram.length === TIME_SYNC_SIZE) {
      try {
        const msg = parseTimeSync(dgram);
        this.estimator.record(msg.clientTimeUs, msg.serverTimeUs, nowUs(this.now()));
      } catch {
        // malformed: dropped (strict parsing, R2 discipline)
      }
    }
    return true;
  }

  sample(): TimeSyncStats | null {
    return this.estimator.best();
  }

  private ping(): void {
    try {
      this.send(encodeTimeSync({ clientTimeUs: nowUs(this.now()), serverTimeUs: 0n }));
    } catch {
      // Session gone or writer failed — the session's own lifecycle handles
      // it; a ping must never take the pipeline down.
    }
  }
}
