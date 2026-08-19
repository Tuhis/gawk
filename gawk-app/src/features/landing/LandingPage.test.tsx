// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { LandingPage } from './LandingPage';
import { SOURCE_URL } from '../../config';

afterEach(cleanup);

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
});
