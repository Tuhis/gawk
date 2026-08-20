// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { href, parseHash } from './router.ts';

describe('portal routes (§4.9)', () => {
  it('lands on broadcasts for an empty hash', () => {
    expect(parseHash('').view).toBe('broadcasts');
    expect(parseHash('#/').view).toBe('broadcasts');
  });

  it('resolves the route every webhook payload points at', () => {
    // `portalUrl` in a webhook is `<external-url>/#/broadcasts` (§4.10). It is
    // followed from a phone, cold, before the operator is even signed in — so
    // this exact shape has to resolve.
    const payloadUrl = 'https://admin.example.com/#/broadcasts';
    expect(parseHash(payloadUrl.slice(payloadUrl.indexOf('#'))).view).toBe('broadcasts');
  });

  it('resolves every view', () => {
    for (const view of ['broadcasts', 'bans', 'events', 'relays', 'webhooks'] as const) {
      expect(parseHash(href(view)).view).toBe(view);
    }
  });

  it('names an unknown route instead of silently landing somewhere', () => {
    const route = parseHash('#/nonsense');
    expect(route.view).toBe('not-found');
    expect(route.path).toBe('/nonsense');
  });

  it('ignores a query inside the hash', () => {
    expect(parseHash('#/bans?state=all').view).toBe('bans');
  });
});
