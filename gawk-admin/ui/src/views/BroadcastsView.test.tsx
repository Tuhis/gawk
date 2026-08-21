// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { BroadcastsView } from './BroadcastsView.tsx';
import type { Broadcast } from '../api/types.ts';
import { bodyOf, json, renderWithSession, stubSession } from '../testing/harness.tsx';

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
    banState: { banned: false, ban: null },
    ...over,
  };
}

function mount(broadcasts: Broadcast[], cooldown = 600) {
  const session = stubSession((path) => {
    if (path === 'api/v1/broadcasts') return json({ broadcasts });
    return json({});
  });
  // `refreshMs={0}`: the 5 s poll is production behaviour, not test behaviour.
  // Left on, a slow file (a cold transform is enough) lets it fire mid-test and
  // push state updates outside `act` while assertions are running.
  const view = renderWithSession(
    <BroadcastsView killCooldownSeconds={cooldown} refreshMs={0} />,
    session,
  );
  return { session, view };
}

describe('the fleet table', () => {
  it('shows the raw id, the HMAC’d key and the publisher IP', async () => {
    mount([broadcast()]);
    // Raw IDs are allowed here and nowhere near a webhook or a log (D8).
    expect(await screen.findByText('ABC123')).toBeTruthy();
    expect(screen.getByText('3f9a1c2b4d5e')).toBeTruthy();
    expect(screen.getByText('203.0.113.7')).toBeTruthy();
    expect(screen.getByText('12')).toBeTruthy();
    expect(screen.getByText(/gawk-server-0/)).toBeTruthy();
  });

  it('marks a broadcast whose publisher is away', async () => {
    mount([broadcast({ publisherActive: false })]);
    expect(await screen.findByText('away')).toBeTruthy();
  });

  // Three states, and the third is the one that matters. AP4 degrades the fleet
  // read rather than 503ing it when Postgres is unreachable, so `banState` can
  // be null — and calling that "not banned" would tell an operator a banned
  // broadcast is clean.
  it('shows an existing ban on the row', async () => {
    mount([broadcast({ banState: { banned: true, ban: null } })]);
    expect(await screen.findByText('banned')).toBeTruthy();
  });

  it('shows no ban when the store says there is none', async () => {
    mount([broadcast({ banState: { banned: false, ban: null } })]);
    await screen.findByText('ABC123');
    expect(screen.queryByText('banned')).toBeNull();
    expect(screen.queryByText('unknown')).toBeNull();
  });

  it('says "unknown" — never "not banned" — when the ban store was unreachable', async () => {
    mount([broadcast({ banState: null })]);
    expect(await screen.findByText('unknown')).toBeTruthy();
    expect(screen.queryByText('banned')).toBeNull();
    // The fleet itself is still readable during the outage; that is the point
    // of the degraded read.
    expect(screen.getByText('ABC123')).toBeTruthy();
  });
});

describe('deep links (§4.12, AP6)', () => {
  it('renders watch and telemetry links exactly as the server sent them', async () => {
    mount([
      broadcast({
        links: {
          watch: 'https://gawk.ioio.fi/#/view/ABC123',
          telemetry: 'https://telemetry.internal/#/broadcast/3f9a1c2b4d5e',
        },
      }),
    ]);
    const watch = await screen.findByRole('link', { name: 'Watch' });
    expect(watch.getAttribute('href')).toBe('https://gawk.ioio.fi/#/view/ABC123');
    expect(screen.getByRole('link', { name: 'Telemetry' }).getAttribute('href')).toBe(
      'https://telemetry.internal/#/broadcast/3f9a1c2b4d5e',
    );
  });

  it('hides a link the server omitted, rather than guessing a base URL', async () => {
    // `-app-base-url` / `-telemetry-base-url` empty ⇒ the server omits the
    // link ⇒ there is nothing to render. A link to nowhere is worse than none.
    mount([broadcast({ links: {} })]);
    await screen.findByText('ABC123');
    expect(screen.queryByRole('link', { name: 'Watch' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Telemetry' })).toBeNull();
  });

  it('hides both when the server sent no links object at all', async () => {
    mount([broadcast({ links: undefined })]);
    await screen.findByText('ABC123');
    expect(screen.queryByRole('link', { name: 'Watch' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Telemetry' })).toBeNull();
  });
});

describe('the kill dialog (§4.9, AP6)', () => {
  async function openKill(cooldown = 600) {
    const h = mount([broadcast()], cooldown);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    return h;
  }

  it('pre-fills the cooldown from the server’s configured default', async () => {
    // Not a constant in the SPA: a deployment that tuned `-kill-cooldown` must
    // see its own number here.
    await openKill(900);
    const field = screen.getByLabelText('Cooldown (seconds)') as HTMLInputElement;
    expect(field.value).toBe('900');
  });

  it('refuses to submit without a reason, and says why', async () => {
    const { session } = await openKill();
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    expect(screen.getByRole('alert').textContent).toMatch(/reason is required/i);
    expect(session.calls.some((c) => c.path.includes('/kill'))).toBe(false);
  });

  // Regression (PR #280 review). `Number('')` is 0, and the server 400s on
  // `cooldownSeconds <= 0` — so clearing the field to type a new value and
  // hitting Kill produced a refusal from the API, at the final confirm,
  // mid-incident. The field is a string until it is submitted, so clearing it
  // shows an empty box rather than a `0` to type in front of.
  it('refuses a cleared cooldown here rather than letting the server 400 on 0', async () => {
    const { session } = await openKill();
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'terms violation' },
    });
    const field = screen.getByLabelText('Cooldown (seconds)') as HTMLInputElement;
    fireEvent.change(field, { target: { value: '' } });
    expect(field.value).toBe('');

    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));
    expect(screen.getByRole('alert').textContent).toMatch(/cooldown/i);
    expect(session.calls.some((c) => c.path.includes('/kill'))).toBe(false);
  });

  it('refuses a zero cooldown, which the server rejects as not positive', async () => {
    const { session } = await openKill();
    fireEvent.change(screen.getByLabelText('Reason (required)'), { target: { value: 'spam' } });
    fireEvent.change(screen.getByLabelText('Cooldown (seconds)'), { target: { value: '0' } });
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    expect(screen.getByRole('alert').textContent).toMatch(/cooldown/i);
    expect(session.calls.some((c) => c.path.includes('/kill'))).toBe(false);
    // The spinner's own floor agreed with the server, so 0 is not reachable by
    // stepping either.
    expect(screen.getByLabelText('Cooldown (seconds)').getAttribute('min')).toBe('1');
  });

  it('posts the reason and the cooldown once both are given', async () => {
    const { session } = await openKill();
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'terms violation' },
    });
    fireEvent.click(screen.getByRole('button', { name: '30 min' }));
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    await waitFor(() => {
      expect(session.calls.some((c) => c.path === 'api/v1/broadcasts/ABC123/kill')).toBe(true);
    });
    const call = session.calls.find((c) => c.path === 'api/v1/broadcasts/ABC123/kill');
    expect(bodyOf(call!)).toEqual({ reason: 'terms violation', cooldownSeconds: 1800 });
  });
});

describe('the ban dialog (§4.9, AP6)', () => {
  async function openBan(fleet: Broadcast[]) {
    const h = mount(fleet);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);
    return h;
  }

  it('offers the duration presets including permanent', async () => {
    await openBan([broadcast()]);
    for (const label of ['1 hour', '24 hours', '7 days', 'permanent']) {
      expect(screen.getByLabelText(label)).toBeTruthy();
    }
  });

  it('shows the resolved publisher IP behind an opt-in checkbox, defaulting to /32', async () => {
    await openBan([broadcast()]);
    const box = screen.getByLabelText(/Also ban the publisher IP/) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(screen.getAllByText('203.0.113.7').length).toBeGreaterThan(0);
    expect((screen.getByLabelText('Prefix') as HTMLSelectElement).value).toBe('32');
  });

  it('defaults an IPv6 publisher to /64, not /128', async () => {
    await openBan([broadcast({ publisherRemoteIp: '2001:db8:1234:5678::9' })]);
    expect((screen.getByLabelText('Prefix') as HTMLSelectElement).value).toBe('64');
    // Shown beside the selector and again inside the NAT caveat.
    expect(screen.getAllByText('2001:db8:1234:5678::9/64').length).toBeGreaterThan(0);
  });

  it('always states the NAT-collateral caveat', async () => {
    await openBan([broadcast()]);
    expect(screen.getByText(/also blocks anyone sharing/i)).toBeTruthy();
  });

  it('bans the ID alone when the IP box is left unchecked', async () => {
    const { session } = await openBan([broadcast()]);
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'repeat offender' },
    });
    fireEvent.click(screen.getByLabelText('7 days'));
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    await waitFor(() => {
      expect(session.calls.filter((c) => c.path === 'api/v1/bans')).toHaveLength(1);
    });
    const body = bodyOf(session.calls.find((c) => c.path === 'api/v1/bans')!) as {
      target: { type: string; value: string };
      expiresAt: string;
      reason: string;
    };
    expect(body.target).toEqual({ type: 'broadcastId', value: 'ABC123' });
    expect(body.reason).toBe('repeat offender');
    expect(Date.parse(body.expiresAt) - Date.now()).toBeGreaterThan(6 * 86_400_000);
  });

  it('adds a publisher-IP ban with the confirmed prefix when the box is ticked', async () => {
    const { session } = await openBan([broadcast()]);
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'ban evasion' },
    });
    fireEvent.click(screen.getByLabelText(/Also ban the publisher IP/));
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    await waitFor(() => {
      expect(session.calls.filter((c) => c.path === 'api/v1/bans')).toHaveLength(2);
    });
    const bodies = session.calls.filter((c) => c.path === 'api/v1/bans').map(bodyOf) as {
      target: { type: string; value: string; prefixLength?: number };
      sourceBroadcastId: string;
    }[];
    // Found by target type, not by position: which of the two writes goes
    // first is a separate property, asserted in "kill + ban is two writes".
    const ipBan = bodies.find((b) => b.target.type === 'ip')!;
    // §4.7: the literal "publisher" plus sourceBroadcastId, so the SERVER
    // resolves the address through relayscan rather than trusting an IP this
    // page read seconds ago.
    expect(ipBan.target).toEqual({ type: 'ip', value: 'publisher', prefixLength: 32 });
    expect(ipBan.sourceBroadcastId).toBe('ABC123');
  });

  it('refuses to submit without a reason', async () => {
    const { session } = await openBan([broadcast()]);
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));
    expect(screen.getByRole('alert').textContent).toMatch(/reason is required/i);
    expect(session.calls.some((c) => c.path === 'api/v1/bans')).toBe(false);
  });

  it('says so when no publisher IP is known, instead of offering a ban it cannot build', async () => {
    await openBan([broadcast({ publisherRemoteIp: null })]);
    expect(screen.queryByLabelText(/Also ban the publisher IP/)).toBeNull();
    expect(screen.getByText(/No publisher IP is known/)).toBeTruthy();
  });
});

describe('the shared-IP warning (§4.9, §5)', () => {
  const shared = '198.51.100.9';

  it('fires when the fleet’s publishers all look like the same address', async () => {
    const fleet = [
      broadcast({ id: 'AAA111', publisherRemoteIp: shared }),
      broadcast({ id: 'BBB222', publisherRemoteIp: shared }),
      broadcast({ id: 'CCC333', publisherRemoteIp: '203.0.113.7' }),
    ];
    const session = stubSession((path) =>
      path === 'api/v1/broadcasts' ? json({ broadcasts: fleet }) : json({}),
    );
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);

    const alert = screen.getByRole('alert');
    expect(alert.textContent).toMatch(/2 of 3 live broadcasts/);
    // The two facts an operator needs: the cause, and the blast radius.
    expect(alert.textContent).toMatch(/externalTrafficPolicy/);
    expect(alert.textContent).toMatch(/every broadcast on the fleet/);
  });

  it('stays quiet when publishers have distinct addresses', async () => {
    const fleet = [
      broadcast({ id: 'AAA111', publisherRemoteIp: '203.0.113.1' }),
      broadcast({ id: 'BBB222', publisherRemoteIp: '203.0.113.2' }),
      broadcast({ id: 'CCC333', publisherRemoteIp: '203.0.113.3' }),
    ];
    const session = stubSession((path) =>
      path === 'api/v1/broadcasts' ? json({ broadcasts: fleet }) : json({}),
    );
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);
    expect(screen.queryByText(/externalTrafficPolicy/)).toBeNull();
  });
});

describe('server refusals', () => {
  it('shows the API’s own words when a kill hits an existing ban', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path.endsWith('/kill')) {
        return json(
          { error: { code: 'ban_exists', message: 'an active ban already covers ABC123' } },
          409,
        );
      }
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'spam' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    expect(await screen.findByText(/an active ban already covers ABC123/)).toBeTruthy();
  });

  // 202 Accepted is the awkward middle state and the one most likely to be
  // mishandled: the Postgres row IS committed, only the Ban CR write failed.
  // It arrives on a SUCCESS response, so nothing throws — the view has to
  // notice `enforcement.inSync: false` and go amber. Treating it as a failure
  // invites a retry that will now 409 against the row that does exist;
  // treating it as a plain success claims an enforcement that has not started.
  const PENDING_BAN = {
    id: 'committed',
    state: 'active',
    enforcement: {
      inSync: false,
      detail:
        'The ban is recorded but NOT enforced yet — its Kubernetes enforcement object could not be written; the reconciler retries within a minute, so do not re-submit.',
    },
  };

  it('reports a ban that was recorded but not yet enforced (202)', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path === 'api/v1/bans') return json(PENDING_BAN, 202);
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'terms' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    const note = await screen.findByRole('alert');
    expect(note.textContent).toMatch(/recorded/i);
    expect(note.textContent).toMatch(/NOT enforced yet/);
    expect(note.textContent).toMatch(/do not re-submit/i);
    // Which broadcast, and no green "Banned ABC123." claiming it is done.
    expect(note.textContent).toMatch(/ABC123/);
    expect(screen.queryByText('Banned ABC123.')).toBeNull();
    // The dialog is done with: there is nothing to retry.
    expect(screen.queryByRole('button', { name: 'Kill and ban' })).toBeNull();
  });

  it('does the same for a plain kill that could not be projected', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path.endsWith('/kill')) return json({ ban: PENDING_BAN }, 202);
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    fireEvent.change(screen.getByLabelText('Reason (required)'), { target: { value: 'spam' } });
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    expect((await screen.findByRole('alert')).textContent).toMatch(/NOT enforced yet/);
    expect(screen.queryByText('Killed ABC123.')).toBeNull();
  });

  // The clean case still reads as clean: a 201 carries no `enforcement`, so
  // nothing goes amber and the operator gets the plain confirmation.
  it('says a broadcast was already banned rather than showing the 409 as a failure', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path === 'api/v1/bans') {
        return json(
          {
            error: { code: 'duplicate_active', message: 'an active ban already covers this target' },
            ban: { id: 'ban-id', state: 'active' },
          },
          409,
        );
      }
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);
    fireEvent.change(screen.getByLabelText('Reason (required)'), { target: { value: 'terms' } });
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    // The state the operator asked for is the state that exists, so this is
    // not a red error — but it must not claim this click did it either.
    expect(await screen.findByText('ABC123 was already banned.')).toBeTruthy();
    expect(screen.queryByText('Banned ABC123.')).toBeNull();
  });

  it('shows a plain confirmation when the CR was written too (201)', async () => {
    const session = stubSession((path) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path.endsWith('/kill')) return json({ ban: { id: 'b1', state: 'active' } }, 201);
      return json({});
    });
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click(await screen.findByRole('button', { name: 'Kill' }));
    fireEvent.change(screen.getByLabelText('Reason (required)'), { target: { value: 'spam' } });
    fireEvent.click(screen.getByRole('button', { name: 'Kill broadcast' }));

    expect(await screen.findByText('Killed ABC123.')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

/**
 * Kill + ban is TWO writes, and the order is load-bearing (PR #280 review).
 *
 * The IP ban goes FIRST. Its server-side `"publisher"` resolve reads the live
 * publisher's address through relayscan, so it has to run before anything has
 * started terminating the session: the ID ban's handler calls
 * `afterMutation()`, which invalidates the relayscan fleet cache, so an IP ban
 * placed after it does a *fresh* scan racing the kill the ID ban just
 * triggered — and loses, 400ing on a publisher that has already gone. Placing
 * it first costs nothing, because the IP ban's own AP3 actuation ends the
 * session just the same.
 *
 * The ID ban is then attempted WHATEVER happened to the IP ban: it is the
 * action that must land, and letting a 503 on the IP half abort it would leave
 * the operator with a live broadcast and nothing enforced.
 *
 * Both writes tolerate `409 duplicate_active`, so clicking again retries only
 * what is missing, in either direction.
 */
describe('kill + ban is two writes (§4.9, AP3)', () => {
  const created = (id: string) => json({ id, state: 'active' }, 201);
  const duplicate = (id: string) =>
    json(
      {
        error: { code: 'duplicate_active', message: 'an active ban already covers this target' },
        ban: { id, state: 'active' },
      },
      409,
    );
  const unresolved = () =>
    json(
      { error: { code: 'invalid_target', message: "the live publisher's IP could not be resolved" } },
      400,
    );
  const unavailable = () =>
    json({ error: { code: 'unavailable', message: 'the ban store is unreachable' } }, 503);

  /** A `/bans` stub answering per target type, recording the order they went out in. */
  function banStub(plan: { ip: (n: number) => Response; id: (n: number) => Response }) {
    const sent: string[] = [];
    let ip = 0;
    let id = 0;
    const session = stubSession((path, init) => {
      if (path === 'api/v1/broadcasts') return json({ broadcasts: [broadcast()] });
      if (path !== 'api/v1/bans') return json({});
      const body = JSON.parse(String(init.body)) as { target: { type: string } };
      sent.push(body.target.type);
      return body.target.type === 'ip' ? plan.ip(++ip) : plan.id(++id);
    });
    return { session, sent, ip: () => ip, id: () => id };
  }

  async function submitKillAndBan(session: ReturnType<typeof stubSession>, withIp = true) {
    renderWithSession(<BroadcastsView killCooldownSeconds={600} refreshMs={0} />, session);
    fireEvent.click((await screen.findAllByRole('button', { name: 'Kill + ban' }))[0]);
    fireEvent.change(screen.getByLabelText('Reason (required)'), {
      target: { value: 'ban evasion' },
    });
    if (withIp) fireEvent.click(screen.getByLabelText(/Also ban the publisher IP/));
    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));
  }

  it('places the publisher-IP ban first, while the publisher is still live', async () => {
    const h = banStub({ ip: () => created('ban-ip'), id: () => created('ban-id') });
    await submitKillAndBan(h.session);

    await waitFor(() => expect(h.sent).toHaveLength(2));
    expect(h.sent).toEqual(['ip', 'broadcastId']);
    expect(await screen.findByText('Banned ABC123 and its publisher IP.')).toBeTruthy();
  });

  it('still bans the broadcast when the publisher-IP ban fails outright', async () => {
    // A 503 rather than a failed resolve: the ID ban must survive ANY failure
    // of the IP half, not just the one the ordering was chosen to avoid.
    const h = banStub({ ip: unavailable, id: () => created('ban-id') });
    await submitKillAndBan(h.session);

    await waitFor(() => expect(h.sent).toEqual(['ip', 'broadcastId']));
    // Honest about which half is in force — that is what decides what the
    // operator does next — and the dialog stays open so they can do it.
    const alert = await screen.findByText(/ABC123 is banned, but its publisher IP is not/);
    expect(alert.textContent).toMatch(/the ban store is unreachable/);
    expect(screen.getByRole('button', { name: 'Kill and ban' })).toBeTruthy();
    expect(screen.queryByText('Banned ABC123 and its publisher IP.')).toBeNull();
  });

  it('retries only the IP ban when the broadcast ban already landed', async () => {
    const h = banStub({
      ip: (n) => (n === 1 ? unresolved() : created('ban-ip')),
      id: (n) => (n === 1 ? created('ban-id') : duplicate('ban-id')),
    });
    await submitKillAndBan(h.session);

    expect(await screen.findByText(/could not be resolved/)).toBeTruthy();
    expect(h.sent).toEqual(['ip', 'broadcastId']);

    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    await waitFor(() => expect(h.ip()).toBe(2));
    expect(h.sent).toEqual(['ip', 'broadcastId', 'ip', 'broadcastId']);
    // The 409 on the second broadcast ban is the state we want, not a failure.
    expect(
      await screen.findByText("Banned ABC123's publisher IP; ABC123 was already banned."),
    ).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Kill and ban' })).toBeNull();
  });

  it('keeps the publisher-IP ban when the broadcast ban fails, and retries that', async () => {
    // The other direction, which only exists because the IP ban goes first:
    // the address was captured while the publisher was demonstrably live, so
    // a failed ID ban does not cost the operator the IP ban as well.
    const h = banStub({
      ip: (n) => (n === 1 ? created('ban-ip') : duplicate('ban-ip')),
      id: (n) => (n === 1 ? unavailable() : created('ban-id')),
    });
    await submitKillAndBan(h.session);

    expect(await screen.findByText(/the ban store is unreachable/)).toBeTruthy();
    expect(h.ip()).toBe(1);

    fireEvent.click(screen.getByRole('button', { name: 'Kill and ban' }));

    await waitFor(() => expect(h.id()).toBe(2));
    expect(h.sent).toEqual(['ip', 'broadcastId', 'ip', 'broadcastId']);
    expect(
      await screen.findByText('Banned ABC123; its publisher IP was already banned.'),
    ).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Kill and ban' })).toBeNull();
  });

  it('reports both failures, and nothing banned, when neither write lands', async () => {
    const h = banStub({ ip: unavailable, id: unavailable });
    await submitKillAndBan(h.session);

    const alert = await screen.findByText(/Nothing was banned/);
    expect(alert.textContent).toMatch(/the ban store is unreachable/);
    expect(screen.getByRole('button', { name: 'Kill and ban' })).toBeTruthy();
  });
});
