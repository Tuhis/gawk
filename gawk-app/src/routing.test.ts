import { describe, expect, it } from 'vitest';
import { parseRoute } from './routing';

describe('parseRoute', () => {
  it('maps the root variants to landing', () => {
    for (const h of ['', '#', '#/', '#//']) {
      expect(parseRoute(h)).toEqual({ view: 'landing' });
    }
  });

  it('maps #/broadcast to the production broadcaster', () => {
    expect(parseRoute('#/broadcast')).toEqual({ view: 'broadcaster' });
  });

  it('maps #/terms to the terms surface (R23)', () => {
    expect(parseRoute('#/terms')).toEqual({ view: 'terms' });
    // Trailing-slash normalization, same as the other routes.
    expect(parseRoute('#/terms/')).toEqual({ view: 'terms' });
  });

  it('maps #/view/<valid-id> to the viewer, uppercasing the id', () => {
    expect(parseRoute('#/view/AB2CD3')).toEqual({ view: 'viewer', broadcastId: 'AB2CD3' });
    expect(parseRoute('#/view/ab2cd3')).toEqual({ view: 'viewer', broadcastId: 'AB2CD3' });
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
