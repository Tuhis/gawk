// Test-only moderation-timeline fixtures (R70), in the docs/72 D7 shape.
// Nothing in app code imports this.

import type { Timeline, TimelineBucket } from '../api/types.ts';
import { contentFlag } from './flags.ts';

const T0 = Date.parse('2026-10-15T19:00:00Z');

export const TL_KEY = '3f9a1c2b4d5e';

function bucket(minute: number, max: number, above: number, critical: number): TimelineBucket {
  return {
    start: new Date(T0 + minute * 60_000).toISOString(),
    max,
    scores: { FEMALE_GENITALIA_EXPOSED: 0, MALE_GENITALIA_EXPOSED: max, ANUS_EXPOSED: Math.max(0, max - 0.2) },
    stored: 12,
    above,
    critical,
  };
}

export function timeline(over: Partial<Timeline> = {}): Timeline {
  return {
    schema: 'gawk.screening.timeline.v1',
    broadcastKey: TL_KEY,
    broadcastId: 'ABC123',
    live: true,
    from: new Date(T0).toISOString(),
    to: new Date(T0 + 60 * 60_000).toISOString(),
    bucketSeconds: 60,
    truncated: false,
    levels: {
      floor: 0.3,
      threshold: 0.5,
      window: 6,
      windowHits: 3,
      critical: 0.7,
      sustainedAfterSeconds: 180,
      rejoinSeconds: 600,
    },
    labels: ['FEMALE_GENITALIA_EXPOSED', 'MALE_GENITALIA_EXPOSED', 'ANUS_EXPOSED'],
    sessions: [
      {
        publisherEpoch: '5f2c9a0b11d4e6f7',
        broadcastId: 'ABC123',
        firstSampleAt: new Date(T0).toISOString(),
        lastSampleAt: new Date(T0 + 59 * 60_000).toISOString(),
        samplesTaken: 720,
        samplesStored: 120,
        floor: 0.3,
        threshold: 0.5,
        window: 6,
        windowHits: 3,
        mode: 'flag',
        samplerId: 'gawk-sampler-0',
      },
    ],
    buckets: [bucket(10, 0.42, 0, 0), bucket(30, 0.62, 8, 0), bucket(31, 0.84, 12, 9), bucket(32, 0.78, 12, 4)],
    incidents: [
      contentFlag({
        id: 14,
        broadcastId: 'ABC123',
        broadcastKey: TL_KEY,
        raisedAt: new Date(T0 + 30 * 60_000).toISOString(),
        links: { watch: 'https://gawk.example/#/view/ABC123' },
        trips: 2,
      }),
    ],
    events: [
      {
        id: 812,
        type: 'content_flag.raised',
        occurredAt: new Date(T0 + 30 * 60_000).toISOString(),
        actor: 'service-account-gawk-sampler',
        summary: 'content flag raised on broadcast ABC123: 4 of 6 samples flagged',
      },
    ],
    summary: {
      pattern: 'recurring',
      thresholdKnown: true,
      sampledSeconds: 3600,
      aboveSeconds: 960,
      criticalSeconds: 600,
      longestRunSeconds: 360,
      runs: 3,
      sustainedRuns: 2,
      peak: 0.84,
      peakLabel: 'MALE_GENITALIA_EXPOSED',
    },
    suggestion: { verdict: 'violation', banSeconds: 604_800, why: 'the signal came back' },
    links: { watch: 'https://gawk.example/#/view/ABC123' },
    ...over,
  };
}
