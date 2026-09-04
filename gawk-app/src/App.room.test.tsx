// @vitest-environment jsdom
//
// R42 (docs/44 §4.8, RM6 acceptance): the `?rt=` grant hand-off happens in
// App's route resolution — BEFORE the room screen's first render — so the
// screen mounts with the grant already in session storage and a URL that no
// longer carries it. Asserted at the App level because that ordering is the
// whole point: a rewrite in an effect would run after the screen dialed.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';

const seen = vi.hoisted(() => ({ hashAtRender: [] as string[], grantAtRender: [] as (string | null)[] }));

vi.mock('./features/room/RoomScreen', () => ({
  RoomScreen: ({ code }: { code: string }) => {
    seen.hashAtRender.push(window.location.hash);
    seen.grantAtRender.push(sessionStorage.getItem(`gawk:room-grant:${code.toLowerCase()}`));
    return <div data-testid="room">{code}</div>;
  },
}));
vi.mock('./features/room/JoinResolver', () => ({
  JoinResolver: ({ code }: { code: string }) => <div data-testid="join">{code}</div>,
}));
vi.mock('./features/landing/LandingPage', () => ({ LandingPage: () => <div data-testid="landing" /> }));

import App from './App';

const TOKEN = 'a'.repeat(32);

beforeEach(() => {
  (globalThis as { WebTransport?: unknown }).WebTransport = class {};
  sessionStorage.clear();
  seen.hashAtRender.length = 0;
  seen.grantAtRender.length = 0;
  window.history.replaceState(null, '', '/');
});
afterEach(() => {
  cleanup();
  delete (globalThis as { WebTransport?: unknown }).WebTransport;
});

describe('App room routes (R42)', () => {
  it('moves ?rt= into session storage and rewrites the hash before the room screen renders', () => {
    window.history.replaceState(null, '', `/#/room/AB2CD3?rt=c:${TOKEN}&relay=https://relay.example.com:4433`);
    render(<App />);
    expect(screen.getByTestId('room').textContent).toBe('AB2CD3');
    // What the screen saw on its very first render.
    expect(seen.hashAtRender[0]).toBe('#/room/AB2CD3?relay=https%3A%2F%2Frelay.example.com%3A4433');
    expect(JSON.parse(seen.grantAtRender[0] ?? 'null')).toEqual({ kind: 'creator', tokenHex: TOKEN });
    expect(window.location.href).not.toContain('rt=');
  });

  it('renders the join resolver for a typed code', () => {
    window.history.replaceState(null, '', '/#/join/ab2cd3');
    render(<App />);
    expect(screen.getByTestId('join').textContent).toBe('AB2CD3');
  });
});
