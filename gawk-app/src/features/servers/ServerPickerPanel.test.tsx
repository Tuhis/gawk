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

// Regression (R37 follow-up): the panel is a modal overlay, so it must escape
// its mount point's layout and be dismissible from outside.
describe('ServerPickerPanel overlay', () => {
  it('renders through a portal, not inside its mount point', () => {
    // The landing chip sits in a `position: absolute; transform: translateX(-50%)`
    // row; a transformed ancestor becomes the containing block for
    // `position: fixed` descendants, which collapsed the full-screen overlay to
    // the chip's ~78px box. Portalling to <body> is what keeps it full-screen.
    const { container } = render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(document.body.querySelector('[role="dialog"]')).toBeTruthy();
  });

  it('portals into the fullscreen element while one is active', () => {
    // The viewer goes fullscreen on its own root (lib/useFullscreen.ts), and
    // the Fullscreen API paints only that subtree — a <body> child would be
    // invisible over a fullscreen stream.
    const host = document.createElement('div');
    document.body.appendChild(host);
    // jsdom implements no fullscreen API at all, so define the one property
    // the component reads rather than spying on a getter that isn't there.
    Object.defineProperty(document, 'fullscreenElement', {
      configurable: true,
      get: () => host,
    });
    try {
      render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
      expect(host.querySelector('[role="dialog"]')).toBeTruthy();
    } finally {
      delete (document as { fullscreenElement?: unknown }).fullscreenElement;
      host.remove();
    }
  });

  // onClose is awaited rather than asserted synchronously: dismissal now runs
  // an exit animation and reports back when it has finished.
  it('closes when the backdrop is clicked but not when the panel is', async () => {
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);

    fireEvent.click(screen.getByRole('dialog'));
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('server-picker-backdrop'));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('closes on Escape', async () => {
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });
});

// Regression (R37 follow-up): the panel resolved a row's cert hash straight
// from the stored entry, bypassing R38's config fallback that every real
// connection goes through — so a local stack's own relay probed as
// "unreachable" while the viewer was streaming from it.
describe('ServerPickerPanel probe credentials', () => {
  it('probes the pinned default with the deployment cert hash when no entry stores one', async () => {
    window.__GAWK_CONFIG__ = {
      relayUrl: 'https://relay.test:4433',
      devCertHashHex: 'aabbcc',
    };
    const probeFn = okProbe();
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() =>
      expect(probeFn).toHaveBeenCalledWith('https://relay.test:4433', 'aabbcc'),
    );
  });

  it('does not hand the deployment hash to a server that is not the default', async () => {
    window.__GAWK_CONFIG__ = {
      relayUrl: 'https://relay.test:4433',
      devCertHashHex: 'aabbcc',
    };
    useTransportStore.getState().addServer({ label: 'Elsewhere', url: 'https://other.test:4433' });
    const probeFn = okProbe();
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() => expect(probeFn).toHaveBeenCalledWith('https://other.test:4433', ''));
  });

  it('prefers a hash the user typed over the configured one', async () => {
    window.__GAWK_CONFIG__ = {
      relayUrl: 'https://relay.test:4433',
      devCertHashHex: 'aabbcc',
    };
    useTransportStore.getState().updateServer('default', { certHashHex: 'ddeeff' });
    const probeFn = okProbe();
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    await waitFor(() =>
      expect(probeFn).toHaveBeenCalledWith('https://relay.test:4433', 'ddeeff'),
    );
  });
});

describe('ServerPickerPanel add/edit form', () => {
  const openAddForm = () => {
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Add a server' }));
  };

  it('keeps the credential fields behind a collapsed Advanced section', () => {
    openAddForm();
    // The identity fields are the whole form at first glance.
    expect(screen.getByLabelText(/Name/)).toBeTruthy();
    expect(screen.getByLabelText(/Server URL/)).toBeTruthy();
    expect(screen.queryByLabelText(/Publish secret/)).toBeNull();
    expect(screen.queryByLabelText(/cert hash/i)).toBeNull();

    const toggle = screen.getByRole('button', { name: /Advanced/ });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(toggle);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByLabelText(/Publish secret/)).toBeTruthy();
    expect(screen.getByLabelText(/cert hash/i)).toBeTruthy();
  });

  it('saves a credential typed under Advanced', () => {
    openAddForm();
    fireEvent.change(screen.getByLabelText(/Server URL/), {
      target: { value: 'https://new.test:4433' },
    });
    fireEvent.click(screen.getByRole('button', { name: /Advanced/ }));
    fireEvent.change(screen.getByLabelText(/Publish secret/), { target: { value: 'hunter2' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    const saved = useTransportStore.getState().servers.find((s) => s.url === 'https://new.test:4433');
    expect(saved?.publishSecret).toBe('hunter2');
  });

  it('starts Advanced open when the entry being edited already has a credential', () => {
    useTransportStore
      .getState()
      .addServer({ label: 'Homelab', url: 'https://home.test:4433', publishSecret: 'kept' });
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    fireEvent.click(screen.getByLabelText('Edit Homelab'));

    expect(screen.getByRole('button', { name: /Advanced/ }).getAttribute('aria-expanded')).toBe(
      'true',
    );
    expect((screen.getByLabelText(/Publish secret/) as HTMLInputElement).value).toBe('kept');
  });

  it('shows the default server credential form without an Advanced gate', () => {
    // That form is nothing but those two fields — collapsing them would leave
    // an empty dialog behind a disclosure.
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    fireEvent.click(screen.getByLabelText('Edit default server credentials'));

    expect(screen.getByLabelText(/Publish secret/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Advanced/ })).toBeNull();
  });
});

describe('ServerPickerPanel probe quality dot', () => {
  const dotFor = async (probeFn: ProbeFn) => {
    render(<ServerPickerPanel onClose={() => {}} probeFn={probeFn} />);
    return await waitFor(() => {
      const dot = document.querySelector('[data-testid="probe-dot"]');
      expect(dot).toBeTruthy();
      return dot!;
    });
  };

  it('reads green under 100 ms', async () => {
    expect((await dotFor(okProbe(42))).getAttribute('data-quality')).toBe('good');
  });

  it('reads yellow from 100 to 250 ms', async () => {
    expect((await dotFor(okProbe(100))).getAttribute('data-quality')).toBe('fair');
    cleanup();
    expect((await dotFor(okProbe(249))).getAttribute('data-quality')).toBe('fair');
  });

  it('reads red from 250 ms up', async () => {
    expect((await dotFor(okProbe(250))).getAttribute('data-quality')).toBe('poor');
  });

  it('reads red when the probe fails', async () => {
    const failing: ProbeFn = vi.fn(async () => ({ state: 'failed' as const }));
    expect((await dotFor(failing)).getAttribute('data-quality')).toBe('poor');
  });

  it('is decorative — the millisecond reading stays the accessible value', async () => {
    const dot = await dotFor(okProbe(42));
    expect(dot.getAttribute('aria-hidden')).toBe('true');
    expect(screen.getByText(/42 ms/)).toBeTruthy();
  });
});

describe('ServerPickerPanel dismissal UX', () => {
  it('closes after selecting a server', async () => {
    useTransportStore.getState().addServer({ label: 'Homelab', url: 'https://home.test:4433' });
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);

    fireEvent.click(screen.getByRole('option', { name: /Homelab/ }));
    expect(useTransportStore.getState().selectedServerId).not.toBe('default');
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('closes after selecting the pinned default too', async () => {
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);
    fireEvent.click(screen.getByRole('option', { name: /This deployment/ }));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('plays the exit animation before reporting closed', async () => {
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);

    fireEvent.click(screen.getByTestId('server-picker-backdrop'));
    // Still mounted, and marked closing, so the animation has something to
    // run on — an immediate unmount would just make it vanish.
    expect(screen.getByTestId('server-picker-backdrop').getAttribute('data-closing')).toBe('true');
    expect(onClose).not.toHaveBeenCalled();
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
  });

  it('does not fire onClose twice when dismissed twice', async () => {
    const onClose = vi.fn();
    render(<ServerPickerPanel onClose={onClose} probeFn={okProbe()} />);
    fireEvent.click(screen.getByTestId('server-picker-backdrop'));
    fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

describe('ServerPickerPanel footer', () => {
  it('puts Add a server bottom-left and Done bottom-right', () => {
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    const foot = screen.getByTestId('server-picker-foot');
    const buttons = [...foot.querySelectorAll('button')];
    expect(buttons.map((b) => b.textContent?.trim())).toEqual(['Add a server', 'Done']);
    // The add button carries a glyph saying what it does.
    expect(buttons[0].querySelector('svg')).toBeTruthy();
    // The title bar no longer carries the dismiss control.
    expect(screen.getByTestId('server-picker-head').querySelector('button')).toBeNull();
  });

  it('keeps Done reachable while the add form is open, without a second Add', () => {
    render(<ServerPickerPanel onClose={() => {}} probeFn={okProbe()} />);
    fireEvent.click(screen.getByRole('button', { name: /Add a server/ }));

    const foot = screen.getByTestId('server-picker-foot');
    const buttons = [...foot.querySelectorAll('button')].map((b) => b.textContent?.trim());
    expect(buttons).toEqual(['Done']);
  });
});
