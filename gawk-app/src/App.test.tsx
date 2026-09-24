// @vitest-environment jsdom
//
// The app-level browser-support gate. The requirements it pins are that the
// warning reaches a *direct viewer link* and not just the landing page, that
// acknowledging it never outlives the page load — and, since the relay's
// WebKit refusal was fixed (docs/gotchas.md, the webtransport-go
// `Server.Config` entry), that a browser which has WebTransport is never
// warned by engine.
//
// The route screens are stubbed: this asserts where the gate sits, and the real
// screens would drag transports and capture into a jsdom run for no added
// coverage.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';

vi.mock('./features/landing/LandingPage', () => ({
  LandingPage: () => <div data-testid="landing" />,
}));
vi.mock('./features/viewer/ViewerScreen', async () => {
  const { useState } = await import('react');
  return {
    // Records the broadcast it was mounted for, so a test can tell a remount
    // from a prop change.
    ViewerScreen: ({ broadcastId }: { broadcastId: string }) => {
      const [mountedFor] = useState(broadcastId);
      return (
        <div data-testid="viewer" data-mounted-for={mountedFor}>
          {broadcastId}
        </div>
      );
    },
  };
});
vi.mock('./features/broadcaster/BroadcasterScreen', () => ({
  BroadcasterScreen: () => <div data-testid="broadcaster" />,
}));

import App from './App';

const SAFARI =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15';

function setUserAgent(ua: string) {
  Object.defineProperty(navigator, 'userAgent', { value: ua, configurable: true });
}

type G = { WebTransport?: unknown };
const withWebTransport = () => {
  (globalThis as G).WebTransport = class {};
};
const withoutWebTransport = () => {
  delete (globalThis as G).WebTransport;
};

beforeEach(() => {
  // jsdom has no WebTransport, so the default here is the *unsupported*
  // client; the supported cases opt in explicitly.
  withoutWebTransport();
  window.location.hash = '';
});

afterEach(() => {
  cleanup();
  withoutWebTransport();
});

const dialog = () => screen.queryByRole('dialog', { name: 'Unsupported browser' });

describe('App browser-support gate', () => {
  it('stays out of the way on a browser that has WebTransport', () => {
    withWebTransport();
    render(<App />);
    expect(dialog()).toBeNull();
    expect(screen.getByTestId('landing')).toBeTruthy();
  });

  // The regression pin: Safari was warned about by user agent while the relay
  // refused WebKit. With that fixed, a Safari that has the API is supported.
  it('does not warn Safari by engine when it has WebTransport', () => {
    withWebTransport();
    setUserAgent(SAFARI);
    render(<App />);
    expect(dialog()).toBeNull();
  });

  it('warns on the landing page without WebTransport', () => {
    render(<App />);
    expect(dialog()).toBeTruthy();
  });

  it('warns on a direct viewer link, with the viewer still mounted behind it', () => {
    window.location.hash = '#/view/ABC234';
    render(<App />);
    expect(dialog()).toBeTruthy();
    expect(screen.getByTestId('viewer').textContent).toBe('ABC234');
  });

  it('lets the user acknowledge and continue to the stream', () => {
    window.location.hash = '#/view/ABC234';
    render(<App />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(dialog()).toBeNull();
    expect(screen.getByTestId('viewer')).toBeTruthy();
  });

  it('does not re-warn on hash navigation within the same load', () => {
    render(<App />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    act(() => {
      window.location.hash = '#/view/ABC234';
      window.dispatchEvent(new HashChangeEvent('hashchange'));
    });
    expect(screen.getByTestId('viewer')).toBeTruthy();
    expect(dialog()).toBeNull();
  });

  // The acknowledgment must not be remembered: a fresh mount is a fresh load.
  it('warns again on the next page load, even after acknowledging', () => {
    const first = render(<App />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(dialog()).toBeNull();
    first.unmount();

    render(<App />);
    expect(dialog()).toBeTruthy();
  });
});

// The viewer holds per-broadcast state (audio controls, telemetry session,
// diagnostics history); moving to another broadcast must not inherit it.
describe('App viewer route', () => {
  it('mounts a fresh viewer for each broadcast', () => {
    withWebTransport();
    window.location.hash = '#/view/AAAAAA';
    render(<App />);
    expect(screen.getByTestId('viewer').dataset.mountedFor).toBe('AAAAAA');
    act(() => {
      window.location.hash = '#/view/BBBBBB';
      window.dispatchEvent(new HashChangeEvent('hashchange'));
    });
    expect(screen.getByTestId('viewer').textContent).toBe('BBBBBB');
    expect(screen.getByTestId('viewer').dataset.mountedFor).toBe('BBBBBB');
  });
});
