// TimeSync client + estimator (R5 Q2, docs/15). Pure logic under fake clocks:
// the NTP-style offset math, the min-RTT sample filter, and the ping loop's
// consume-replies-only contract.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  TIME_SYNC_SAMPLE_WINDOW,
  TimeSyncClient,
  TimeSyncEstimator,
  nowUs,
  timeOriginMs,
} from './time-sync';
import { encodeClockMapping, encodeTimeSync, parseTimeSync } from './wire';

describe('TimeSyncEstimator', () => {
  it('is null with no samples', () => {
    expect(new TimeSyncEstimator().best()).toBeNull();
  });

  it('computes offset and rtt from one exchange', () => {
    const e = new TimeSyncEstimator();
    // Sent at t0=1_000_000µs, server clock read 500_000µs, received t1=1_020_000µs.
    // Midpoint 1_010_000 → offset = 500_000 − 1_010_000 = −510_000. RTT 20ms.
    e.record(1_000_000n, 500_000n, 1_020_000n);
    const s = e.best();
    expect(s).not.toBeNull();
    expect(s!.offsetUs).toBe(-510_000n);
    expect(s!.rttMs).toBeCloseTo(20);
  });

  it('prefers the lowest-RTT sample (least asymmetry)', () => {
    const e = new TimeSyncEstimator();
    e.record(0n, 1_000_000n, 100_000n); // rtt 100ms, offset 950_000
    e.record(200_000n, 1_205_000n, 210_000n); // rtt 10ms, offset 1_000_000
    e.record(400_000n, 1_450_000n, 480_000n); // rtt 80ms, offset 1_010_000
    const s = e.best();
    expect(s!.rttMs).toBeCloseTo(10);
    expect(s!.offsetUs).toBe(1_000_000n);
  });

  it('slides the window: a stale best ages out after enough newer samples', () => {
    const e = new TimeSyncEstimator();
    e.record(0n, 0n, 1_000n); // rtt 1ms — the early best
    for (let i = 1; i <= TIME_SYNC_SAMPLE_WINDOW; i++) {
      const t0 = BigInt(i) * 1_000_000n;
      e.record(t0, t0 + 5_000n, t0 + 20_000n); // rtt 20ms, offset −5_000
    }
    expect(e.best()!.rttMs).toBeCloseTo(20);
    expect(e.best()!.offsetUs).toBe(-5_000n);
  });

  it('ignores a reply that predates its own request (bogus echo)', () => {
    const e = new TimeSyncEstimator();
    e.record(2_000_000n, 1n, 1_000_000n); // t1 < t0: impossible, dropped
    expect(e.best()).toBeNull();
  });
});

describe('TimeSyncClient', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('pings immediately and on the interval, and converges from echoed replies', () => {
    const sent: Uint8Array[] = [];
    let now = 10_000; // ms
    const client = new TimeSyncClient((d) => sent.push(d), () => now);
    client.start();
    expect(sent).toHaveLength(1); // first ping fires at start

    // Answer like the relay: echo clientTimeUs, fill serverTimeUs. 5ms later,
    // relay clock at 42s.
    const ping = parseTimeSync(sent[0]);
    expect(ping.serverTimeUs).toBe(0n);
    now += 5;
    const consumed = client.handleDatagram(
      encodeTimeSync({ clientTimeUs: ping.clientTimeUs, serverTimeUs: 42_000_000n }),
    );
    expect(consumed).toBe(true);
    const s = client.sample();
    expect(s).not.toBeNull();
    expect(s!.rttMs).toBeCloseTo(5);
    // offset = 42_000_000 − (t0 + rtt/2) = 42_000_000 − 10_002_500
    expect(s!.offsetUs).toBe(42_000_000n - 10_002_500n);
    // The sample names its clock domain: this context's timeOrigin. A
    // consumer in another worker rebases via this before applying offsetUs.
    expect(s!.timeOriginMs).toBe(timeOriginMs());

    vi.advanceTimersByTime(2_000);
    expect(sent).toHaveLength(2);

    client.stop();
    vi.advanceTimersByTime(10_000);
    expect(sent).toHaveLength(2); // stopped: no more pings
  });

  it('consumes only TimeSync datagrams and never throws on junk', () => {
    const client = new TimeSyncClient(() => {}, () => 0);
    expect(client.handleDatagram(encodeClockMapping(5n))).toBe(false);
    expect(client.handleDatagram(new Uint8Array([1, 5, 9]))).toBe(true); // truncated 0x05: consumed, dropped
    expect(client.handleDatagram(new Uint8Array(0))).toBe(false);
    expect(client.sample()).toBeNull();
  });

  it('a failing send does not break the ping loop', () => {
    const client = new TimeSyncClient(
      () => {
        throw new Error('session gone');
      },
      () => 0,
    );
    client.start();
    vi.advanceTimersByTime(6_000); // several pings, all throwing
    expect(client.sample()).toBeNull(); // alive, just no samples
    client.stop();
  });
});

describe('nowUs', () => {
  it('converts a millisecond clock reading to integer microseconds', () => {
    expect(nowUs(1234.5678)).toBe(1_234_568n);
  });
});
