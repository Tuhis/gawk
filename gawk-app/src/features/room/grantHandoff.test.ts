// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { applyRouteGrant, clearGrant, formatGrant, parseGrant, readGrant, stashGrant } from './grantHandoff';
import { parseRoute } from '../../routing';

const TOKEN = 'a'.repeat(32);

beforeEach(() => {
  sessionStorage.clear();
  window.history.replaceState(null, '', '/');
});
afterEach(() => sessionStorage.clear());

describe('parseGrant', () => {
  it('reads a creator token bare or prefixed, lower-casing it', () => {
    expect(parseGrant(TOKEN.toUpperCase())).toEqual({ kind: 'creator', tokenHex: TOKEN });
    expect(parseGrant(`c:${TOKEN}`)).toEqual({ kind: 'creator', tokenHex: TOKEN });
  });

  it('reads an attach secret', () => {
    expect(parseGrant('a:hunter2')).toEqual({ kind: 'attach', secret: 'hunter2' });
    expect(parseGrant('a:')).toBeNull();
  });

  it('refuses junk', () => {
    expect(parseGrant('')).toBeNull();
    expect(parseGrant('not-hex')).toBeNull();
    expect(parseGrant('c:1234')).toBeNull();
  });

  it('round-trips through formatGrant', () => {
    for (const g of [parseGrant(`c:${TOKEN}`)!, parseGrant('a:s3cret')!]) {
      expect(parseGrant(formatGrant(g))).toEqual(g);
    }
  });
});

describe('the stash', () => {
  it('is keyed by the normalized code and survives a case change', () => {
    stashGrant('TuhisRoom', { kind: 'attach', secret: 's' });
    expect(readGrant('tuhisroom')).toEqual({ kind: 'attach', secret: 's' });
    clearGrant('TUHISROOM');
    expect(readGrant('TuhisRoom')).toBeNull();
  });

  it('drops a corrupt entry rather than throwing', () => {
    sessionStorage.setItem('gawk:room-grant:ab2cd3', '{not json');
    expect(readGrant('AB2CD3')).toBeNull();
    sessionStorage.setItem('gawk:room-grant:ab2cd3', JSON.stringify({ kind: 'creator', tokenHex: 'zz' }));
    expect(readGrant('AB2CD3')).toBeNull();
  });
});

describe('applyRouteGrant (the rewrite before first render)', () => {
  it('moves rt into session storage and strips it from the hash, keeping ?relay=', () => {
    window.location.hash = `#/room/AB2CD3?rt=c:${TOKEN}&relay=https%3A%2F%2Frelay.example.com%3A4433`;
    const route = parseRoute(window.location.hash);
    applyRouteGrant(route);
    expect(readGrant('AB2CD3')).toEqual({ kind: 'creator', tokenHex: TOKEN });
    expect(window.location.hash).toBe('#/room/AB2CD3?relay=https%3A%2F%2Frelay.example.com%3A4433');
  });

  it('cleans a malformed grant off the URL without stashing anything', () => {
    window.location.hash = '#/room/AB2CD3?rt=junk';
    applyRouteGrant(parseRoute(window.location.hash));
    expect(readGrant('AB2CD3')).toBeNull();
    expect(window.location.hash).toBe('#/room/AB2CD3');
  });

  it('does nothing on routes without a grant', () => {
    window.location.hash = '#/room/AB2CD3';
    applyRouteGrant(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/room/AB2CD3');
    window.location.hash = '#/view/AB2CD3';
    applyRouteGrant(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/view/AB2CD3');
  });
});
