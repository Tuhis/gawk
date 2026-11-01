// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

// Redoc is ~2 MB of prebuilt JavaScript. Mounting it in jsdom would test
// Redoc, not this view; what this view is responsible for is WHAT it hands
// the renderer, and WHERE it mounts it. The stand-in records, per render, the
// props, whether it found a router above it, and the address bar it saw.
interface Mounted {
  specUrl?: string;
  basePath?: string;
  options?: Record<string, unknown>;
  inRouter: boolean;
  hash: string;
}
const mounted: Mounted[] = [];
vi.mock('redoc', async () => {
  const { useInRouterContext } = await import('react-router');
  return {
    RedocStandalone: (props: { specUrl?: string; basePath?: string; options?: Record<string, unknown> }) => {
      mounted.push({ ...props, inRouter: useInRouterContext(), hash: window.location.hash });
      return <div data-testid="redoc" />;
    },
  };
});

// The drawer loads the contract itself; here it is a stub so opening it in
// jsdom needs no network. `ApiConsole.test.tsx` covers the drawer.
vi.mock('./ApiConsole.tsx', () => ({
  ApiConsole: ({ onClose }: { onClose: () => void }) => (
    <aside aria-label="API console">
      <button type="button" onClick={onClose}>
        Close console
      </button>
    </aside>
  ),
}));

import ApiView from './ApiView.tsx';
import { ASYNCAPI_URL, DOCS_BASE_PATH, OPENAPI_URL } from './apiDocs.ts';
import { json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(() => {
  cleanup();
  mounted.length = 0;
  window.history.replaceState(null, '', '/');
});

function mount() {
  const session = stubSession(() => json({}));
  renderWithSession(<ApiView />, session);
  return session;
}

describe('the API page (R48, docs/49 D5)', () => {
  it('renders the contract this deployment serves', async () => {
    mount();
    expect(await screen.findByText('API')).toBeTruthy();
    expect(mounted.length).toBeGreaterThan(0);
    expect(mounted[0].specUrl).toBe(OPENAPI_URL);
    expect(screen.getByTestId('redoc')).toBeTruthy();
  });

  // Redoc 3 checks for a router above it and, finding none, mounts a
  // BrowserRouter that rewrites location.pathname — breaking a reload under
  // an Ingress sub-path and the OIDC redirect URI. It must find ours, and
  // route under the base the portal's router resolves to this page.
  it('mounts Redoc inside the view’s own router, under #/api', async () => {
    mount();
    await screen.findByText('API');
    expect(mounted[0].inRouter).toBe(true);
    expect(mounted[0].basePath).toBe(DOCS_BASE_PATH);
  });

  // A Redoc 2 bookmark is rewritten BEFORE Redoc reads the location, so it
  // opens on the operation it named rather than on Redoc's not-found.
  it('rewrites a Redoc 2 link before Redoc reads it', async () => {
    window.history.replaceState(null, '', '#tag/bans/operation/createBan');
    mount();
    await screen.findByText('API');
    expect(mounted[0].hash).toBe('#/api/bans/createban');
    expect(window.location.hash).toBe('#/api/bans/createban');
  });

  it('links to the raw document, relatively', async () => {
    mount();
    const link = (await screen.findByText('openapi.json')) as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe(OPENAPI_URL);
    expect(link.getAttribute('href')?.startsWith('/')).toBe(false);
  });

  // The event contract is a link, not a rendering (docs/52 §2, Rejected):
  // the AsyncAPI renderer is a large bundle for a page that already embeds
  // Redoc, and the catalogue reads fine raw.
  it('links to the event catalogue, relatively', async () => {
    mount();
    const link = (await screen.findByText('asyncapi.json')) as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe(ASYNCAPI_URL);
    expect(link.getAttribute('href')?.startsWith('/')).toBe(false);
  });

  // Redoc 3 reads its options once, when it loads the document, so a fresh
  // object on a parent re-render would be silently ignored rather than
  // applied. `Portal` re-renders on every hash change and session transition;
  // pinning the object keeps what Redoc was handed and what it uses the same.
  it('hands Redoc the same options object across re-renders', async () => {
    // Rendered WITHOUT the session provider, deliberately: this page needs no
    // session, and re-rendering the same element type is what exercises the
    // property — `renderWithSession`'s `rerender` would replace the tree shape
    // and remount, which proves nothing.
    const { rerender } = render(<ApiView />);
    await screen.findByText('API');

    rerender(<ApiView />);
    rerender(<ApiView />);
    await screen.findByText('API');

    // `mounted` records every RENDER of the renderer, so more than one here is
    // the point: the parent re-rendered, and the options object survived it by
    // identity. A fresh object on each render is what rebuilds Redoc's store.
    expect(mounted.length).toBeGreaterThan(1);
    for (const render of mounted) {
      expect(render.options).toBe(mounted[0].options);
      expect(render.specUrl).toBe(mounted[0].specUrl);
    }
  });

  // The drawer is closed on every visit and opens from a floating toggle
  // (fixed, so it is there however far down the reference the reader is);
  // the wrapper is marked `withConsole`, which hides Redoc's samples column
  // where the drawer sits over it. While the drawer is open the toggle is
  // gone — the drawer's own close is the way back — and closing returns both
  // the page and the toggle.
  it('opens and closes the Console drawer from the floating toggle', async () => {
    mount();
    await screen.findByText('API');
    expect(screen.queryByLabelText('API console')).toBeNull();
    expect(document.querySelector('.gawk-redoc.withConsole')).toBeNull();
    const toggle = screen.getByRole('button', { name: 'Console' });
    expect(toggle.className).toContain('gawk-console-toggle');

    fireEvent.click(toggle);
    expect(screen.getByLabelText('API console')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Console' })).toBeNull();
    expect(document.querySelector('.gawk-redoc.withConsole')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Close console' }));
    expect(screen.queryByLabelText('API console')).toBeNull();
    expect(screen.getByRole('button', { name: 'Console' })).toBeTruthy();
    expect(document.querySelector('.gawk-redoc.withConsole')).toBeNull();
  });

  // Redoc reads the contract; it never acts on the caller's behalf. It
  // executes no requests, so — unlike the Swagger UI it replaced — it is never
  // handed the access token. The Console sends, and does so through the
  // session (its own test asserts that); this assertion keeps the RENDERER
  // token-free, whichever the drawer is doing.
  it('never hands the renderer the session token', async () => {
    const session = mount();
    await screen.findByText('API');

    const props = JSON.stringify(mounted[0].options ?? {});
    expect(props).not.toContain(session.accessToken());
    expect(props.toLowerCase()).not.toContain('authorization');
    // The view does not even read the token: nothing asked the session for one.
    expect(session.calls).toHaveLength(0);
  });
});
