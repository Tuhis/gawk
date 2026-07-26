// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { TermsPage } from './TermsPage';

function mockFetch(impl: (url: string) => Promise<Response> | Response) {
  const fn = vi.fn((url: string) => Promise.resolve(impl(url)));
  vi.stubGlobal('fetch', fn as unknown as typeof fetch);
  return fn;
}

beforeEach(() => {
  delete (window as { __GAWK_CONFIG__?: unknown }).__GAWK_CONFIG__;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  delete (window as { __GAWK_CONFIG__?: unknown }).__GAWK_CONFIG__;
});

describe('TermsPage', () => {
  it('renders the bundled default and issues no fetch when termsUrl is unset', () => {
    const fetchFn = mockFetch(() => new Response('', { status: 200 }));
    render(<TermsPage />);
    expect(screen.getByRole('heading', { name: /terms of use/i })).toBeTruthy();
    expect(screen.getByText(/governed by the laws of Finland/i)).toBeTruthy();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  // R28 (docs/33 D8): always-on collection has to be stated, not merely
  // permitted by the broad rights Section 6 already reserves. The text must
  // name both what IS collected and what is NOT — silence about the second
  // half is what makes a privacy claim unverifiable.
  it('states the telemetry practice, including what is not collected', () => {
    render(<TermsPage />);
    expect(screen.getByText(/technical performance measurements/i)).toBeTruthy();
    expect(screen.getByText(/coarse browser and operating-system category/i)).toBeTruthy();
    const notCollected = screen.getByText(/does not include any broadcast audio or video/i);
    expect(notCollected.textContent).toMatch(/your IP address/i);
    expect(notCollected.textContent).toMatch(/full browser user-agent string/i);
    expect(notCollected.textContent).toMatch(/follows you between sessions/i);
    // And it must not undermine Section 7's existing no-media-recording claim.
    expect(screen.getByText(/does not persistently record or store broadcast media/i)).toBeTruthy();
  });

  it('renders a fetched override in place of the default', async () => {
    window.__GAWK_CONFIG__ = { termsUrl: '/terms.html' };
    mockFetch(() => new Response('<h1>Custom terms</h1><p>house rules</p>', { status: 200 }));
    render(<TermsPage />);
    await waitFor(() => expect(screen.getByText('house rules')).toBeTruthy());
    // The bundled text is gone once the override lands.
    expect(screen.queryByText(/governed by the laws of Finland/i)).toBeNull();
  });

  it('sanitizes the override end-to-end (no script executes)', async () => {
    window.__GAWK_CONFIG__ = { termsUrl: '/terms.html' };
    (window as { __pwned?: number }).__pwned = 0;
    mockFetch(
      () =>
        new Response(
          '<h1>Custom</h1><p>ok</p><script>window.__pwned=1</script><img src=x onerror="window.__pwned=1">',
          { status: 200 },
        ),
    );
    render(<TermsPage />);
    await waitFor(() => expect(screen.getByText('ok')).toBeTruthy());
    expect((window as { __pwned?: number }).__pwned).toBe(0);
    expect(document.querySelector('script')).toBeNull();
  });

  it('falls back to the bundled default on a 404', async () => {
    window.__GAWK_CONFIG__ = { termsUrl: '/terms.html' };
    mockFetch(() => new Response('not found', { status: 404 }));
    render(<TermsPage />);
    // The default is shown immediately and stays (fetch resolves 404).
    await waitFor(() => expect(screen.getByText(/governed by the laws of Finland/i)).toBeTruthy());
  });

  it('falls back to the bundled default when the fetch rejects', async () => {
    window.__GAWK_CONFIG__ = { termsUrl: '/terms.html' };
    mockFetch(() => Promise.reject(new Error('offline')));
    render(<TermsPage />);
    await waitFor(() => expect(screen.getByText(/governed by the laws of Finland/i)).toBeTruthy());
  });
});
