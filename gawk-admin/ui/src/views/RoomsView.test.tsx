// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { RoomsView } from './RoomsView.tsx';
import type { Room } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

const STATIC: Room = {
  name: 'tuhisroom',
  kind: 'static',
  code: 'TuhisRoom',
  displayName: "Tuhis' room",
  maxBroadcasts: 4,
  attachments: 1,
  homeHolder: 'gawk-server-0',
  key: '9c1d2e3f4a5b',
  createdAt: '2026-09-03T18:00:00Z',
  hasAttachSecret: true,
  managed: true,
};

const OPEN_KUBECTL: Room = {
  name: 'ourroom',
  kind: 'static',
  code: 'ourroom',
  attachments: 0,
  hasAttachSecret: false,
  managed: false,
};

const DYNAMIC: Room = {
  name: 'r7k3mx',
  kind: 'dynamic',
  code: 'R7K3MX',
  attachments: 2,
  homeHolder: 'gawk-server-1',
  key: '1a2b3c4d5e6f',
  createdAt: '2026-09-03T18:05:00Z',
  emptySince: '2026-09-03T18:30:00Z',
  hasAttachSecret: false,
  managed: false,
};

function mount(
  rooms: Room[],
  extra?: (path: string, init: RequestInit) => Response,
  initialFilter = '',
) {
  const session = stubSession((path, init) => {
    if (path === 'api/v1/rooms' && (init.method ?? 'GET') === 'GET') return json({ rooms });
    if (extra) return extra(path, init);
    return new Response(null, { status: 204 });
  });
  renderWithSession(<RoomsView initialFilter={initialFilter} />, session);
  return session;
}

function rowFor(code: string): HTMLElement {
  const cell = screen.getByText(code);
  const row = cell.closest('tr');
  if (!row) throw new Error(`no row for ${code}`);
  return row;
}

describe('the room list (docs/44 D20)', () => {
  it('renders both kinds with their gate, home and provenance', async () => {
    mount([STATIC, OPEN_KUBECTL, DYNAMIC]);
    expect(await screen.findByText('TuhisRoom')).toBeTruthy();
    const st = within(rowFor('TuhisRoom'));
    expect(st.getByText('static')).toBeTruthy();
    expect(st.getByText('attach secret')).toBeTruthy();
    expect(st.getByText('gawk-server-0')).toBeTruthy();
    expect(st.getByText('9c1d2e3f4a5b')).toBeTruthy();
    expect(st.getByRole('button', { name: 'Rotate secret' })).toBeTruthy();
    expect(st.getByRole('button', { name: 'Delete' })).toBeTruthy();

    // A kubectl-applied open room: marked, and offered "Add secret".
    const kc = within(rowFor('ourroom'));
    expect(kc.getByText('kubectl')).toBeTruthy();
    expect(kc.getByText('open')).toBeTruthy();
    expect(kc.getByText('not homed')).toBeTruthy();
    expect(kc.getByRole('button', { name: 'Add secret' })).toBeTruthy();

    // A dynamic room: no secret to rotate, "End room" instead of delete.
    const dy = within(rowFor('R7K3MX'));
    expect(dy.getByText('dynamic')).toBeTruthy();
    expect(dy.getByText('creator token')).toBeTruthy();
    expect(dy.getByText('(empty)')).toBeTruthy();
    expect(dy.queryByRole('button', { name: /secret/i })).toBeNull();
    expect(dy.getByRole('button', { name: 'End room' })).toBeTruthy();
  });

  it('pre-fills the filter from the route key, as a webhook deep link does', async () => {
    // `portalUrl` on a room event is `#/rooms?key=<roomKey>` — the HMAC'd
    // key, never the code — so a paged operator lands on that one row.
    mount([STATIC, DYNAMIC], undefined, '1a2b3c4d5e6f');
    expect(await screen.findByText('R7K3MX')).toBeTruthy();
    expect(screen.queryByText('TuhisRoom')).toBeNull();
    expect((screen.getByLabelText('Filter rooms') as HTMLInputElement).value).toBe('1a2b3c4d5e6f');
  });

  it('filters by code, display name or key as typed', async () => {
    mount([STATIC, DYNAMIC]);
    await screen.findByText('TuhisRoom');
    fireEvent.change(screen.getByLabelText('Filter rooms'), { target: { value: 'tuhis' } });
    expect(screen.getByText('TuhisRoom')).toBeTruthy();
    expect(screen.queryByText('R7K3MX')).toBeNull();
    fireEvent.change(screen.getByLabelText('Filter rooms'), { target: { value: 'nothing-here' } });
    expect(screen.getByText('No room matches the filter.')).toBeTruthy();
  });

  it('says rooms are not enabled when the API answers 404', async () => {
    // Without `-rooms` the route does not exist. That must read as "not
    // enabled", not as an empty list and not as a broken link.
    const session = stubSession(() =>
      json({ error: { code: 'not_found', message: 'no such endpoint' } }, 404),
    );
    renderWithSession(<RoomsView />, session);
    expect(await screen.findByText(/not enabled on this deployment/)).toBeTruthy();
    expect(
      (screen.getByRole('button', { name: 'Create static room' }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it('shows the empty state', async () => {
    mount([]);
    expect(await screen.findByText(/No rooms\./)).toBeTruthy();
  });
});

describe('creating a static room', () => {
  it('sends the form and reveals the one-time secret exactly once', async () => {
    const session = mount([], (path, init) =>
      path === 'api/v1/rooms' && init.method === 'POST'
        ? json({ room: { ...STATIC, attachments: 0 }, attachSecret: 'AttachSecretValue12345678' }, 201)
        : json({}),
    );
    await waitFor(() => expect(session.calls.length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole('button', { name: 'Create static room' }));
    fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: 'TuhisRoom' } });
    fireEvent.change(screen.getByLabelText(/Display name/), { target: { value: "Tuhis' room" } });
    fireEvent.change(screen.getByLabelText(/Max broadcasts/), { target: { value: '4' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/rooms' && c.init.method === 'POST')).toBe(
        true,
      );
    });
    const post = session.calls.find((c) => c.path === 'api/v1/rooms' && c.init.method === 'POST');
    expect(bodyOf(post!)).toEqual({
      code: 'TuhisRoom',
      displayName: "Tuhis' room",
      maxBroadcasts: 4,
      withAttachSecret: true,
    });

    // The reveal: the secret, selectable, with the warning that it is shown once.
    const secret = (await screen.findByLabelText('Attach secret')) as HTMLInputElement;
    expect(secret.value).toBe('AttachSecretValue12345678');
    expect(secret.readOnly).toBe(true);
    expect(screen.getByText(/Shown once/)).toBeTruthy();

    // Dismissed, it is gone for good: nothing on the page carries it any more.
    fireEvent.click(screen.getByRole('button', { name: 'I have copied it' }));
    expect(screen.queryByText('AttachSecretValue12345678')).toBeNull();
    expect(screen.queryByLabelText('Attach secret')).toBeNull();
  });

  it('creates an open room with no secret and no reveal', async () => {
    const session = mount([], (path, init) =>
      path === 'api/v1/rooms' && init.method === 'POST'
        ? json({ room: OPEN_KUBECTL }, 201)
        : json({}),
    );
    await waitFor(() => expect(session.calls.length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole('button', { name: 'Create static room' }));
    fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: 'ourroom' } });
    fireEvent.click(screen.getByLabelText(/Require an attach secret/));
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => {
      expect(session.calls.some((c) => c.init.method === 'POST')).toBe(true);
    });
    const post = session.calls.find((c) => c.init.method === 'POST');
    expect(bodyOf(post!)).toEqual({ code: 'ourroom', withAttachSecret: false });
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Create static room' })).toBeNull());
    expect(screen.queryByLabelText('Attach secret')).toBeNull();
  });

  it('refuses a six-character broadcast-alphabet code before the round trip (docs/44 D2)', async () => {
    const session = mount([]);
    await waitFor(() => expect(session.calls.length).toBeGreaterThan(0));
    fireEvent.click(screen.getByRole('button', { name: 'Create static room' }));
    fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: 'ABC234' } });
    expect(screen.getByText(/look like a dynamic room code/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement).disabled).toBe(true);
    // Six characters outside the alphabet (a 0 and a 1) are fine.
    fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: 'room01' } });
    expect(screen.queryByText(/look like a dynamic room code/)).toBeNull();
    expect((screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement).disabled).toBe(false);
    expect(session.calls.some((c) => c.init.method === 'POST')).toBe(false);
  });

  it('shows the server’s duplicate refusal in the form', async () => {
    const session = mount([STATIC], (path, init) =>
      path === 'api/v1/rooms' && init.method === 'POST'
        ? json({ error: { code: 'room_exists', message: 'a room with that code already exists' } }, 409)
        : json({}),
    );
    await screen.findByText('TuhisRoom');
    fireEvent.click(screen.getByRole('button', { name: 'Create static room' }));
    fireEvent.change(screen.getByLabelText(/^Code/), { target: { value: 'tuhisroom' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    expect(await screen.findByText(/already exists/)).toBeTruthy();
    expect(session.calls.filter((c) => c.init.method === 'POST').length).toBe(1);
    // The form stays open for a corrected code.
    expect(screen.getByRole('heading', { name: 'Create static room' })).toBeTruthy();
  });
});

describe('rotating an attach secret', () => {
  it('confirms first — the old secret dies the moment it is rotated', async () => {
    const session = mount([STATIC], (path, init) =>
      path.endsWith('/rotate-secret') && init.method === 'POST'
        ? json({ room: STATIC, attachSecret: 'RotatedSecretValue123456' })
        : json({}),
    );
    await screen.findByText('TuhisRoom');
    fireEvent.click(within(rowFor('TuhisRoom')).getByRole('button', { name: 'Rotate secret' }));
    const dialog = await screen.findByRole('dialog');
    expect(session.calls.some((c) => c.path.endsWith('/rotate-secret'))).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(session.calls.some((c) => c.path.endsWith('/rotate-secret'))).toBe(false);

    fireEvent.click(within(rowFor('TuhisRoom')).getByRole('button', { name: 'Rotate secret' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Rotate' }));
    await waitFor(() => {
      expect(
        session.calls.some(
          (c) => c.path === 'api/v1/rooms/tuhisroom/rotate-secret' && c.init.method === 'POST',
        ),
      ).toBe(true);
    });
    const secret = (await screen.findByLabelText('Attach secret')) as HTMLInputElement;
    expect(secret.value).toBe('RotatedSecretValue123456');
  });

  it('surfaces a refusal inside the dialog', async () => {
    mount([STATIC], (path) =>
      path.endsWith('/rotate-secret')
        ? json({ error: { code: 'room_not_static', message: 'only a static room has an attach secret' } }, 409)
        : json({}),
    );
    await screen.findByText('TuhisRoom');
    fireEvent.click(within(rowFor('TuhisRoom')).getByRole('button', { name: 'Rotate secret' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Rotate' }));
    expect(await screen.findByText(/only a static room/)).toBeTruthy();
  });
});

describe('ending and deleting', () => {
  it('ends a dynamic room through /end after the confirm', async () => {
    const session = mount([DYNAMIC]);
    await screen.findByText('R7K3MX');
    fireEvent.click(within(rowFor('R7K3MX')).getByRole('button', { name: 'End room' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/close code 4007/)).toBeTruthy();
    expect(session.calls.some((c) => c.init.method === 'POST')).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: 'End room' }));
    await waitFor(() => {
      expect(
        session.calls.some((c) => c.path === 'api/v1/rooms/r7k3mx/end' && c.init.method === 'POST'),
      ).toBe(true);
    });
    expect(session.calls.some((c) => c.init.method === 'DELETE')).toBe(false);
  });

  it('deletes a static room through DELETE after the confirm', async () => {
    const session = mount([STATIC]);
    await screen.findByText('TuhisRoom');
    fireEvent.click(within(rowFor('TuhisRoom')).getByRole('button', { name: 'Delete' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));
    await waitFor(() => {
      expect(
        session.calls.some((c) => c.path === 'api/v1/rooms/tuhisroom' && c.init.method === 'DELETE'),
      ).toBe(true);
    });
  });

  it('keeps a failed end visible rather than closing the dialog', async () => {
    mount([DYNAMIC], (path) =>
      path.endsWith('/end')
        ? json({ error: { code: 'not_found', message: 'no such room' } }, 404)
        : json({}),
    );
    await screen.findByText('R7K3MX');
    fireEvent.click(within(rowFor('R7K3MX')).getByRole('button', { name: 'End room' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'End room' }));
    expect(await screen.findByText('no such room')).toBeTruthy();
    expect(screen.getByRole('dialog')).toBeTruthy();
  });
});
