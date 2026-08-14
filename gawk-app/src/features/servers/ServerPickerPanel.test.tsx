// @vitest-environment jsdom
// R37 (docs/40 SP6/SP7): the picker's probe + directory surfaces, driven
// through injected probe/fetch fns (jsdom has no WebTransport).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';

import { ServerPickerPanel } from './ServerPickerPanel';
import { useTransportStore } from '../../state/transportStore';
import type { ProbeFn } from './useServerProbe';

const okProbe = (rtt = 42): ProbeFn =>
  vi.fn(async () => ({
    state: 'ok' as const,
    rttMs: rtt,
    identity: { serverVersion: '9.9.9', name: 'Homelab ‮Evil' },
  }));

function directoryFetch(doc: unknown, ok = true): typeof fetch {
  return vi.fn(async () =>
    ({ ok, json: async () => doc }) as unknown as Response,
  ) as unknown as typeof fetch;
}

beforeEach(() => {
  localStorage.clear();
  window.__GAWK_CONFIG__ = {};
  const s = useTransportStore.getState();
  s.setSessionOverride(null);
  s.reloadFromStorage();
  s.selectServer('default');
});

afterEach(() => {
  cleanup();
  delete window.__GAWK_CONFIG__;
  localStorage.clear();
});

describe('ServerPickerPanel probe (SP6)', () => {
  it('probes saved servers on open and renders RTT + sanitized identity beside the host', async () => {
    const probeFn = okProbe();
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() => expect(screen.getByText(/42 ms/)).toBeTruthy());
    // F6: bidi controls stripped, host still rendered by the row itself.
    expect(screen.getByText(/Homelab Evil/)).toBeTruthy();
    expect(screen.getByText(/gawk-server 9\.9\.9/)).toBeTruthy();
    expect(probeFn).toHaveBeenCalledTimes(1); // the pinned default only
  });

  it('renders the combined failure state', async () => {
    const probeFn = vi.fn(async () => ({ state: 'failed' as const }));
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() => expect(screen.getByText('unreachable')).toBeTruthy());
  });

  it('an identity-less relay still shows its RTT (pre-R37 relay)', async () => {
    const probeFn = vi.fn(async () => ({ state: 'ok' as const, rttMs: 7, identity: null }));
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() => expect(screen.getByText(/7 ms/)).toBeTruthy());
    expect(screen.queryByText(/gawk-server/)).toBeNull();
  });
});

describe('ServerPickerPanel directory (SP7)', () => {
  const doc = {
    version: 1,
    servers: [
      { label: 'EU mirror', url: 'https://relay-eu.example.com:4433' },
      { label: 'Managed one', url: 'https://managed.example.com', managed: true },
      { label: 'bad', url: 'http://insecure.example' }, // dropped individually
      { url: 'https://unnamed.example.com:4433', secret: 'ignored' }, // label defaults, junk field ignored
    ],
  };

  it('fetches on open and renders valid offers only, on-demand probe only (F10)', async () => {
    window.__GAWK_CONFIG__ = { serverDirectoryUrl: '/servers.json' };
    const probeFn = okProbe();
    render(
      <ServerPickerPanel onClose={() => {}} probeFn={probeFn} fetchFn={directoryFetch(doc)} />,
    );
    await waitFor(() => expect(screen.getByText('EU mirror')).toBeTruthy());
    expect(screen.getByText(/Managed one · managed/)).toBeTruthy();
    expect(screen.getAllByText('unnamed.example.com:4433').length).toBeGreaterThan(0);
    expect(screen.queryByText(/insecure/)).toBeNull();
    // F10: opening the panel probed ONLY the saved default — no offer.
    expect(probeFn).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'Ping EU mirror' }));
    await waitFor(() => expect(probeFn).toHaveBeenCalledTimes(2));
    expect(probeFn).toHaveBeenLastCalledWith('https://relay-eu.example.com:4433', '');
  });

  it('adding an offer is explicit and equals a manually-added entry', async () => {
    window.__GAWK_CONFIG__ = { serverDirectoryUrl: '/servers.json' };
    render(
      <ServerPickerPanel onClose={() => {}} probeFn={okProbe()} fetchFn={directoryFetch(doc)} />,
    );
    await waitFor(() => expect(screen.getByText('EU mirror')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: 'Add EU mirror' }));
    const entry = useTransportStore
      .getState()
      .servers.find((e) => e.url === 'https://relay-eu.example.com:4433');
    expect(entry).toBeTruthy();
    expect(entry!.label).toBe('EU mirror');
    expect(entry!.publishSecret).toBe(''); // no credential fields exist in the schema
    // Adding never selects (D2's explicit-act rule).
    expect(useTransportStore.getState().selectedServerId).toBe('default');
  });

  it('degrades a failed or wrong-version fetch to "unavailable" without blocking the panel', async () => {
    window.__GAWK_CONFIG__ = { serverDirectoryUrl: '/servers.json' };
    render(
      <ServerPickerPanel
        onClose={() => {}}
        probeFn={okProbe()}
        fetchFn={directoryFetch({ version: 2, servers: [] })}
      />,
    );
    await waitFor(() => expect(screen.getByText('Directory unavailable.')).toBeTruthy());
    expect(screen.getByText('This deployment')).toBeTruthy(); // panel content intact
  });

  it('renders no directory section when unconfigured, and fetches nothing', () => {
    const fetchFn = vi.fn() as unknown as typeof fetch;
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} fetchFn={fetchFn} />);
    expect(screen.queryByText('Directory')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
  });
});
