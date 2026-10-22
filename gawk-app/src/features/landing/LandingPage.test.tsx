// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { LandingPage } from './LandingPage';
import { SITE_DOWNLOAD_URL, SITE_URL, SOURCE_URL } from '../../config';
import { DEFAULT_SERVER_ID, useTransportStore } from '../../state/transportStore';

afterEach(cleanup);

// A typed code goes through #/join/, which asks the relay whether it names a
// room or a broadcast. The landing page itself never decides, and offers no
// "start a room" (rooms are made from a broadcast).
describe('LandingPage join box (R42)', () => {
  it('routes a completed code through #/join/ and offers no start-a-room action', () => {
    window.location.hash = '';
    render(<LandingPage />);
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'ab2cd3' } });
    expect(window.location.hash).toBe('#/join/AB2CD3');
    expect(screen.queryByText(/start a room/i)).toBeNull();
  });
});

// The front-door footer. The links are quiet by design, so the thing worth
// pinning down is that they exist and point somewhere real, and that the ones
// leaving the app carry rel="noopener noreferrer".
describe('LandingPage footer', () => {
  it('links to the terms route', () => {
    render(<LandingPage />);
    const terms = screen.getByRole('link', { name: 'Terms of use' });
    expect(terms.getAttribute('href')).toBe('#/terms');
  });

  it('links to the source repository in a new tab', () => {
    render(<LandingPage />);
    const source = screen.getByRole('link', { name: 'GitHub' });
    expect(source.getAttribute('href')).toBe(SOURCE_URL);
    expect(SOURCE_URL).toMatch(/^https:\/\/github\.com\//);
    expect(source.getAttribute('target')).toBe('_blank');
    expect(source.getAttribute('rel')).toBe('noopener noreferrer');
  });

  // The project site and, separately, its Download section: the native apps
  // are the one thing a user of this UI may need that the UI itself cannot
  // give them.
  it('links to the project site and straight to the app downloads, in new tabs', () => {
    render(<LandingPage />);
    const about = screen.getByRole('link', { name: 'About' });
    expect(about.getAttribute('href')).toBe(SITE_URL);
    expect(SITE_URL).toMatch(/^https:\/\/[^/]+\/gawk\/$/);
    const download = screen.getByRole('link', { name: 'Get the app' });
    expect(download.getAttribute('href')).toBe(SITE_DOWNLOAD_URL);
    expect(SITE_DOWNLOAD_URL).toBe(`${SITE_URL}#download`);
    for (const a of [about, download]) {
      expect(a.getAttribute('target')).toBe('_blank');
      expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    }
  });

  it('orders the footer: about, download, terms, source', () => {
    render(<LandingPage />);
    const names = screen
      .getAllByRole('link')
      .filter((a) => a.closest('footer'))
      .map((a) => a.textContent?.trim());
    expect(names).toEqual(['About', 'Get the app', 'Terms of use', 'GitHub']);
  });
});

// R74 (docs/76 §6.1 item 5): a standalone PoP's join link lands here with
// `?relay=<lan-ip>&certHash=…`. The page offers to keep that server, and its
// own hops stay on it until then.
describe('LandingPage on a link-named server (R74)', () => {
  const H1 = '11'.repeat(32);
  const H2 = '22'.repeat(32);
  const POP = 'https://192.168.1.20:4433';
  const suffix = `?relay=${encodeURIComponent(POP)}&certHash=${H1},${H2}`;

  beforeEach(() => {
    localStorage.clear();
    const s = useTransportStore.getState();
    s.reloadFromStorage();
    s.selectServer(DEFAULT_SERVER_ID);
    s.setSessionOverride(null);
    s.setRelayLinkNote(null);
  });

  it('offers nothing extra on the user’s own server', () => {
    render(<LandingPage />);
    expect(screen.queryByRole('button', { name: 'Save and use this server' })).toBeNull();
  });

  it('“Save and use this server” saves the relay with its pins and selects it', () => {
    useTransportStore.getState().setSessionOverride(POP, [H1, H2]);
    render(<LandingPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Save and use this server' }));

    const s = useTransportStore.getState();
    const saved = s.servers.find((e) => e.url === POP);
    expect(saved).toBeDefined();
    expect(saved?.certHashes).toEqual([H1, H2]);
    expect(s.selectedServerId).toBe(saved?.id);
    // Still resolving the same relay, now as a saved entry, so the offer goes.
    expect(s.serverUrl).toBe(POP);
    expect(s.certHashes).toEqual([H1, H2]);
    expect(screen.queryByRole('button', { name: 'Save and use this server' })).toBeNull();

    // Selected for good: a later plain visit stays on it.
    s.setSessionOverride(null);
    expect(useTransportStore.getState().serverUrl).toBe(POP);
    expect(useTransportStore.getState().certHashes).toEqual([H1, H2]);
  });

  it('keeps the link’s server on the join and start-a-stream hops', () => {
    useTransportStore.getState().setSessionOverride(POP, [H1, H2]);
    render(<LandingPage />);
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'ab2cd3' } });
    expect(window.location.hash).toBe(`#/join/AB2CD3${suffix}`);
    fireEvent.click(screen.getByRole('button', { name: /Start a stream/ }));
    expect(window.location.hash).toBe(`#/broadcast${suffix}`);
  });

  it('shows the link note', () => {
    useTransportStore.getState().setRelayLinkNote('a quiet note');
    render(<LandingPage />);
    expect(screen.getByRole('status').textContent).toBe('a quiet note');
  });
});
