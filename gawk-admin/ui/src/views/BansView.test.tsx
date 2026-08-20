// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { BansView } from './BansView.tsx';
import type { Ban } from '../api/types.ts';
import { json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

// Built per test, not at module scope. The rendered countdown is
// `expiresAt - loadedAt`, and `loadedAt` is the moment the stubbed fetch
// resolved — so a module-level fixture would be as stale as the file's import
// took, which on a cold transform cache is not a fixed quantity. Constructed
// inside the test, the gap is a couple of milliseconds.
function activeBan(): Ban {
  const now = Date.now();
  return {
    id: 'ban-1',
    target: { type: 'broadcastId', value: 'ABC123' },
    state: 'active',
    reason: 'terms violation',
    createdAt: new Date(now - 60_000).toISOString(),
    createdBy: 'juho@example.com',
    expiresAt: new Date(now + 3_600_000).toISOString(),
    sourceBroadcastId: 'ABC123',
  };
}

function removedBan(): Ban {
  return {
    ...activeBan(),
    id: 'ban-2',
    target: { type: 'ip', value: '203.0.113.7/32' },
    state: 'removed',
    expiresAt: null,
  };
}

describe('the ban list (§4.9)', () => {
  it('shows the target, actor, reason and the time left', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/bans') ? json({ bans: [activeBan()] }) : json({}),
    );
    renderWithSession(<BansView />, session);

    expect(await screen.findByText('ABC123')).toBeTruthy();
    expect(screen.getByText('juho@example.com')).toBeTruthy();
    // Ban reasons are operator-private context — they render here and in
    // Postgres, and relays log them at Debug only (D8, §5).
    expect(screen.getByText('terms violation')).toBeTruthy();
    expect(screen.getByText(/1h 00m|59m/)).toBeTruthy();
  });

  it('says "permanent" rather than showing an empty expiry', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/bans') ? json({ bans: [{ ...activeBan(), expiresAt: null }] }) : json({}),
    );
    renderWithSession(<BansView />, session);
    expect(await screen.findByText('permanent')).toBeTruthy();
  });

  it('asks the server for active bans by default and for all on request', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/bans') ? json({ bans: [activeBan()] }) : json({}),
    );
    renderWithSession(<BansView />, session);
    await screen.findByText('ABC123');
    expect(session.calls[0].path).toBe('api/v1/bans?state=active');

    fireEvent.click(screen.getByLabelText('All'));
    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/bans?state=all')).toBe(true);
    });
  });

  it('offers Unban only on an active ban', async () => {
    const session = stubSession((path) =>
      path.startsWith('api/v1/bans') ? json({ bans: [removedBan()] }) : json({}),
    );
    renderWithSession(<BansView />, session);
    await screen.findByText('203.0.113.7/32');
    expect(screen.queryByRole('button', { name: 'Unban' })).toBeNull();
  });
});

describe('unban round-trips (AP6)', () => {
  it('DELETEs the ban and re-reads the list rather than editing it locally', async () => {
    // The row is not removed optimistically on purpose: DELETE flips the row to
    // `removed` AND deletes the Ban CR, so a row that vanished on a click would
    // be a claim about the fleet the portal has not actually verified.
    let bans = [activeBan()];
    const session = stubSession((path, init) => {
      if (path === 'api/v1/bans/ban-1' && init.method === 'DELETE') {
        bans = [];
        return new Response(null, { status: 204 });
      }
      if (path.startsWith('api/v1/bans')) return json({ bans });
      return json({});
    });
    renderWithSession(<BansView />, session);

    fireEvent.click(await screen.findByRole('button', { name: 'Unban' }));

    await waitFor(() => expect(screen.queryByText('ABC123')).toBeNull());
    const paths = session.calls.map((c) => c.path);
    expect(paths).toContain('api/v1/bans/ban-1');
    // Read, delete, read again.
    expect(paths.filter((p) => p.startsWith('api/v1/bans?')).length).toBe(2);
    expect(await screen.findByText(/No active bans/)).toBeTruthy();
  });

  it('shows the server’s refusal and keeps the row', async () => {
    const session = stubSession((path, init) => {
      if (init.method === 'DELETE') {
        return json({ error: { code: 'not_found', message: 'no such ban' } }, 404);
      }
      if (path.startsWith('api/v1/bans')) return json({ bans: [activeBan()] });
      return json({});
    });
    renderWithSession(<BansView />, session);
    fireEvent.click(await screen.findByRole('button', { name: 'Unban' }));

    expect(await screen.findByText('no such ban')).toBeTruthy();
    expect(screen.getByText('ABC123')).toBeTruthy();
  });
});
