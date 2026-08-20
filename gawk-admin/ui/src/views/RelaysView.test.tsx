// @vitest-environment jsdom
import { cleanup, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { RelaysView } from './RelaysView.tsx';
import { json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(cleanup);

const relays = [
  {
    pod: 'gawk-server-0',
    reachable: true,
    version: 'v0.41.0',
    config: { maxSubscribers: 1200, keepalivePeriod: '10s', moderationSource: 'k8s' },
  },
  { pod: 'gawk-server-1', reachable: false, error: 'dial tcp: i/o timeout' },
];

function mount() {
  const session = stubSession((path) => (path === 'api/v1/relays' ? json(relays) : json({})));
  renderWithSession(<RelaysView />, session);
  return session;
}

describe('the relay settings view (D10)', () => {
  it('renders each pod’s effective configuration', async () => {
    mount();
    expect(await screen.findByText('gawk-server-0')).toBeTruthy();
    expect(screen.getByText('maxSubscribers')).toBeTruthy();
    expect(screen.getByText('1200')).toBeTruthy();
    expect(screen.getByText('v0.41.0')).toBeTruthy();
  });

  it('degrades one unreachable pod without losing the rest', async () => {
    mount();
    await screen.findByText('gawk-server-0');
    expect(screen.getByText('unreachable')).toBeTruthy();
    expect(screen.getByText('dial tcp: i/o timeout')).toBeTruthy();
    expect(screen.getByText('reachable')).toBeTruthy();
  });

  it('offers no write path of any kind', async () => {
    // D10 is a decision, not an oversight: relay configuration belongs to the
    // chart, and a portal that could change it would put the running fleet out
    // of step with the manifest that describes it. The only control on this
    // page is Refresh.
    mount();
    await screen.findByText('gawk-server-0');
    const buttons = screen.getAllByRole('button').map((b) => b.textContent);
    expect(buttons).toEqual(['Refresh']);
    expect(screen.queryAllByRole('textbox')).toHaveLength(0);
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0);
    expect(screen.queryAllByRole('combobox')).toHaveLength(0);
  });
});
