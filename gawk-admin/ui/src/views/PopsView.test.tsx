// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { PopsView } from './PopsView.tsx';
import type { Pop, PopWithPairing } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

const BUDGET = { maxMbps: 25, maxStreams: 3 };

const LINKED: Pop = {
  name: 'assembly-2026',
  displayName: 'Assembly 2026',
  state: 'linked',
  stateSince: '2026-10-21T18:04:11Z',
  notAfter: '2026-11-09T00:00:00Z',
  createdAt: '2026-10-21T18:00:00Z',
  managed: true,
  revoked: false,
  pairing: { issuedAt: '2026-10-21T18:00:00Z', expiresAt: '2026-10-24T18:00:00Z', paired: true },
  uplink: BUDGET,
  downlink: { maxMbps: 0, maxStreams: 0 },
  limits: { maxBroadcasts: 40, maxDownlinkStreams: 20 },
  guests: { autoSwitch: true },
  wall: { roomCode: 'assembly-2026' },
  namedAddress: { enabled: false },
  link: {
    connected: true,
    since: '2026-10-21T18:04:11Z',
    version: '0.44.0',
    publicAddrs: ['203.0.113.7'],
    lanAddr: '192.168.1.20',
    pod: 'gawk-server-1',
  },
  live: {
    pod: 'gawk-server-1',
    live: 7,
    forwarded: 2,
    outsideViewers: 11,
    uplinkMbps: 14.5,
    uplinkStreams: 2,
    uplink: BUDGET,
    downlink: { maxMbps: 0, maxStreams: 0 },
    downlinkStreams: 1,
    guestsConnected: 23,
    offersSent: 19,
  },
};

const WAITING: Pop = {
  ...LINKED,
  name: 'lanparty',
  displayName: 'LAN party',
  state: 'waiting-for-pairing',
  pairing: { issuedAt: '2026-10-21T18:00:00Z', expiresAt: '2026-10-24T18:00:00Z', paired: false },
  wall: undefined,
  link: undefined,
  live: undefined,
};

const REVOKED: Pop = { ...WAITING, name: 'old-party', displayName: 'Old party', state: 'revoked', revoked: true };

const DETAIL: Pop = {
  ...LINKED,
  broadcasts: [
    {
      id: 'ABC234',
      label: 'main pc',
      publisherLanAddr: '192.168.1.41',
      localViewers: 5,
      outsideViewers: 4,
      forwarded: true,
      wall: 'on',
      links: { watch: 'https://gawk.example/#/view/ABC234' },
    },
    { id: 'DEF567', label: 'Stream 2', localViewers: 2, outsideViewers: 0, forwarded: false, wall: 'hidden' },
  ],
  health: [
    { key: 'link', ok: true, detail: 'linked to gawk-server-1' },
    { key: 'firewall', ok: false, detail: 'UDP 4433 is blocked' },
  ],
};

const PAIRING = 'gawk-pair:api.gawk.ioio.fi:4433/K7M2Q-9XDA4-RT3VB-6NWPE-HJ5LC-Y';

function created(pop: Pop): PopWithPairing {
  return { pop, pairingString: PAIRING, expiresAt: '2026-10-24T18:00:00Z' };
}

function mount(
  pops: Pop[] | Response,
  extra?: (path: string, init: RequestInit) => Response,
  props: { detail?: string; namedAddress?: boolean; rooms?: boolean } = {},
) {
  const session = stubSession((path, init) => {
    if (path === 'api/v1/pops' && (init.method ?? 'GET') === 'GET') {
      return pops instanceof Response ? pops : json({ pops });
    }
    if (extra) return extra(path, init);
    return new Response(null, { status: 204 });
  });
  renderWithSession(
    <PopsView detail={props.detail ?? ''} namedAddress={props.namedAddress ?? false} rooms={props.rooms ?? true} />,
    session,
  );
  return session;
}

function cardFor(name: string): HTMLElement {
  const el = screen.getByText(name).closest('article');
  if (!el) throw new Error(`no card for ${name}`);
  return el as HTMLElement;
}

describe('the PoPs list (docs/76 §7.2)', () => {
  it('shows a card per PoP with its state, counts and uplink against the budget', async () => {
    mount([LINKED, WAITING, REVOKED]);
    expect(await screen.findByText('Assembly 2026')).toBeTruthy();
    const linked = within(cardFor('Assembly 2026'));
    expect(linked.getByText(/^Linked/)).toBeTruthy();
    expect(linked.getByText('7 live · 2 forwarded')).toBeTruthy();
    expect(linked.getByText(/11 outside viewers/)).toBeTruthy();
    expect(linked.getByText(/14\.5 \/ 25 Mbps/)).toBeTruthy();
    expect(linked.getByText(/19 offers · 23 guests here/)).toBeTruthy();
    expect(linked.getByText(/0\.44\.0/)).toBeTruthy();
    expect(within(cardFor('LAN party')).getByText('Waiting for pairing')).toBeTruthy();
    expect(within(cardFor('Old party')).getByText('Revoked')).toBeTruthy();
  });

  it('says PoPs are off when the API answers 404', async () => {
    mount(json({ error: { code: 'not_found', message: 'no such endpoint' } }, 404));
    expect(await screen.findByText(/PoPs are not enabled on this deployment/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'New PoP' }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('the New PoP wizard', () => {
  it('walks the four steps and shows the pairing string once', async () => {
    const session = mount([], (path, init) => {
      if (path === 'api/v1/pops' && init.method === 'POST') return json(created(WAITING), 201);
      return new Response(null, { status: 204 });
    });
    fireEvent.click(await screen.findByRole('button', { name: 'New PoP' }));

    // 1. Name, display name, end date.
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'lanparty' } });
    fireEvent.change(screen.getByLabelText(/^Display name/), { target: { value: 'LAN party' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    // 2. Party internet.
    fireEvent.change(screen.getByLabelText(/^Uplink Mbps/), { target: { value: '30' } });
    fireEvent.change(screen.getByLabelText(/^Uplink streams/), { target: { value: '4' } });
    fireEvent.change(screen.getByLabelText(/^Party broadcasts home carries/), { target: { value: '50' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    // 3. Guests: the wall on, with the default code; named address unavailable.
    fireEvent.click(screen.getByLabelText(/Party wall/));
    expect((screen.getByLabelText(/Named address/) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText(/needs -pop-dns-zone and -pop-cert-issuer/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Create and pair' }));

    // 4. Pair: the string once, the docker line, the expiry.
    const input = (await screen.findByLabelText('Pairing string')) as HTMLInputElement;
    expect(input.value).toBe(PAIRING);
    expect(input.readOnly).toBe(true);
    expect(screen.getByText(new RegExp(`GAWK_PAIR='${PAIRING.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}'`))).toBeTruthy();
    expect(screen.getByText(/Shown once/)).toBeTruthy();
    // The relay's site page, which explains it before the download (not the
    // bare releases list).
    expect(screen.getByRole('link', { name: /Download gawk-relay/ }).getAttribute('href')).toBe(
      'https://tuhis.github.io/gawk/party.html',
    );

    const post = session.calls.find((c) => c.path === 'api/v1/pops' && c.init.method === 'POST');
    expect(post).toBeTruthy();
    expect(bodyOf(post!)).toEqual({
      name: 'lanparty',
      displayName: 'LAN party',
      uplink: { maxMbps: 30, maxStreams: 4 },
      limits: { maxBroadcasts: 50 },
      guests: { autoSwitch: true },
      wall: {},
    });

    fireEvent.click(screen.getByRole('button', { name: 'I have copied it' }));
    await waitFor(() => expect(screen.queryByLabelText('Pairing string')).toBeNull());
    expect(screen.queryByText(PAIRING)).toBeNull();
  });

  it('refuses a name that is no DNS label before a round trip, and says why a taken wall code failed', async () => {
    mount([], (path, init) => {
      if (path === 'api/v1/pops' && init.method === 'POST') {
        return json({ error: { code: 'wall_code_taken', message: 'taken' } }, 409);
      }
      return new Response(null, { status: 204 });
    });
    fireEvent.click(await screen.findByRole('button', { name: 'New PoP' }));
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'Bad_Name' } });
    expect((screen.getByRole('button', { name: 'Next' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'party' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByLabelText(/Party wall/));
    fireEvent.click(screen.getByRole('button', { name: 'Create and pair' }));
    expect(await screen.findByText(/wall code is already a room/)).toBeTruthy();
  });

  it('offers no wall without rooms, and the named address when configured', async () => {
    mount([], undefined, { rooms: false, namedAddress: true });
    fireEvent.click(await screen.findByRole('button', { name: 'New PoP' }));
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'party' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect((screen.getByLabelText(/Party wall/) as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText(/Named address/) as HTMLInputElement).disabled).toBe(false);
  });
});

describe('the PoP detail', () => {
  it('lists live broadcasts, health and addresses', async () => {
    mount([], (path) => (path === 'api/v1/pops/assembly-2026' ? json(DETAIL) : new Response(null, { status: 204 })), {
      detail: 'assembly-2026',
    });
    expect(await screen.findByText('main pc')).toBeTruthy();
    const main = within(screen.getByText('main pc').closest('tr') as HTMLElement);
    expect(main.getByText('192.168.1.41')).toBeTruthy();
    expect(main.getByText('forwarded')).toBeTruthy();
    expect(main.getByRole('link', { name: 'Watch' }).getAttribute('href')).toBe('https://gawk.example/#/view/ABC234');
    const second = within(screen.getByText('Stream 2').closest('tr') as HTMLElement);
    expect(second.getByText('party-only')).toBeTruthy();
    expect(second.getByText('hidden')).toBeTruthy();
    expect(screen.getByText('UDP 4433 is blocked')).toBeTruthy();
    expect(screen.getByText('203.0.113.7')).toBeTruthy();
    expect(screen.getByText('192.168.1.20')).toBeTruthy();
  });

  it('re-pairs after a confirmation and shows the new string once', async () => {
    const session = mount(
      [],
      (path, init) => {
        if (path === 'api/v1/pops/assembly-2026') return json(DETAIL);
        if (path === 'api/v1/pops/assembly-2026/repair' && init.method === 'POST') {
          return json(created({ ...LINKED, state: 'waiting-for-pairing' }));
        }
        return new Response(null, { status: 204 });
      },
      { detail: 'assembly-2026' },
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Re-pair' }));
    expect(screen.getByText(/stops working at once/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Re-pair now' }));
    const input = (await screen.findByLabelText('Pairing string')) as HTMLInputElement;
    expect(input.value).toBe(PAIRING);
    expect(session.calls.some((c) => c.path === 'api/v1/pops/assembly-2026/repair')).toBe(true);
  });

  it('revokes after a confirmation', async () => {
    const session = mount(
      [],
      (path, init) => {
        if (path === 'api/v1/pops/assembly-2026') return json(DETAIL);
        if (path === 'api/v1/pops/assembly-2026/revoke' && init.method === 'POST') return json(REVOKED);
        return new Response(null, { status: 204 });
      },
      { detail: 'assembly-2026' },
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
    fireEvent.click(screen.getByRole('button', { name: 'Revoke now' }));
    await waitFor(() => expect(session.calls.some((c) => c.path === 'api/v1/pops/assembly-2026/revoke')).toBe(true));
  });

  it('edits the budget with a PATCH of only what changed', async () => {
    const session = mount(
      [],
      (path, init) => {
        if (path === 'api/v1/pops/assembly-2026' && (init.method ?? 'GET') === 'GET') return json(DETAIL);
        if (path === 'api/v1/pops/assembly-2026' && init.method === 'PATCH') return json(DETAIL);
        return new Response(null, { status: 204 });
      },
      { detail: 'assembly-2026' },
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    fireEvent.change(screen.getByLabelText(/^Uplink Mbps/), { target: { value: '40' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(session.calls.some((c) => c.init.method === 'PATCH')).toBe(true));
    const patch = session.calls.find((c) => c.init.method === 'PATCH')!;
    expect(bodyOf(patch)).toEqual({ uplink: { maxMbps: 40, maxStreams: 3 } });
  });
});

describe('the PoPs view: other states, errors and cancels', () => {
  it('shows an offline PoP with when it went offline', async () => {
    mount([{ ...LINKED, state: 'offline', stateSince: '2026-10-21T21:04:00Z', live: undefined }]);
    expect(await screen.findByText('Assembly 2026')).toBeTruthy();
    expect(within(cardFor('Assembly 2026')).getByText(/^Offline since/)).toBeTruthy();
  });

  it('sends every optional wizard field, and says when the name is taken', async () => {
    const session = mount([], (path, init) => {
      if (path === 'api/v1/pops' && init.method === 'POST') {
        return json({ error: { code: 'pop_exists', message: 'exists' } }, 409);
      }
      return new Response(null, { status: 204 });
    }, { namedAddress: true });
    fireEvent.click(await screen.findByRole('button', { name: 'New PoP' }));
    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'party' } });
    fireEvent.change(screen.getByLabelText(/^End date/), { target: { value: '2026-11-09T00:00' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.change(screen.getByLabelText(/^Downlink Mbps/), { target: { value: '100' } });
    fireEvent.change(screen.getByLabelText(/^Downlink streams/), { target: { value: '5' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(screen.getByLabelText(/Auto-switch/));
    fireEvent.click(screen.getByLabelText(/Party wall/));
    fireEvent.change(screen.getByLabelText(/^Wall code/), { target: { value: 'the-wall' } });
    fireEvent.click(screen.getByLabelText(/Named address/));
    fireEvent.click(screen.getByRole('button', { name: 'Create and pair' }));
    expect(await screen.findByText('A PoP with that name already exists.')).toBeTruthy();

    const post = session.calls.find((c) => c.path === 'api/v1/pops' && c.init.method === 'POST')!;
    expect(bodyOf(post)).toEqual({
      name: 'party',
      notAfter: new Date('2026-11-09T00:00').toISOString(),
      downlink: { maxMbps: 100, maxStreams: 5 },
      guests: { autoSwitch: false },
      wall: { roomCode: 'the-wall' },
      namedAddress: true,
    });

    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByLabelText(/^Name/)).toBeNull());
  });

  it('says why a re-pair failed, and cancels the confirmation', async () => {
    mount(
      [],
      (path, init) => {
        if (path === 'api/v1/pops/assembly-2026') return json(DETAIL);
        if (path === 'api/v1/pops/assembly-2026/repair' && init.method === 'POST') {
          return json({ error: { code: 'pop_revoked', message: 'revoked' } }, 409);
        }
        return new Response(null, { status: 204 });
      },
      { detail: 'assembly-2026' },
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Re-pair' }));
    fireEvent.click(screen.getByRole('button', { name: 'Re-pair now' }));
    expect(await screen.findByText(/revoked: a new pairing string would pair nothing/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Re-pair now' })).toBeNull());
  });

  it('patches every edited field, shows a failed save, and cancels', async () => {
    let fail = true;
    const session = mount(
      [],
      (path, init) => {
        if (path === 'api/v1/pops/assembly-2026' && (init.method ?? 'GET') === 'GET') return json(DETAIL);
        if (path === 'api/v1/pops/assembly-2026' && init.method === 'PATCH') {
          return fail ? json({ error: { code: '', message: 'the API is down' } }, 500) : json(DETAIL);
        }
        return new Response(null, { status: 204 });
      },
      { detail: 'assembly-2026', namedAddress: true },
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    fireEvent.change(screen.getByLabelText(/^Display name/), { target: { value: 'Assembly 26' } });
    fireEvent.change(screen.getByLabelText(/^Downlink Mbps/), { target: { value: '50' } });
    fireEvent.change(screen.getByLabelText(/^Party broadcasts home carries/), { target: { value: '' } });
    fireEvent.click(screen.getByLabelText(/Auto-switch/));
    fireEvent.click(screen.getByLabelText(/Named address/));
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('the API is down')).toBeTruthy();
    const patch = session.calls.find((c) => c.init.method === 'PATCH')!;
    expect(bodyOf(patch)).toEqual({
      displayName: 'Assembly 26',
      downlink: { maxMbps: 50 },
      limits: {},
      guests: { autoSwitch: false },
      namedAddress: true,
    });
    fail = false;
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save' })).toBeNull());
  });
});
