// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { LandingPage } from './LandingPage';
import { SITE_DOWNLOAD_URL, SITE_URL, SOURCE_URL } from '../../config';

afterEach(cleanup);

// R42 (docs/44 D19): a typed code goes through #/join/, which asks the relay
// whether it names a room or a broadcast. The landing page itself never
// decides — and offers no "start a room" (rooms are made from a broadcast).
describe('LandingPage join box (R42)', () => {
  it('routes a completed code through #/join/ and offers no start-a-room action', () => {
    window.location.hash = '';
    render(<LandingPage />);
    fireEvent.change(screen.getByLabelText('Broadcast code'), { target: { value: 'ab2cd3' } });
    expect(window.location.hash).toBe('#/join/AB2CD3');
    expect(screen.queryByText(/start a room/i)).toBeNull();
  });
});

// The front-door footer. Both links are quiet by design, so the thing worth
// pinning down is that they exist and point somewhere real — and that the
// outbound one carries rel="noopener noreferrer", since it is the only link
// on this page that leaves the app.
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

  // R46 DL5 (docs/46 §6): the project site and, separately, its Download
  // section — the native apps are the one thing a user of this UI may need
  // that the UI itself cannot give them. Same quiet weight as the other two.
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
