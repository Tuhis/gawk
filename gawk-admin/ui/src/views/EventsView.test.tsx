// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { EventsView } from './EventsView.tsx';
import type { ModerationEvent } from '../api/types.ts';
import { json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

function event(id: number, over: Partial<ModerationEvent> = {}): ModerationEvent {
  return {
    id,
    type: 'broadcast.killed',
    occurredAt: new Date(Date.now() - id * 1000).toISOString(),
    actor: 'juho@example.com',
    broadcastKey: '3f9a1c2b4d5e',
    broadcastId: 'ABC123',
    reason: 'terms violation',
    ...over,
  };
}

describe('the audit feed (§4.9)', () => {
  it('renders the newest events with actor and type', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/events')
        ? json({ events: [event(9), event(8)], nextAfterId: null })
        : json({}),
    );
    renderWithSession(<EventsView />, session);
    expect(await screen.findAllByText('broadcast.killed')).toHaveLength(2);
    expect(screen.getAllByText('juho@example.com')).toHaveLength(2);
  });

  it('pages older events with the afterId cursor', async () => {
    const session = stubSession((path) => {
      if (path.includes('afterId=8')) return json({ events: [event(7)], nextAfterId: null });
      if (path.startsWith('api/v1/events')) {
        return json({ events: [event(9), event(8)], nextAfterId: 8 });
      }
      return json({});
    });
    renderWithSession(<EventsView />, session);
    await screen.findAllByText('broadcast.killed');

    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    await waitFor(() => expect(screen.getAllByText('broadcast.killed')).toHaveLength(3));
    expect(session.calls.some((c) => c.path.includes('afterId=8'))).toBe(true);
  });

  // Regression (PR #280 review). "Load older" is disabled while in flight;
  // Refresh is not. A Refresh that overtakes a pending page used to splice
  // that page — fetched against a cursor the refresh has since thrown away —
  // onto the end of the fresh first page. Every event between the fresh page's
  // last row and the old cursor then exists in neither, and the stored cursor
  // no longer describes what is on screen. In an audit feed, silently missing
  // rows are the one failure that must not happen.
  it('ignores a "Load older" page that a Refresh has already overtaken', async () => {
    let releaseOlder!: (res: Response) => void;
    const older = new Promise<Response>((resolve) => {
      releaseOlder = resolve;
    });
    let refreshed = false;
    const session = stubSession((path) => {
      if (path.includes('afterId=8')) return older;
      if (path.includes('afterId=11')) {
        return json({ events: [event(6, { summary: 'older-6' })], nextAfterId: null });
      }
      if (path.startsWith('api/v1/events')) {
        return refreshed
          ? json({
              events: [event(12, { summary: 'fresh-12' }), event(11, { summary: 'fresh-11' })],
              nextAfterId: 11,
            })
          : json({
              events: [event(9, { summary: 'first-9' }), event(8, { summary: 'first-8' })],
              nextAfterId: 8,
            });
      }
      return json({});
    });
    renderWithSession(<EventsView />, session);
    await screen.findByText('first-9');

    // Older page in flight against cursor 8 …
    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    // … and two newer events arrive and are refreshed in underneath it.
    refreshed = true;
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await screen.findByText('fresh-12');

    releaseOlder(json({ events: [event(7, { summary: 'stale-7' })], nextAfterId: null }));
    await waitFor(() => {
      const button = screen.getByRole('button', { name: 'Load older' }) as HTMLButtonElement;
      expect(button.disabled).toBe(false);
    });

    // Events 10, 9 and 8 are NOT on screen, so appending 7 would leave a hole
    // rather than a page.
    expect(screen.queryByText('stale-7')).toBeNull();
    expect(screen.getByText('fresh-11')).toBeTruthy();

    // And the cursor still belongs to what is rendered: the next page is the
    // one after the last visible row, not after a row nobody ever saw.
    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    expect(await screen.findByText('older-6')).toBeTruthy();
  });

  it('stops offering "Load older" once nextAfterId comes back null', async () => {
    // The key is always present; non-null only when the page came back full. So
    // null means exhausted, and the view must not page into an empty response.
    const session = stubSession((path) =>
      path.startsWith('api/v1/events')
        ? json({ events: [event(9), event(8)], nextAfterId: null })
        : json({}),
    );
    renderWithSession(<EventsView />, session);
    await screen.findAllByText('broadcast.killed');
    expect(screen.queryByRole('button', { name: 'Load older' })).toBeNull();
  });
});

describe('webhook delivery state per event (§4.10, AP6)', () => {
  it('shows a failed delivery, with the error, on the event itself', async () => {
    // "A failed delivery must be *seen*": R40's posture — a flag must reach a
    // human — inherits this pipe, so a silent failure here is the failure that
    // matters most.
    const session = stubSession((path) =>
      path.startsWith('api/v1/events')
        ? json({
            nextAfterId: null,
            events: [
              event(9, {
                deliveries: [
                  { webhookName: 'ntfy-oncall', state: 'failed', attempts: 5, lastError: '502' },
                  { webhookName: 'discord', state: 'delivered', attempts: 1 },
                ],
              }),
            ],
          })
        : json({}),
    );
    renderWithSession(<EventsView />, session);
    expect(await screen.findByText(/ntfy-oncall: failed/)).toBeTruthy();
    expect(screen.getByText(/502/)).toBeTruthy();
    expect(screen.getByText(/discord: delivered/)).toBeTruthy();
  });

  it('shows a pending delivery too, so a retry in flight is not mistaken for success', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/events')
        ? json({
            nextAfterId: null,
            events: [
              event(9, {
                deliveries: [{ webhookName: 'ntfy-oncall', state: 'pending', attempts: 2 }],
              }),
            ],
          })
        : json({}),
    );
    renderWithSession(<EventsView />, session);
    expect(await screen.findByText(/ntfy-oncall: pending \(2 attempts\)/)).toBeTruthy();
  });

  it('says nothing about deliveries when no webhook is configured', async () => {
    // Zero webhooks is a supported state (§4.10) — events still land here.
    const session = stubSession((path) =>
      path.startsWith('api/v1/events')
        ? json({ events: [event(9)], nextAfterId: null })
        : json({}),
    );
    renderWithSession(<EventsView />, session);
    await screen.findByText('broadcast.killed');
    expect(screen.queryByText(/delivered|failed|pending/)).toBeNull();
  });
});
