// @vitest-environment jsdom
import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { href, parseHash, useRoute } from './router.ts';

describe('portal routes (§4.9)', () => {
  it('lands on broadcasts for an empty hash', () => {
    expect(parseHash('').view).toBe('broadcasts');
    expect(parseHash('#/').view).toBe('broadcasts');
  });

  it('resolves the route every webhook payload points at', () => {
    // `portalUrl` in a webhook is `<external-url>/#/broadcasts?key=<key>`
    // (§4.10). It is followed from a phone, cold, before the operator is even
    // signed in — so this exact shape has to resolve, key included.
    const payloadUrl = 'https://admin.example.com/#/broadcasts?key=3f9a1c2b4d5e';
    const route = parseHash(payloadUrl.slice(payloadUrl.indexOf('#')));
    expect(route.view).toBe('broadcasts');
    expect(route.key).toBe('3f9a1c2b4d5e');
  });

  it('reads an empty key from a hash carrying none', () => {
    expect(parseHash('#/broadcasts').key).toBe('');
    expect(parseHash('#/bans?state=all').key).toBe('');
  });

  it('resolves every view', () => {
    for (const view of ['broadcasts', 'bans', 'events', 'relays', 'webhooks', 'rooms'] as const) {
      expect(parseHash(href(view)).view).toBe(view);
    }
  });

  it('resolves the route a room webhook points at, key included (R42)', () => {
    // `portalUrl` on a room.* event is `<external-url>/#/rooms?key=<roomKey>`
    // — the HMAC'd key, never the code (docs/44 D16).
    const route = parseHash('#/rooms?key=9c1d2e3f4a5b');
    expect(route.view).toBe('rooms');
    expect(route.key).toBe('9c1d2e3f4a5b');
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

describe('the live route (§4.9)', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/');
  });

  // The OIDC callback lands on `origin + pathname` with NO fragment — a
  // redirect URI cannot carry one — and `session.ts` puts the deep link back
  // with `history.replaceState` (§4.8). `replaceState` fires no `hashchange`,
  // so a route snapshotted once at module load stays the empty callback hash
  // and every deep link silently renders broadcasts.
  it('reads a hash installed by replaceState, which fires no hashchange', () => {
    window.history.replaceState(null, '', '#/relays');
    const { result } = renderHook(() => useRoute());
    expect(result.current.view).toBe('relays');
  });

  it('follows a hashchange', () => {
    window.history.replaceState(null, '', '#/broadcasts');
    const { result } = renderHook(() => useRoute());
    expect(result.current.view).toBe('broadcasts');
    act(() => {
      window.history.replaceState(null, '', '#/webhooks');
      window.dispatchEvent(new HashChangeEvent('hashchange'));
    });
    expect(result.current.view).toBe('webhooks');
  });
});
