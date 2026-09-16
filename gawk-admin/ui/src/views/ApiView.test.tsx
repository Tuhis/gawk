// @vitest-environment jsdom
import { cleanup, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

// The real bundle is 1.6 MB of prebuilt JavaScript. Mounting it in jsdom would
// test Swagger UI, not this view; what this view is responsible for is WHAT it
// hands the bundle, and that it hands it exactly once.
const mounted: Record<string, unknown>[] = [];
vi.mock('swagger-ui-dist/swagger-ui-es-bundle.js', () => ({
  default: (options: Record<string, unknown>) => {
    mounted.push(options);
    (options.domNode as HTMLElement).append(document.createTextNode('swagger-ui'));
    return { options };
  },
}));

import ApiView from './ApiView.tsx';
import { OPENAPI_URL } from './apiDocs.ts';
import type { SwaggerRequest } from './apiDocs.ts';
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
  it('renders Swagger UI against this deployment’s own document', async () => {
    mount();
    expect(await screen.findByText('API')).toBeTruthy();
    expect(mounted).toHaveLength(1);
    expect(mounted[0].url).toBe(OPENAPI_URL);
    expect(mounted[0].domNode).toBe(screen.getByTestId('swagger-ui'));
  });

  // The page's whole reason to exist: try a call with your own token before
  // writing any code against the contract.
  it('puts the caller’s bearer token on a "Try it out" request', async () => {
    mount();
    await screen.findByText('API');
    const intercept = mounted[0].requestInterceptor as (r: SwaggerRequest) => SwaggerRequest;
    const out = intercept({ url: '/api/v1/me' });
    expect(out.headers?.Authorization).toBe('Bearer test-access-token');
  });

  it('links to the raw document, relatively', async () => {
    mount();
    const link = (await screen.findByText('openapi.json')) as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe(OPENAPI_URL);
  });

  it('leaves nothing behind when the operator navigates away', async () => {
    const { unmount } = (() => {
      const session = stubSession(() => json({}));
      return renderWithSession(<ApiView />, session);
    })();
    const host = await screen.findByTestId('swagger-ui');
    expect(host.childNodes.length).toBeGreaterThan(0);
    unmount();
    // A second visit must not stack a second copy of the whole UI under the
    // first — which is what an un-emptied host node would do.
    expect(host.childNodes.length).toBe(0);
  });
});
