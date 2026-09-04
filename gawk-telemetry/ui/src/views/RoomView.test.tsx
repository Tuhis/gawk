// @vitest-environment jsdom
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { HistoryPage, HistoryRow } from '../api/types.ts';
import { groupByBroadcast } from '../lib/rooms.ts';
import { RoomView } from './RoomView.tsx';

// R42 (RM8): the room view is one server-filtered history query grouped by
// broadcast on the client. The grouping is the only thing the browser does,
// so it is what these tests pin — first as a pure function, then rendered.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const ROOM = '0a1b2c3d4e5f';

function row(over: Partial<HistoryRow> & Pick<HistoryRow, 'sessionId' | 'broadcastKey' | 'role'>): HistoryRow {
  return {
    severity: 'ok',
    stalls: 0,
    reconnects: 0,
    relayCoverage: 'none',
    findings: 0,
    roomKey: ROOM,
    startedAtMs: 1_000_000,
    durationMs: 60_000,
    ...over,
  };
}

const rows: HistoryRow[] = [
  row({ sessionId: 'a00000000000000000000001', broadcastKey: 'aaaaaaaaaaaa', role: 'viewer', startedAtMs: 3000 }),
  row({ sessionId: 'b00000000000000000000001', broadcastKey: 'bbbbbbbbbbbb', role: 'broadcaster', startedAtMs: 2500 }),
  row({ sessionId: 'a00000000000000000000002', broadcastKey: 'aaaaaaaaaaaa', role: 'broadcaster', startedAtMs: 2000 }),
  row({ sessionId: 'b00000000000000000000002', broadcastKey: 'bbbbbbbbbbbb', role: 'viewer', startedAtMs: 1000, live: true }),
];

describe('groupByBroadcast', () => {
  it('puts every session under its broadcast and keeps the server order within a group', () => {
    const groups = groupByBroadcast(rows);
    expect(groups.map((g) => g.broadcastKey).sort()).toEqual(['aaaaaaaaaaaa', 'bbbbbbbbbbbb']);
    const a = groups.find((g) => g.broadcastKey === 'aaaaaaaaaaaa')!;
    expect(a.rows.map((r) => r.sessionId)).toEqual([
      'a00000000000000000000001',
      'a00000000000000000000002',
    ]);
    expect(a.viewers).toBe(1);
    expect(a.broadcaster?.sessionId).toBe('a00000000000000000000002');
    expect(a.live).toBe(false);
  });

  it('orders live groups first, then by newest session', () => {
    const groups = groupByBroadcast(rows);
    // b has the still-running viewer; a has the newest session. Live wins.
    expect(groups.map((g) => g.broadcastKey)).toEqual(['bbbbbbbbbbbb', 'aaaaaaaaaaaa']);
    expect(groups[0].live).toBe(true);
  });

  it('is empty for no rows rather than throwing on Math.max of nothing', () => {
    expect(groupByBroadcast([])).toEqual([]);
  });
});

describe('RoomView', () => {
  it('asks the server for the room, then renders one section per broadcast with its sessions', async () => {
    const page: HistoryPage = { rows, total: rows.length, coverage: { rawFromMs: 0 } };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('v1/history/sessions')) {
        return new Response(JSON.stringify(page), { headers: { 'Content-Type': 'application/json' } });
      }
      return new Response('{}', { headers: { 'Content-Type': 'application/json' } });
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<RoomView roomKey={ROOM} />);

    // The filter is the server's, not the page's: the request names the room.
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain('v1/history/sessions');
    expect(url).toContain(`room=${ROOM}`);

    const a = await screen.findByLabelText('broadcast aaaaaaaaaaaa');
    const b = await screen.findByLabelText('broadcast bbbbbbbbbbbb');
    // Each group lists exactly its own sessions, as links to their pages.
    const aLinks = within(a).getAllByRole('link', { name: /a0000000/ });
    expect(aLinks.map((l) => l.getAttribute('href'))).toEqual([
      '#/session/a00000000000000000000001',
      '#/session/a00000000000000000000002',
    ]);
    expect(within(a).queryByRole('link', { name: /b0000000/ })).toBeNull();
    expect(within(b).getAllByRole('link', { name: /b0000000/ })).toHaveLength(2);
    // The group heading links to the broadcast's own page.
    expect(within(a).getByRole('link', { name: /broadcast aaaaaaaaaaaa/ }).getAttribute('href')).toBe(
      '#/broadcast/aaaaaaaaaaaa',
    );
    expect(screen.getByText(/2 broadcasts · 4 sessions/)).toBeTruthy();
  });

  it('says so for a malformed key instead of querying', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    render(<RoomView roomKey="not-a-key" />);
    expect(screen.getByText(/is not a room key/)).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
