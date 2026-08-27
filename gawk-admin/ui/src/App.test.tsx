// @vitest-environment jsdom
import { cleanup, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { App } from './App.tsx';
import { json, renderWithSession, stubSession } from './testing/harness.tsx';

afterEach(() => {
  cleanup();
  window.location.hash = '';
});

const OPERATOR = { email: 'juho@example.com', subject: 'sub-1', roles: ['operator'] };

describe('the authorization gate (§4.8, AP6)', () => {
  it('renders the missing-role page — not a login loop — on 403', async () => {
    // A 403 is a VALID token whose identity lacks the operator role. Sending
    // the operator back through the IdP would hand back the same token with the
    // same missing role, forever.
    const session = stubSession(() => json({}), { status: 'forbidden' });
    renderWithSession(<App />, session);

    expect(await screen.findByText('Not an operator')).toBeTruthy();
    expect(screen.getByText('operator')).toBeTruthy();
    // The one thing the page must NOT do.
    expect(session.loginCount()).toBe(0);
    expect(screen.queryByRole('link', { name: 'Broadcasts' })).toBeNull();
  });

  it('tells the reader that roles are granted at the identity provider', async () => {
    const session = stubSession(() => json({}), { status: 'forbidden' });
    renderWithSession(<App />, session);
    expect(await screen.findByText(/identity provider, not here/i)).toBeTruthy();
  });

  it('shows a sign-in placeholder while the flow is running', () => {
    const session = stubSession(() => json({}), { status: 'authenticating' });
    renderWithSession(<App />, session);
    expect(screen.getByText('Signing in…')).toBeTruthy();
  });

  it('names a failed flow instead of bouncing back to the IdP', () => {
    const session = stubSession(() => json({}), {
      status: 'error',
      message: 'authorization response did not match the request',
    });
    renderWithSession(<App />, session);
    expect(screen.getByText(/did not match the request/)).toBeTruthy();
    expect(session.loginCount()).toBe(0);
  });
});

describe('the shell', () => {
  /**
   * `lateMe` delays `/api/v1/me` by a macrotask.
   *
   * React runs child effects before parent ones, so `BroadcastsView`'s fetch is
   * always issued before `Portal`'s identity probe — which means the broadcast
   * rows can commit while `me` is still in flight. Before the fix, the Kill
   * button appeared in that window and a dialog opened from it seeded its
   * cooldown from the SPA's fallback instead of the server's configured value.
   * These tests run with the skew forced on, so the ordering cannot quietly
   * reverse and hide the bug again.
   */
  function mount(me: Record<string, unknown>, lateMe = false) {
    const session = stubSession(async (path) => {
      if (path === 'api/v1/me') {
        if (lateMe) await new Promise((resolve) => setTimeout(resolve, 0));
        return json(me);
      }
      if (path === 'api/v1/broadcasts') {
        return json({
          broadcasts: [
            {
              id: 'ABC123',
              key: '3f9a1c2b4d5e',
              publisherActive: true,
              publisherRemoteIp: '203.0.113.7',
              startedAt: new Date().toISOString(),
              viewersGlobal: 1,
              pods: [],
              banState: { banned: false, ban: null },
            },
          ],
        });
      }
      return json({});
    });
    renderWithSession(<App />, session);
    return session;
  }

  it('probes /api/v1/me and shows who is signed in', async () => {
    const session = mount(OPERATOR);
    expect(await screen.findByText('juho@example.com')).toBeTruthy();
    expect(session.calls.some((c) => c.path === 'api/v1/me')).toBe(true);
  });

  it('lands on broadcasts and offers every view', async () => {
    mount(OPERATOR);
    expect(await screen.findByRole('heading', { name: 'Broadcasts' })).toBeTruthy();
    for (const label of ['Broadcasts', 'Bans', 'Events', 'Relays', 'Webhooks']) {
      expect(screen.getByRole('link', { name: label })).toBeTruthy();
    }
  });

  it('feeds the server’s kill cooldown into the kill dialog', async () => {
    mount({ ...OPERATOR, defaults: { killCooldownSeconds: 900 } }, true);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    expect((screen.getByLabelText('Cooldown (seconds)') as HTMLInputElement).value).toBe('900');
  });

  it('falls back to the documented 600 s when the API does not carry a default', async () => {
    mount(OPERATOR, true);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    expect((screen.getByLabelText('Cooldown (seconds)') as HTMLInputElement).value).toBe('600');
  });

  // The rule behind the two tests above, stated directly: no actuator is on
  // screen until `/api/v1/me` has answered. Kill and Kill + ban end someone's
  // broadcast, and offering them for an identity whose `operator` role has not
  // come back yet is the wrong default even though a 403 would flip the whole
  // app a moment later.
  it('renders no kill controls until the authorization probe has answered', async () => {
    mount(OPERATOR, true);
    expect(screen.queryByRole('button', { name: 'Kill' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Kill + ban' })).toBeNull();
    expect(screen.getByText('Loading…')).toBeTruthy();
    // …and then it does.
    expect(await screen.findByRole('button', { name: 'Kill' })).toBeTruthy();
  });

  // A dead /api/v1/me must not black out a moderation console: the portal comes
  // up degraded, on the documented default, with the failure on screen.
  it('still renders the views when the probe fails for a reason other than 403', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/me') {
        return json({ error: { code: 'internal', message: 'postgres unreachable' } }, 500);
      }
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [] });
      return json({});
    });
    renderWithSession(<App />, session);

    expect(await screen.findByText('postgres unreachable')).toBeTruthy();
    expect(screen.getByRole('heading', { name: 'Broadcasts' })).toBeTruthy();
  });
});
