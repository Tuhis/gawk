import { describe, expect, it } from 'vitest';
import { ConnectionStatsSampler, sampleConnectionStats } from './net-stats';

// R9 M6 (docs/13 D7): WebTransport.getStats() is unevenly shipped, so the
// sampler must handle present, absent, partial, and rejecting getStats
// without ever throwing.

const fullDict = {
  smoothedRtt: 23.5,
  rttVariation: 4.2,
  packetsSent: 1000,
  packetsReceived: 900,
  packetsLost: 7,
  bytesSent: 1_000_000,
  bytesReceived: 900_000,
  estimatedSendRate: 5_000_000,
  atSendCapacity: false,
  datagrams: { expiredOutgoing: 3, lostOutgoing: 5, droppedIncoming: 2 },
};

describe('sampleConnectionStats', () => {
  it('maps a full stats dictionary', async () => {
    const wt = { getStats: async () => fullDict };
    const s = await sampleConnectionStats(wt);
    expect(s).toEqual({
      rttMs: 23.5,
      rttVarMs: 4.2,
      packetsSent: 1000,
      packetsReceived: 900,
      packetsLost: 7,
      bytesSent: 1_000_000,
      bytesReceived: 900_000,
      estimatedSendRateBps: 5_000_000,
      atSendCapacity: false,
      datagramsExpiredOutgoing: 3,
      datagramsLostOutgoing: 5,
      datagramsDroppedIncoming: 2,
    });
  });

  it('returns null when getStats is missing (Firefox shape)', async () => {
    expect(await sampleConnectionStats({})).toBeNull();
    expect(await sampleConnectionStats(null)).toBeNull();
    expect(await sampleConnectionStats(undefined)).toBeNull();
  });

  it('returns null when getStats rejects, without throwing', async () => {
    const wt = {
      getStats: async () => {
        throw new Error('not connected');
      },
    };
    expect(await sampleConnectionStats(wt)).toBeNull();
  });

  it('maps missing/odd fields to null instead of NaN or garbage', async () => {
    const wt = {
      getStats: async () => ({
        smoothedRtt: 12,
        estimatedSendRate: null, // spec: nullable
        atSendCapacity: 'yes', // wrong type → null
        packetsLost: Number.NaN, // non-finite → null
        // no datagrams member at all
      }),
    };
    const s = await sampleConnectionStats(wt);
    expect(s).not.toBeNull();
    expect(s?.rttMs).toBe(12);
    expect(s?.estimatedSendRateBps).toBeNull();
    expect(s?.atSendCapacity).toBeNull();
    expect(s?.packetsLost).toBeNull();
    expect(s?.datagramsLostOutgoing).toBeNull();
  });
});

describe('ConnectionStatsSampler', () => {
  it('exposes the latest completed sample after a tick', async () => {
    const wt = { getStats: async () => fullDict };
    const sampler = new ConnectionStatsSampler(wt);
    expect(sampler.latest()).toBeNull(); // nothing sampled yet
    sampler.tick();
    await Promise.resolve(); // let the async refresh settle
    await Promise.resolve();
    expect(sampler.latest()?.rttMs).toBe(23.5);
  });

  it('stays null forever on an unsupported transport', async () => {
    const sampler = new ConnectionStatsSampler({});
    sampler.tick();
    await Promise.resolve();
    await Promise.resolve();
    expect(sampler.latest()).toBeNull();
  });
});
