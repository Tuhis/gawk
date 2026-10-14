// Test-only content-flag fixtures (R40), in the §9.4 shape. Nothing in app code
// imports this.

import type { ContentFlag, ScreeningStatus } from '../api/types.ts';

export const HEALTHY: ScreeningStatus = {
  flagRoute: true,
  expectedShards: 2,
  missingShards: [],
  lastPollAt: new Date().toISOString(),
};

export function contentFlag(over: Partial<ContentFlag> = {}): ContentFlag {
  const raised = Date.now() - 190_000;
  return {
    id: 12,
    mode: 'flag',
    state: 'open',
    resolution: null,
    resolutionReason: null,
    broadcastId: 'ABC123',
    broadcastKey: '3f9a1c2b4d5e',
    publisherEpoch: '5f2c9a0b11d4e6f7',
    samplerId: 'gawk-sampler-1',
    raisedAt: new Date(raised).toISOString(),
    updatedAt: new Date(raised + 60_000).toISOString(),
    lastPostAt: new Date(raised + 60_000).toISOString(),
    acknowledgedAt: null,
    acknowledgedBy: null,
    resolvedAt: null,
    resolvedBy: null,
    signal: 'live',
    window: {
      start: new Date(raised - 30_000).toISOString(),
      end: new Date(raised).toISOString(),
      samples: 6,
      flaggedSamples: 4,
    },
    scores: { max: 0.97, mean: 0.81 },
    labels: ['FEMALE_GENITALIA_EXPOSED'],
    labelCounts: { FEMALE_GENITALIA_EXPOSED: 4 },
    peakScore: 0.99,
    reminders: 0,
    mutedUntil: null,
    ...over,
  };
}
