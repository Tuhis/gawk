// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { BroadcastTimelineView } from './BroadcastTimelineView.tsx';
import type { Broadcast, Timeline } from '../api/types.ts';
import { contentFlag } from '../testing/flags.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';
import type { ApiHandler } from '../testing/harness.tsx';
import { TL_KEY, timeline } from '../testing/timeline.ts';

afterEach(cleanup);

const LIVE: Broadcast = {
  id: 'ABC123',
  key: TL_KEY,
  publisherActive: true,
  publisherRemoteIp: '203.0.113.7',
  startedAt: '2026-10-15T19:00:00Z',
  viewersGlobal: 3,
  pods: [{ pod: 'gawk-server-0', role: 'origin', viewersLocal: 3 }],
  banState: { banned: false, ban: null },
};

function mount(tl: Timeline, extra?: ApiHandler) {
  const session = stubSession((path, init) => {
    if (path.startsWith('api/v1/screening/timeline?')) return json(tl);
    if (path === 'api/v1/broadcasts') return json({ broadcasts: tl.live ? [LIVE] : [] });
    if (extra) return extra(path, init);
    return json({});
  });
  renderWithSession(<BroadcastTimelineView broadcastKey={TL_KEY} killCooldownSeconds={600} refreshMs={0} />, session);
  return session;
}

const panel = () => screen.findByRole('complementary', { name: 'Decision' });

describe('the timeline (docs/72 D7, D9)', () => {
  it('asks for the broadcast by its key and draws the summary and the levels', async () => {
    const session = mount(timeline());
    expect(await screen.findByText('Recurring')).toBeTruthy();
    expect(screen.getByText('The signal keeps coming back')).toBeTruthy();
    expect(session.calls.some((c) => c.path === `api/v1/screening/timeline?key=${TL_KEY}`)).toBe(true);
    // The levels are the timeline's, never assumed (OD6).
    expect(screen.getByText(/threshold 0\.50 · trips at 3 of 6/)).toBeTruthy();
    expect(screen.getAllByText(/critical 0\.70 · floor 0\.30/).length).toBeGreaterThan(0);
    expect(screen.getByRole('img', { name: /Highest flagged-label score per 60 seconds/ })).toBeTruthy();
    expect(screen.getByText('Above threshold').parentElement?.textContent).toContain('27%');
  });

  it('says when the sampler reported no threshold instead of drawing one', async () => {
    mount(
      timeline({
        levels: { floor: 0.3, critical: 0.7, sustainedAfterSeconds: 180, rejoinSeconds: 600 },
        sessions: timeline().sessions.map((s) => ({ ...s, threshold: null, window: null, windowHits: null })),
        summary: { ...timeline().summary, pattern: 'unknown', thresholdKnown: false },
      }),
    );
    expect(await screen.findByText(/the sampler reported no threshold/)).toBeTruthy();
    expect(screen.getByText('threshold unknown', { exact: false })).toBeTruthy();
    expect(screen.getByText('Unknown')).toBeTruthy();
  });

  it('switches the range by asking for a from', async () => {
    const session = mount(timeline());
    fireEvent.click(await screen.findByRole('button', { name: 'Last hour' }));
    await waitFor(() => expect(session.calls.some((c) => /timeline\?key=.*&from=/.test(c.path))).toBe(true));
  });

  it('toggles a label strip off', async () => {
    mount(timeline());
    const box = (await screen.findByRole('checkbox', { name: 'ANUS' })) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    expect(box.checked).toBe(false);
  });
});

describe('the decision panel (docs/72 OD2, OD3)', () => {
  it('opens on Look for an open incident: Watch & acknowledge, nothing else', async () => {
    const session = mount(timeline());
    const p = await panel();
    const watch = within(p).getByRole('link', { name: 'Watch & acknowledge' }) as HTMLAnchorElement;
    expect(watch.href).toBe('https://gawk.example/#/view/ABC123');
    expect(watch.target).toBe('_blank');
    // One step's actions at a time: no dismiss, no violation yet.
    expect(within(p).queryByRole('button', { name: /Not a violation/ })).toBeNull();
    expect(within(p).queryByRole('button', { name: /Violation/ })).toBeNull();
    fireEvent.click(watch);
    await waitFor(() =>
      expect(session.calls.some((c) => c.path === 'api/v1/content-flags/14/acknowledge' && c.init.method === 'POST')).toBe(true),
    );
  });

  it('dismisses with the chosen reason and scope', async () => {
    const acked = timeline({ incidents: [contentFlag({ id: 14, state: 'acknowledged', broadcastKey: TL_KEY, raisedAt: '2026-10-15T19:30:00Z' })] });
    const session = mount(acked, (path) => (path === 'api/v1/content-flags/14/dismiss' ? json(contentFlag({ id: 14 })) : json({})));
    const p = await panel();
    fireEvent.click(within(p).getByRole('button', { name: /Not a violation/ }));
    fireEvent.click(within(p).getByLabelText('Detector error'));
    fireEvent.click(within(p).getByLabelText(/Rest of this session/));
    fireEvent.click(within(p).getByRole('button', { name: 'Dismiss flag #14' }));
    await waitFor(() => expect(session.calls.some((c) => c.path === 'api/v1/content-flags/14/dismiss')).toBe(true));
    const call = session.calls.find((c) => c.path === 'api/v1/content-flags/14/dismiss');
    expect(bodyOf(call!)).toEqual({ reason: 'detector_error', scope: 'session' });
  });

  it('suggests the violation and its ban length, and bans IP first then ID', async () => {
    const acked = timeline({ incidents: [contentFlag({ id: 14, state: 'acknowledged', broadcastKey: TL_KEY, raisedAt: '2026-10-15T19:30:00Z' })] });
    const session = mount(acked, (path, init) =>
      path === 'api/v1/bans' && init.method === 'POST' ? json({ id: 'b1', state: 'active' }, 201) : json({}),
    );
    const p = await panel();
    const violation = within(p).getByRole('button', { name: /Violation/ });
    expect(violation.textContent).toContain('suggested');
    fireEvent.click(violation);
    // The suggestion pre-selects 7 days; the person still confirms.
    expect((within(p).getByLabelText(/7 days/) as HTMLInputElement).checked).toBe(true);
    await waitFor(() => expect(within(p).getByLabelText(/Also ban the publisher IP/)).toBeTruthy());
    fireEvent.click(within(p).getByLabelText(/Also ban the publisher IP/));
    fireEvent.click(within(p).getByRole('button', { name: 'End & ban 7 days' }));
    await waitFor(() => expect(session.calls.filter((c) => c.path === 'api/v1/bans').length).toBe(2));
    const bans = session.calls.filter((c) => c.path === 'api/v1/bans').map((c) => bodyOf(c) as { target: { type: string } });
    // docs/42 §4.9's order: the IP ban resolves `publisher` before anything ends the session.
    expect(bans.map((b) => b.target.type)).toEqual(['ip', 'broadcastId']);
  });

  it('just ends the broadcast with the cooldown', async () => {
    const acked = timeline({
      incidents: [contentFlag({ id: 14, state: 'acknowledged', broadcastKey: TL_KEY, raisedAt: '2026-10-15T19:30:00Z' })],
      suggestion: {},
    });
    const session = mount(acked, (path) =>
      path === 'api/v1/broadcasts/ABC123/kill' ? json({ ban: { id: 'b1', state: 'active' } }, 201) : json({}),
    );
    const p = await panel();
    fireEvent.click(within(p).getByRole('button', { name: /Violation/ }));
    expect((within(p).getByLabelText(/Just end it/) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(p).getByRole('button', { name: 'End broadcast' }));
    await waitFor(() => expect(session.calls.some((c) => c.path === 'api/v1/broadcasts/ABC123/kill')).toBe(true));
    const call = session.calls.find((c) => c.path === 'api/v1/broadcasts/ABC123/kill');
    expect(bodyOf(call!)).toEqual({ reason: 'Explicit content — content flag #14', cooldownSeconds: 600 });
  });

  it('offers no action on an observed incident', async () => {
    mount(timeline({ incidents: [contentFlag({ id: 9, mode: 'observe', broadcastKey: TL_KEY })] }));
    const p = await panel();
    expect(within(p).getByText(/Observe mode: this is what would have paged/)).toBeTruthy();
    expect(within(p).queryAllByRole('button')).toHaveLength(0);
  });

  it('shows a resolved incident read-only, with its dismissal', async () => {
    mount(
      timeline({
        incidents: [
          contentFlag({
            id: 13,
            state: 'resolved',
            resolution: 'dismissed',
            resolutionReason: 'game_scene',
            mutedUntil: '2026-10-15T20:30:00Z',
            muteScope: 'episode',
            broadcastKey: TL_KEY,
          }),
        ],
      }),
    );
    const p = await panel();
    expect(within(p).getAllByText(/dismissed \(game scene\)/).length).toBeGreaterThan(0);
    expect(within(p).getByText(/\(episode\)/)).toBeTruthy();
    expect(within(p).queryAllByRole('button')).toHaveLength(0);
  });

  it('switches incident from the list', async () => {
    mount(
      timeline({
        incidents: [
          contentFlag({ id: 13, state: 'resolved', resolution: 'cleared', broadcastKey: TL_KEY, raisedAt: '2026-10-15T19:10:00Z' }),
          contentFlag({ id: 14, broadcastKey: TL_KEY, raisedAt: '2026-10-15T19:30:00Z' }),
        ],
      }),
    );
    // Opens on the unresolved one.
    expect(within(await panel()).getByRole('heading', { name: 'Incident #14' })).toBeTruthy();
    const list = screen.getByRole('heading', { name: 'Incidents' }).parentElement as HTMLElement;
    fireEvent.click(within(list).getByRole('button', { name: /#13/ }));
    expect(within(await panel()).getByRole('heading', { name: 'Incident #13' })).toBeTruthy();
  });
});
