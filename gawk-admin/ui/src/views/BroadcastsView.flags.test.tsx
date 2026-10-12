// @vitest-environment jsdom
//
// R40 content flags on the fleet view (docs/71 §9.4, CF4): the pinned row, its
// actions, and the two screening banners.
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { BroadcastsView } from './BroadcastsView.tsx';
import type { Broadcast, ContentFlag, ScreeningStatus } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';
import type { ApiHandler } from '../testing/harness.tsx';
import { HEALTHY, contentFlag } from '../testing/flags.ts';

afterEach(cleanup);

const NOW = Date.now();

function broadcast(over: Partial<Broadcast> = {}): Broadcast {
  return {
    id: 'ABC123',
    key: '3f9a1c2b4d5e',
    publisherActive: true,
    publisherRemoteIp: '203.0.113.7',
    startedAt: new Date(NOW - 90_000).toISOString(),
    viewersGlobal: 12,
    pods: [{ pod: 'gawk-server-0', role: 'origin', viewersLocal: 7 }],
    links: { watch: 'https://gawk.example/#/view/ABC123' },
    banState: { banned: false, ban: null },
    ...over,
  };
}

function mount({
  broadcasts,
  flags = [],
  status = HEALTHY,
  screening = true,
  extra,
  initialFilter,
}: {
  broadcasts: Broadcast[];
  flags?: ContentFlag[];
  status?: ScreeningStatus;
  screening?: boolean;
  extra?: ApiHandler;
  initialFilter?: string;
}) {
  const session = stubSession((path, init) => {
    if (path === 'api/v1/broadcasts') return json({ broadcasts });
    if (path.startsWith('api/v1/content-flags?')) return json({ contentFlags: flags, screening: status });
    if (extra) return extra(path, init);
    return json({});
  });
  renderWithSession(
    <BroadcastsView
      killCooldownSeconds={600}
      refreshMs={0}
      screening={screening}
      initialFilter={initialFilter}
    />,
    session,
  );
  return session;
}

/** The rows of the fleet table, in order, as their broadcast IDs. */
function rowIds(): string[] {
  return screen
    .getAllByRole('row')
    .map((r) => within(r).queryAllByText(/^[A-Z0-9]{6}$/)[0]?.textContent ?? '')
    .filter(Boolean);
}

describe('the pinned row', () => {
  it('pins a flagged broadcast to the top, with its strip', async () => {
    mount({
      broadcasts: [
        broadcast({ id: 'AAA111', key: 'aaaa11112222' }),
        broadcast({ id: 'ZZZ999', key: 'ffff99998888', contentFlag: contentFlag({ broadcastId: 'ZZZ999' }) }),
      ],
    });
    expect(await screen.findByText('Content flag #12')).toBeTruthy();
    // The server's order put the flagged one last; the pin puts it first.
    expect(rowIds()[0]).toBe('ZZZ999');
    expect(screen.getByText('flagged')).toBeTruthy();
    expect(screen.getByText('window 4 of 6')).toBeTruthy();
  });

  it('orders open flags before acknowledged ones, oldest first', async () => {
    const t = (ago: number) => new Date(NOW - ago).toISOString();
    mount({
      broadcasts: [
        broadcast({ id: 'ACK111', contentFlag: contentFlag({ id: 1, broadcastId: 'ACK111', state: 'acknowledged', raisedAt: t(900_000) }) }),
        broadcast({ id: 'NEW222', contentFlag: contentFlag({ id: 2, broadcastId: 'NEW222', raisedAt: t(60_000) }) }),
        broadcast({ id: 'OLD333', contentFlag: contentFlag({ id: 3, broadcastId: 'OLD333', raisedAt: t(600_000) }) }),
        broadcast({ id: 'CLN444' }),
      ],
    });
    await screen.findByText('Content flag #1');
    expect(rowIds()).toEqual(['OLD333', 'NEW222', 'ACK111', 'CLN444']);
  });

  // An observed flag pages no one and pins nothing (D19); a resolved one is over.
  it('never pins an observed or a resolved flag', async () => {
    mount({
      broadcasts: [
        broadcast({ id: 'OBS111', contentFlag: contentFlag({ broadcastId: 'OBS111', mode: 'observe' }) }),
        broadcast({ id: 'RES222', contentFlag: contentFlag({ broadcastId: 'RES222', state: 'resolved', resolution: 'cleared' }) }),
      ],
    });
    await screen.findByText('OBS111');
    expect(screen.queryByText(/Content flag #/)).toBeNull();
    expect(screen.queryByText('flagged')).toBeNull();
  });

  // A page must not be filterable out of sight while someone is typing.
  it('keeps a pinned row visible whatever the filter says', async () => {
    mount({
      broadcasts: [
        broadcast({ id: 'AAA111', key: 'aaaa11112222' }),
        broadcast({ id: 'ZZZ999', key: 'ffff99998888', contentFlag: contentFlag({ broadcastId: 'ZZZ999' }) }),
      ],
    });
    await screen.findByText('ZZZ999');
    fireEvent.change(screen.getByLabelText('Filter broadcasts'), { target: { value: 'aaa1' } });
    expect(screen.getByText('ZZZ999')).toBeTruthy();
    expect(screen.getByText('AAA111')).toBeTruthy();
  });

  // The flags list carries the flag even when the row does not (the two reads
  // are milliseconds apart): the union pins it.
  it('pins from the flags list when the row does not carry the flag yet', async () => {
    mount({ broadcasts: [broadcast()], flags: [contentFlag()] });
    expect(await screen.findByText('Content flag #12')).toBeTruthy();
    expect(screen.getByText('flagged')).toBeTruthy();
  });

  // A relay pod that did not answer must not be able to hide a page.
  it('shows a flag whose broadcast is missing from the fleet scan', async () => {
    const session = mount({
      broadcasts: [broadcast({ id: 'AAA111' })],
      flags: [contentFlag({ broadcastId: 'GHO555', links: { watch: 'https://gawk.example/#/view/GHO555' } })],
      extra: (path) => (path.endsWith('/acknowledge') ? json(contentFlag({ state: 'acknowledged' })) : json({})),
    });
    const orphan = (await screen.findByText(/not in this fleet scan/)).closest('tr') as HTMLElement;
    expect(within(orphan).getByText('GHO555')).toBeTruthy();
    // Kill needs the broadcast in hand; the flag's own actions still work.
    expect(within(orphan).queryByRole('button', { name: 'Kill…' })).toBeNull();
    expect(within(orphan).getByRole('link', { name: 'Watch' }).getAttribute('href')).toContain('GHO555');
    fireEvent.click(screen.getByRole('button', { name: 'Acknowledge' }));
    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/content-flags/12/acknowledge')).toBe(true);
    });
  });

  it('asks for no flags when screening is off', async () => {
    const session = mount({ broadcasts: [broadcast()], screening: false });
    await screen.findByText('ABC123');
    expect(session.calls.some((c) => c.path.startsWith('api/v1/content-flags'))).toBe(false);
  });
});

describe('the actions', () => {
  const flagged = () => broadcast({ contentFlag: contentFlag() });

  it('acknowledges in one tap and re-reads the fleet', async () => {
    const session = mount({
      broadcasts: [flagged()],
      extra: (path, init) =>
        path === 'api/v1/content-flags/12/acknowledge' && init.method === 'POST'
          ? json(contentFlag({ state: 'acknowledged' }))
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Acknowledge' }));
    expect(await screen.findByText(/Acknowledged flag #12 on ABC123/)).toBeTruthy();
    const broadcastsReads = session.calls.filter((c) => c.path === 'api/v1/broadcasts').length;
    expect(broadcastsReads).toBeGreaterThanOrEqual(2);
  });

  // Someone else (or the lifecycle) got there first: say so, do not retry.
  it('explains a 409 instead of failing loudly', async () => {
    mount({
      broadcasts: [flagged()],
      extra: (path) =>
        path.endsWith('/acknowledge')
          ? json({ error: { code: 'bad_request', message: 'flag is not open' } }, 409)
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Acknowledge' }));
    expect(await screen.findByText(/changed before your action landed \(flag is not open\)/)).toBeTruthy();
  });

  it('dismisses only after a confirm that says what the mute does', async () => {
    const muted = new Date(NOW + 30 * 60_000).toISOString();
    const session = mount({
      broadcasts: [flagged()],
      extra: (path, init) =>
        path === 'api/v1/content-flags/12/dismiss' && init.method === 'POST'
          ? json(contentFlag({ state: 'resolved', resolution: 'dismissed', mutedUntil: muted }))
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Dismiss…' }));
    const dialog = screen.getByRole('dialog', { name: 'Dismiss content flag #12' });
    expect(within(dialog).getByText(/mutes re-flagging of that broadcast/)).toBeTruthy();
    expect(session.calls.some((c) => c.path.endsWith('/dismiss'))).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Dismiss and mute' }));
    expect(await screen.findByText(/Dismissed flag #12; re-flagging ABC123 is muted until/)).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('keeps a failed dismissal in its dialog', async () => {
    mount({
      broadcasts: [flagged()],
      extra: (path) =>
        path.endsWith('/dismiss')
          ? json({ error: { code: 'unavailable', message: 'postgres unreachable' } }, 503)
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Dismiss…' }));
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss and mute' }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('postgres unreachable')).toBeTruthy();
  });

  // Kill goes through R39's dialog; the reason names the flag so the audit
  // trail links the two.
  it('kills through the kill dialog with the flag named in the reason', async () => {
    const session = mount({
      broadcasts: [flagged()],
      extra: (path, init) =>
        path === 'api/v1/broadcasts/ABC123/kill' && init.method === 'POST'
          ? json({ ban: { id: 'b1', target: { type: 'broadcastId', value: 'ABC123' }, state: 'active', reason: 'x', createdAt: new Date().toISOString(), createdBy: 'op', expiresAt: null } }, 201)
          : json({}),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Kill…' }));
    const reason = screen.getByLabelText('Reason (required)') as HTMLTextAreaElement;
    expect(reason.value).toBe('Content flag #12: explicit content');
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));
    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/broadcasts/ABC123/kill')).toBe(true);
    });
    const kill = session.calls.find((c) => c.path === 'api/v1/broadcasts/ABC123/kill');
    expect((bodyOf(kill!) as { reason: string }).reason).toBe('Content flag #12: explicit content');
  });

  it('keeps the row’s own Kill free of any flag reason', async () => {
    mount({ broadcasts: [flagged()] });
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    expect((screen.getByLabelText('Reason (required)') as HTMLTextAreaElement).value).toBe('');
  });

  it('links Watch to the live broadcast', async () => {
    mount({ broadcasts: [flagged()] });
    await screen.findByText('Content flag #12');
    const strip = screen.getByText('Content flag #12').closest('tr') as HTMLElement;
    expect(within(strip).getByRole('link', { name: 'Watch' }).getAttribute('href')).toBe(
      'https://gawk.example/#/view/ABC123',
    );
  });
});

describe('the screening banners', () => {
  it('raises the no-pager alarm when no webhook delivers content_flag.raised (D12)', async () => {
    mount({ broadcasts: [broadcast()], status: { ...HEALTHY, flagRoute: false } });
    const alarm = await screen.findByText(/No pager for content flags/);
    expect(alarm.closest('[role="alert"]')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Webhooks' })).toBeTruthy();
  });

  it('names the missing sampler shards and the recovery (D22)', async () => {
    mount({ broadcasts: [broadcast()], status: { ...HEALTHY, missingShards: [1] } });
    expect(await screen.findByText('Sampler shard 1 of 2 is missing.')).toBeTruthy();
    expect(screen.getByText(/--grace-period=0 --force/)).toBeTruthy();
  });

  it('pluralizes several missing shards', async () => {
    mount({ broadcasts: [broadcast()], status: { ...HEALTHY, expectedShards: 4, missingShards: [3, 1] } });
    expect(await screen.findByText('Sampler shards 1, 3 of 4 are missing.')).toBeTruthy();
  });

  it('stays quiet when the plane is healthy', async () => {
    mount({ broadcasts: [broadcast()] });
    await screen.findByText('ABC123');
    await waitFor(() => expect(screen.queryByText(/No pager/)).toBeNull());
    expect(screen.queryByText(/is missing/)).toBeNull();
  });

  // Screening switched off since the probe: not an outage.
  it('treats a 404 from the flags route as screening off', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path.startsWith('api/v1/content-flags')) {
        return json({ error: { code: 'not_found', message: 'no such endpoint' } }, 404);
      }
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} screening />, session);
    await screen.findByText('ABC123');
    await waitFor(() => {
      expect(session.calls.some((c) => c.path.startsWith('api/v1/content-flags'))).toBe(true);
    });
    expect(screen.queryByText(/could not be loaded/)).toBeNull();
  });

  // Any other failure is said out loud: a flag on a broadcast the scan missed
  // would otherwise be invisible.
  it('says when the flags list could not be loaded', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path.startsWith('api/v1/content-flags')) {
        return json({ error: { code: 'unavailable', message: 'postgres unreachable' } }, 503);
      }
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} screening />, session);
    expect(await screen.findByText(/Content flags could not be loaded \(postgres unreachable\)/)).toBeTruthy();
  });
});
