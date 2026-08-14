import { describe, expect, it } from 'vitest';
import { parseRoute } from './routing';

const noQuery = { relay: null, droppedParams: [] };

describe('parseRoute', () => {
  it('maps the root variants to landing', () => {
    for (const h of ['', '#', '#/', '#//']) {
      expect(parseRoute(h)).toEqual({ view: 'landing' });
    }
  });

  it('maps #/broadcast to the production broadcaster', () => {
    expect(parseRoute('#/broadcast')).toEqual({ view: 'broadcaster', ...noQuery });
  });

  it('maps #/terms to the terms surface (R23)', () => {
    expect(parseRoute('#/terms')).toEqual({ view: 'terms' });
    // Trailing-slash normalization, same as the other routes.
    expect(parseRoute('#/terms/')).toEqual({ view: 'terms' });
  });

  it('maps #/view/<valid-id> to the viewer, uppercasing the id', () => {
    expect(parseRoute('#/view/AB2CD3')).toEqual({ view: 'viewer', broadcastId: 'AB2CD3', ...noQuery });
    expect(parseRoute('#/view/ab2cd3')).toEqual({ view: 'viewer', broadcastId: 'AB2CD3', ...noQuery });
  });

  it('redirects #/view with no id to home', () => {
    expect(parseRoute('#/view')).toEqual({ view: 'redirect', to: '#/' });
    expect(parseRoute('#/view/')).toEqual({ view: 'redirect', to: '#/' });
  });

  it('redirects an invalid id (wrong length or bad chars) to home', () => {
    expect(parseRoute('#/view/ABC')).toEqual({ view: 'redirect', to: '#/' }); // too short
    expect(parseRoute('#/view/ABCDEFG')).toEqual({ view: 'redirect', to: '#/' }); // too long
    expect(parseRoute('#/view/AB0CD1')).toEqual({ view: 'redirect', to: '#/' }); // 0 and 1 excluded
  });

  it('maps the debug tree', () => {
    expect(parseRoute('#/debug')).toEqual({ view: 'debug-index' });
    expect(parseRoute('#/debug/broadcast')).toEqual({ view: 'debug-broadcast' });
    expect(parseRoute('#/debug/view')).toEqual({ view: 'debug-view' });
    expect(parseRoute('#/debug/loopback')).toEqual({ view: 'debug-loopback' });
  });

  it('keeps the debug viewer on its own route when it appends an id', () => {
    // ViewPage syncs #/debug/view/<id>; it must stay in the debug viewer, not
    // fall through to the production viewer or a redirect.
    expect(parseRoute('#/debug/view/AB2CD3')).toEqual({ view: 'debug-view' });
  });

  it('redirects unknown routes to home', () => {
    expect(parseRoute('#/nope')).toEqual({ view: 'redirect', to: '#/' });
    expect(parseRoute('#/debug/nope')).toEqual({ view: 'redirect', to: '#/' });
  });
});

// R37 (docs/40 §4.2): the ?relay= grammar on both production routes. The
// query is split off before path matching, so a query never changes which
// route matches — only what rides along with it.
describe('parseRoute ?relay=', () => {
  it('carries a valid relay on the viewer route, normalized', () => {
    expect(parseRoute('#/view/AB2CD3?relay=https%3A%2F%2FRelay.Example.com%3A4433%2F')).toEqual({
      view: 'viewer',
      broadcastId: 'AB2CD3',
      relay: 'https://relay.example.com:4433',
      droppedParams: [],
    });
  });

  it('carries a valid relay on the broadcast route', () => {
    expect(parseRoute('#/broadcast?relay=https://relay.example.com:4433')).toEqual({
      view: 'broadcaster',
      relay: 'https://relay.example.com:4433',
      droppedParams: [],
    });
  });

  it('drops an invalid relay value into the quiet note, never fatal (D7)', () => {
    for (const bad of [
      'http://insecure.example',
      'not a url',
      'https://user:pw@relay.example.com',
      'https://relay.example.com/path',
    ]) {
      expect(parseRoute(`#/view/AB2CD3?relay=${encodeURIComponent(bad)}`)).toEqual({
        view: 'viewer',
        broadcastId: 'AB2CD3',
        relay: null,
        droppedParams: ['relay'],
      });
    }
  });

  it('ignores unknown parameters silently (left for the R26 grammar)', () => {
    expect(parseRoute('#/broadcast?start=1&res=720')).toEqual({
      view: 'broadcaster',
      ...noQuery,
    });
  });

  it('a query on a non-producing route changes nothing', () => {
    expect(parseRoute('#/terms?relay=https://x.example')).toEqual({ view: 'terms' });
    expect(parseRoute('#/?relay=https://x.example')).toEqual({ view: 'landing' });
  });

  it('an invalid id still redirects, query or not', () => {
    expect(parseRoute('#/view/ABC?relay=https://relay.example.com')).toEqual({
      view: 'redirect',
      to: '#/',
    });
  });
});
