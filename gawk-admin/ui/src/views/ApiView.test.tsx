// @vitest-environment jsdom
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

// Redoc is ~1 MB of prebuilt JavaScript and spawns a worker. Mounting it in
// jsdom would test Redoc, not this view; what this view is responsible for is
// WHAT it hands the renderer.
const mounted: { specUrl?: string; options?: Record<string, unknown> }[] = [];
vi.mock('redoc', () => ({
  RedocStandalone: (props: { specUrl?: string; options?: Record<string, unknown> }) => {
    mounted.push(props);
    return <div data-testid="redoc" />;
  },
}));

import ApiView from './ApiView.tsx';
import { ASYNCAPI_URL, OPENAPI_URL } from './apiDocs.ts';
import { json, renderWithSession, stubSession } from '../testing/harness.tsx';

afterEach(() => {
  cleanup();
  mounted.length = 0;
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
    expect(mounted).toHaveLength(1);
    expect(mounted[0].specUrl).toBe(OPENAPI_URL);
    expect(screen.getByTestId('redoc')).toBeTruthy();
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

  // Redoc's StoreBuilder memoises `new AppStore(spec, specUrl, options)` on
  // those three BY REFERENCE, so a fresh options object on a parent re-render
  // re-normalises the document and resets the sidebar and every expanded
  // response. `Portal` re-renders on every hash change and session
  // transition, so this is not hypothetical.
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

  // The page reads the contract; it never acts on the caller's behalf. Redoc
  // executes no requests, so — unlike the Swagger UI it replaced — it is never
  // handed the access token. This is the assertion that keeps it that way.
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
