// @vitest-environment jsdom
import { describe, expect, it } from 'vitest';

import { href, isBroadcastKey, isSessionId, parseHash } from './router.ts';

// TH1's tests, and the first one is the whole reason the chunk exists.
//
// `readapi.Diagnose` sets `rep.DashboardURL = base + "#/session/" + sessionID`
// and that string is serialized into the `verdict` blob of every rollup row.
// **Rollups are permanent.** So the defect — an SPA with no router, on which
// every such link landed on the fleet page — was being written into the one
// artifact that is never pruned. This is a fix, not a feature, and the test
// takes a URL of exactly the shape the Go code emits.

describe('the dead permalink (TH1, UD6)', () => {
  it('resolves a DashboardURL of the exact shape a stored verdict carries', () => {
    // Byte-for-byte what readapi.Diagnose writes, given -dashboard-base.
    const stored = 'https://telemetry.internal/#/session/0a1b2c3d4e5f60718293a4b5';
    const hash = stored.slice(stored.indexOf('#'));

    const route = parseHash(hash);
    expect(route.view).toBe('session');
    expect(route.id).toBe('0a1b2c3d4e5f60718293a4b5');
    expect(isSessionId(route.id)).toBe(true);
  });

  it('resolves a broadcast-scope verdict’s link too', () => {
    const route = parseHash('#/broadcast/1a2b3c4d5e6f');
    expect(route.view).toBe('broadcast');
    expect(isBroadcastKey(route.id)).toBe(true);
  });

  it('routes a MALFORMED session id to the session view, not to the fleet page', () => {
    // The failure this guards is subtle: falling through to `live` would look
    // like a working router while reproducing the original defect exactly. The
    // view is what renders "no such session".
    const route = parseHash('#/session/not-a-real-id');
    expect(route.view).toBe('session');
    expect(route.id).toBe('not-a-real-id');
    expect(isSessionId(route.id)).toBe(false);
  });

  it('names an unknown path rather than silently landing somewhere', () => {
    const route = parseHash('#/nonsense/deep');
    expect(route.view).toBe('not-found');
  });
});

describe('URL state (TH1)', () => {
  it('parses view state out of the hash query', () => {
    const route = parseHash('#/history?range=7d&role=viewer&sort=severity');
    expect(route.view).toBe('history');
    expect(route.params.get('range')).toBe('7d');
    expect(route.params.get('role')).toBe('viewer');
    expect(route.params.get('sort')).toBe('severity');
  });

  it('round-trips a view, an id and its state', () => {
    const url = href('explore', undefined, { sessions: 'abc', fields: 'receivedFps', from: 123 });
    const back = parseHash(url);
    expect(back.view).toBe('explore');
    expect(back.params.get('sessions')).toBe('abc');
    expect(back.params.get('fields')).toBe('receivedFps');
    expect(back.params.get('from')).toBe('123');
  });

  it('omits absent and false state rather than encoding it', () => {
    // A URL full of `&asc=false&broadcast=` is a URL nobody can read, and the
    // absent/false distinction is what the whole page is built on.
    expect(href('history', undefined, { asc: false, broadcast: '', limit: undefined })).toBe(
      '#/history',
    );
  });

  it('treats the empty hash as the live fleet (UD13’s landing view)', () => {
    expect(parseHash('').view).toBe('live');
    expect(parseHash('#/').view).toBe('live');
    expect(href('live')).toBe('#/');
  });
});
