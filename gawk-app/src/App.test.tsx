// @vitest-environment jsdom
//
// The app-level browser-support gate. The requirements it pins (BUGS.md: WebKit
// cannot join since the quic-go bump) are that the warning reaches a *direct
// viewer link* and not just the landing page, and that acknowledging it never
// outlives the page load.
//
// The route screens are stubbed: this asserts where the gate sits, and the real
// screens would drag transports and capture into a jsdom run for no added
// coverage.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';

vi.mock('./features/landing/LandingPage', () => ({
  LandingPage: () => <div data-testid="landing" />,
}));
vi.mock('./features/viewer/ViewerScreen', () => ({
  ViewerScreen: ({ broadcastId }: { broadcastId: string }) => (
    <div data-testid="viewer">{broadcastId}</div>
  ),
}));
vi.mock('./features/broadcaster/BroadcasterScreen', () => ({
  BroadcasterScreen: () => <div data-testid="broadcaster" />,
}));

import App from './App';

const SAFARI =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15';
const CHROME =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36';

function setUserAgent(ua: string) {
  Object.defineProperty(navigator, 'userAgent', { value: ua, configurable: true });
}

beforeEach(() => {
  // jsdom has no WebTransport; without this every client would trip the
  // no-webtransport branch and the WebKit assertions would pass vacuously.
  (globalThis as { WebTransport?: unknown }).WebTransport = class {};
  window.location.hash = '';
});

afterEach(() => {
  cleanup();
  delete (globalThis as { WebTransport?: unknown }).WebTransport;
});

const dialog = () => screen.queryByRole('dialog', { name: 'Unsupported browser' });

describe('App browser-support gate', () => {
  it('stays out of the way on a supported browser', () => {
    setUserAgent(CHROME);
    render(<App />);
    expect(dialog()).toBeNull();
    expect(screen.getByTestId('landing')).toBeTruthy();
  });

  it('warns on the landing page in Safari', () => {
    setUserAgent(SAFARI);
    render(<App />);
    expect(dialog()).toBeTruthy();
  });

  it('warns on a direct viewer link, with the viewer still mounted behind it', () => {
    setUserAgent(SAFARI);
    window.location.hash = '#/view/ABC234';
    render(<App />);
    expect(dialog()).toBeTruthy();
    expect(screen.getByTestId('viewer').textContent).toBe('ABC234');
  });

  it('lets the user acknowledge and continue to the stream', () => {
    setUserAgent(SAFARI);
    window.location.hash = '#/view/ABC234';
    render(<App />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(dialog()).toBeNull();
    expect(screen.getByTestId('viewer')).toBeTruthy();
  });

  it('does not re-warn on hash navigation within the same load', () => {
    setUserAgent(SAFARI);
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
    setUserAgent(SAFARI);
    const first = render(<App />);
    fireEvent.click(screen.getByRole('button', { name: /continue/i }));
    expect(dialog()).toBeNull();
    first.unmount();

    render(<App />);
    expect(dialog()).toBeTruthy();
  });
});
