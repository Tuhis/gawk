// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { CalibrationView } from './CalibrationView.tsx';
import type { ContentFlag, ReplayRequest, ReplayResponse } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';
import type { ApiHandler } from '../testing/harness.tsx';
import { HEALTHY, contentFlag } from '../testing/flags.ts';

afterEach(cleanup);

function replayResult(over: Partial<ReplayResponse> = {}): ReplayResponse {
  return {
    schema: 'gawk.screening.replay.v1',
    request: {
      threshold: 0.8,
      window: 6,
      windowHits: 4,
      from: '2026-10-06T00:00:00Z',
      to: '2026-10-13T00:00:00Z',
    },
    floor: 0.3,
    sessions: 41,
    samplesTaken: 72_000,
    samplesStored: 3_100,
    broadcastHours: 120,
    wouldTrips: 2,
    wouldTripsPer100h: 2 / 1.2,
    trips: [
      {
        broadcastKey: 'aaaa11112222',
        broadcastId: 'AAA111',
        publisherEpoch: 'e1',
        at: '2026-10-10T20:00:00Z',
        peakScore: 0.91,
      },
    ],
    truncated: false,
    ...over,
  };
}

function mount({
  observed = [],
  extra,
}: {
  observed?: ContentFlag[];
  extra?: ApiHandler;
} = {}) {
  const session = stubSession((path, init) => {
    if (path.startsWith('api/v1/content-flags?')) {
      return json({ contentFlags: observed, screening: HEALTHY });
    }
    if (extra) return extra(path, init);
    return json({});
  });
  renderWithSession(<CalibrationView refreshMs={0} />, session);
  return session;
}

describe('the replay form (D21)', () => {
  it('posts the setting and shows what it would have done', async () => {
    const session = mount({
      extra: (path, init) =>
        path === 'api/v1/screening/replay' && init.method === 'POST' ? json(replayResult()) : json({}),
    });
    fireEvent.change(await screen.findByLabelText('Threshold (0–1)'), { target: { value: '0.85' } });
    fireEvent.change(screen.getByLabelText('Window (samples)'), { target: { value: '8' } });
    fireEvent.change(screen.getByLabelText('Hits to trip'), { target: { value: '5' } });
    fireEvent.change(screen.getByLabelText(/^Labels/), {
      target: { value: 'female_genitalia_exposed, ANUS_EXPOSED' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Replay' }));

    const result = await screen.findByLabelText('Replay result');
    const post = session.calls.find((c) => c.path === 'api/v1/screening/replay');
    const req = bodyOf(post!) as ReplayRequest;
    expect(req.threshold).toBe(0.85);
    expect(req.window).toBe(8);
    expect(req.windowHits).toBe(5);
    expect(req.labels).toEqual(['FEMALE_GENITALIA_EXPOSED', 'ANUS_EXPOSED']);
    // The default range is the last week, sent as RFC3339.
    expect(Date.parse(req.to) - Date.parse(req.from)).toBeGreaterThan(6.9 * 86_400_000);

    expect(within(result).getByText('1.67')).toBeTruthy();
    expect(within(result).getByText(/at or over the CF9 bar/)).toBeTruthy();
    expect(within(result).getByText('120.0')).toBeTruthy();
    expect(within(result).getByText('72000 / 3100')).toBeTruthy();
    expect(within(result).getByText('AAA111')).toBeTruthy();
    expect(within(result).getByText('0.91')).toBeTruthy();
  });

  it('marks a setting under the CF9 bar, and says so when nothing tripped', async () => {
    mount({
      extra: (path) =>
        path === 'api/v1/screening/replay'
          ? json(replayResult({ wouldTrips: 0, wouldTripsPer100h: 0, trips: [] }))
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Replay' }));
    const result = await screen.findByLabelText('Replay result');
    expect(within(result).getByText(/under the CF9 bar/)).toBeTruthy();
    expect(within(result).getByText('No broadcast would have tripped.')).toBeTruthy();
  });

  it('omits labels when the field is blank — every flagged label', async () => {
    const session = mount({ extra: () => json(replayResult()) });
    fireEvent.click(await screen.findByRole('button', { name: 'Replay' }));
    await screen.findByLabelText('Replay result');
    const req = bodyOf(session.calls.find((c) => c.path === 'api/v1/screening/replay')!) as ReplayRequest;
    expect(req.labels).toBeUndefined();
    expect(req).toMatchObject({ threshold: 0.8, window: 6, windowHits: 4 });
  });

  it('refuses hits over the window before the round trip', async () => {
    const session = mount();
    fireEvent.change(await screen.findByLabelText('Hits to trip'), { target: { value: '7' } });
    fireEvent.click(screen.getByRole('button', { name: 'Replay' }));
    expect(screen.getByRole('alert').textContent).toMatch(/hits must be a whole number from 1 to the window size/);
    expect(session.calls.some((c) => c.path === 'api/v1/screening/replay')).toBe(false);
  });

  // Only the server knows the stored floor.
  it('shows the server’s refusal of a threshold under the floor', async () => {
    mount({
      extra: (path) =>
        path === 'api/v1/screening/replay'
          ? json({ error: { code: 'bad_request', message: 'threshold 0.2 is below the sample floor 0.3' } }, 400)
          : json({}),
    });
    fireEvent.change(await screen.findByLabelText('Threshold (0–1)'), { target: { value: '0.2' } });
    fireEvent.click(screen.getByRole('button', { name: 'Replay' }));
    expect(await screen.findByText('threshold 0.2 is below the sample floor 0.3')).toBeTruthy();
  });

  it('notes a truncated trip list', async () => {
    mount({ extra: () => json(replayResult({ truncated: true })) });
    fireEvent.click(await screen.findByRole('button', { name: 'Replay' }));
    expect(await screen.findByText(/Only the newest 1 trips are listed/)).toBeTruthy();
  });
});

describe('observed flags (OD14)', () => {
  it('lists them, newest first as served, with Watch', async () => {
    const session = mount({
      observed: [
        contentFlag({
          id: 31,
          mode: 'observe',
          broadcastId: 'OBS111',
          links: { watch: 'https://gawk.example/#/view/OBS111' },
        }),
        contentFlag({
          id: 30,
          mode: 'observe',
          broadcastId: 'OBS222',
          state: 'resolved',
          resolution: 'cleared',
          resolutionReason: 'stale',
        }),
      ],
    });
    const watch = await screen.findByRole('link', { name: 'Watch' });
    expect(watch.getAttribute('href')).toBe('https://gawk.example/#/view/OBS111');
    expect(screen.getByText('OBS111')).toBeTruthy();
    expect(screen.getByText('tripped')).toBeTruthy();
    expect(screen.getByText('cleared (stale)')).toBeTruthy();
    // Observe mode only, every state: the calibration record, not the pager.
    const read = session.calls.find((c) => c.path.startsWith('api/v1/content-flags?'));
    const qs = new URLSearchParams(read!.path.split('?')[1]);
    expect(qs.get('mode')).toBe('observe');
    expect(qs.get('state')).toBe('all');
    // Observed flags are never acted on.
    expect(screen.queryByRole('button', { name: 'Acknowledge' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Dismiss…' })).toBeNull();
  });

  it('says when nothing has tripped yet', async () => {
    mount();
    expect(await screen.findByText(/No observed flags/)).toBeTruthy();
  });
});

describe('the view’s frame', () => {
  it('carries the screening banners', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/content-flags?')
        ? json({ contentFlags: [], screening: { ...HEALTHY, flagRoute: false, missingShards: [0] } })
        : json({}),
    );
    renderWithSession(<CalibrationView refreshMs={0} />, session);
    expect(await screen.findByText(/No pager for content flags/)).toBeTruthy();
    expect(screen.getByText('Sampler shard 0 of 2 is missing.')).toBeTruthy();
  });

  it('says screening is not enabled when the API answers 404', async () => {
    const session = stubSession(() =>
      json({ error: { code: 'not_found', message: 'no such endpoint' } }, 404),
    );
    renderWithSession(<CalibrationView refreshMs={0} />, session);
    expect(await screen.findByText(/Content screening is not enabled/)).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Replay' })).toBeNull());
  });
});
