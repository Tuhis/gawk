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
      path.startsWith('api/v1/events') ? json({ events: [event(9), event(8)] }) : json({}),
    );
    renderWithSession(<EventsView />, session);
    expect(await screen.findAllByText('broadcast.killed')).toHaveLength(2);
    expect(screen.getAllByText('juho@example.com')).toHaveLength(2);
  });

  it('pages older events with the afterId cursor', async () => {
    const session = stubSession((path) => {
      if (path.includes('afterId=8')) return json({ events: [event(7)] });
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

  it('falls back to the oldest id on screen when the server sends no cursor', async () => {
    // §4.7 pins `afterId` as the cursor but not the field that carries the next
    // one; the feed must still page rather than stall.
    const session = stubSession((path) => {
      if (path.includes('afterId=8')) return json([event(7)]);
      if (path.startsWith('api/v1/events')) return json([event(9), event(8)]);
      return json({});
    });
    renderWithSession(<EventsView />, session);
    await screen.findAllByText('broadcast.killed');
    fireEvent.click(screen.getByRole('button', { name: 'Load older' }));
    await waitFor(() => {
      expect(session.calls.some((c) => c.path.includes('afterId=8'))).toBe(true);
    });
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
      path.startsWith('api/v1/events') ? json({ events: [event(9)] }) : json({}),
    );
    renderWithSession(<EventsView />, session);
    await screen.findByText('broadcast.killed');
    expect(screen.queryByText(/delivered|failed|pending/)).toBeNull();
  });
});
