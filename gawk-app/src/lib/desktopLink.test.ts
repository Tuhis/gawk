// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyRouteDesktop,
  buildDesktopBroadcastLink,
  claimAutomaticHandoff,
  desktopHandoffOffered,
  isDesktopForHandoff,
  launchDesktopLink,
  loadHandoffAuto,
  saveHandoffAuto,
  type DesktopBroadcastLink,
} from './desktopLink';
import { parseRoute } from '../routing';

// R66's broadcast vectors (gawk-broadcast-desktop/crates/engine/tests/
// link-vectors.json), restated, never imported (docs/69 G5): each parsed
// link and the canonical `gawk://` form the desktop engine's `to_gawk`
// builds from it. Distinct pairs only; the file has more inputs per pair.
const VECTORS: Array<{ name: string; link: DesktopBroadcastLink; gawk: string }> = [
  { name: 'room and nickname', link: { room: 'lan-party', nick: 'Juho', relay: null }, gawk: 'gawk://broadcast?room=lan-party&nick=Juho' },
  { name: 'a bare broadcast link', link: { room: null, nick: null, relay: null }, gawk: 'gawk://broadcast' },
  {
    name: 'all three, canonical order',
    link: { room: 'abc', nick: 'a', relay: 'https://relay.friend.example' },
    gawk: 'gawk://broadcast?room=abc&nick=a&relay=https%3A%2F%2Frelay.friend.example',
  },
  { name: 'room keeps its case', link: { room: 'Lan-Party', nick: null, relay: null }, gawk: 'gawk://broadcast?room=Lan-Party' },
  { name: 'nickname alone', link: { room: null, nick: 'x', relay: null }, gawk: 'gawk://broadcast?nick=x' },
  { name: 'nickname with a space', link: { room: null, nick: 'Juho K', relay: null }, gawk: 'gawk://broadcast?nick=Juho%20K' },
  { name: 'non-ASCII nickname', link: { room: null, nick: 'Jürgen', relay: null }, gawk: 'gawk://broadcast?nick=J%C3%BCrgen' },
  {
    name: 'nickname at 31 bytes',
    link: { room: null, nick: 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx', relay: null },
    gawk: 'gawk://broadcast?nick=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx',
  },
  {
    name: 'reserved characters encoded like encodeURIComponent',
    link: { room: null, nick: "O'Neil (a&b=c)!+", relay: null },
    gawk: "gawk://broadcast?nick=O'Neil%20(a%26b%3Dc)!%2B",
  },
  { name: 'room alone', link: { room: 'abc', nick: null, relay: null }, gawk: 'gawk://broadcast?room=abc' },
  {
    name: 'relay with a port',
    link: { room: null, nick: null, relay: 'https://relay.friend.example:4433' },
    gawk: 'gawk://broadcast?relay=https%3A%2F%2Frelay.friend.example%3A4433',
  },
  {
    name: 'relay alone',
    link: { room: null, nick: null, relay: 'https://relay.friend.example' },
    gawk: 'gawk://broadcast?relay=https%3A%2F%2Frelay.friend.example',
  },
  {
    name: 'room and relay',
    link: { room: 'abc', nick: null, relay: 'https://relay.friend.example' },
    gawk: 'gawk://broadcast?room=abc&relay=https%3A%2F%2Frelay.friend.example',
  },
];

// Real user-agent strings.
const UA = {
  windowsChrome:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  windowsEdge:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0',
  macSafari:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  macFirefox: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 15.6; rv:143.0) Gecko/20100101 Firefox/143.0',
  linuxFirefox: 'Mozilla/5.0 (X11; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0',
  linuxChrome: 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
  iPhone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  android:
    'Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36',
  chromeOS:
    'Mozilla/5.0 (X11; CrOS x86_64 16328.55.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36',
};

describe('buildDesktopBroadcastLink (R67 D3, G5)', () => {
  it.each(VECTORS)('reproduces R66’s canonical link byte for byte: $name', ({ link, gawk }) => {
    expect(buildDesktopBroadcastLink(link)).toBe(gawk);
  });

  it('treats an empty value as absent, and has no parameter for a secret', () => {
    expect(buildDesktopBroadcastLink({ room: '', nick: '', relay: '' })).toBe('gawk://broadcast');
    // The only inputs are room, nick and relay: a grant has nowhere to go.
    const withExtra = { room: 'abc', nick: null, relay: null, rt: 'c:secret' } as DesktopBroadcastLink;
    expect(buildDesktopBroadcastLink(withExtra)).toBe('gawk://broadcast?room=abc');
  });
});

describe('isDesktopForHandoff (D2)', () => {
  it('offers on Windows, macOS and Linux', () => {
    for (const ua of [UA.windowsChrome, UA.windowsEdge, UA.macSafari, UA.macFirefox, UA.linuxFirefox, UA.linuxChrome]) {
      expect(isDesktopForHandoff(ua, 0)).toBe(true);
    }
  });

  it('never on phones, tablets or ChromeOS', () => {
    for (const ua of [UA.iPhone, UA.android, UA.chromeOS]) {
      expect(isDesktopForHandoff(ua, 0)).toBe(false);
    }
  });

  it('takes a touch Mac for an iPad: iPadOS Safari sends the desktop UA', () => {
    expect(isDesktopForHandoff(UA.macSafari, 5)).toBe(false);
    expect(isDesktopForHandoff(UA.macSafari, 0)).toBe(true);
    // A touch screen on Windows or Linux is still a desktop.
    expect(isDesktopForHandoff(UA.windowsChrome, 10)).toBe(true);
  });
});

describe('desktopHandoffOffered (D7)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    window.__GAWK_CONFIG__ = {};
  });

  it('follows the operator switch, default on', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(UA.windowsChrome);
    window.__GAWK_CONFIG__ = {};
    expect(desktopHandoffOffered()).toBe(true);
    window.__GAWK_CONFIG__ = { desktopHandoff: true };
    expect(desktopHandoffOffered()).toBe(true);
    window.__GAWK_CONFIG__ = { desktopHandoff: false };
    expect(desktopHandoffOffered()).toBe(false);
  });

  it('is off on a device the apps don’t exist for', () => {
    vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(UA.android);
    expect(desktopHandoffOffered()).toBe(false);
  });
});

describe('the remembered choice and the per-room flag (D5)', () => {
  beforeEach(() => {
    localStorage.clear();
    sessionStorage.clear();
  });
  afterEach(() => vi.restoreAllMocks());

  it('remembers "always" and forgets it', () => {
    expect(loadHandoffAuto()).toBe(false);
    saveHandoffAuto(true);
    expect(localStorage.getItem('gawk:desktop-handoff')).toBe('auto');
    expect(loadHandoffAuto()).toBe(true);
    saveHandoffAuto(false);
    expect(localStorage.getItem('gawk:desktop-handoff')).toBeNull();
    expect(loadHandoffAuto()).toBe(false);
  });

  it('allows one automatic launch per room in this tab, case-insensitively', () => {
    expect(claimAutomaticHandoff('Lan-Party')).toBe(true);
    expect(claimAutomaticHandoff('lan-party')).toBe(false);
    expect(claimAutomaticHandoff('other')).toBe(true);
  });

  it('never launches when session storage refuses', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(claimAutomaticHandoff('abc')).toBe(false);
  });
});

describe('launchDesktopLink', () => {
  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  it('fires the link from a hidden iframe and removes it later', () => {
    vi.useFakeTimers();
    launchDesktopLink('gawk://broadcast?room=abc');
    const frame = document.querySelector('iframe');
    expect(frame?.getAttribute('src')).toBe('gawk://broadcast?room=abc');
    expect(frame?.style.display).toBe('none');
    expect(frame?.getAttribute('aria-hidden')).toBe('true');
    vi.advanceTimersByTime(5000);
    expect(document.querySelector('iframe')).toBeNull();
  });
});

describe('applyRouteDesktop (D9)', () => {
  it('strips ?desktop= from a broadcast link and keeps the rest', () => {
    window.history.replaceState(null, '', '/#/broadcast?room=vip-abc&desktop=1');
    applyRouteDesktop(parseRoute(window.location.hash));
    expect(window.location.hash).toBe('#/broadcast?room=vip-abc');

    // Nothing to do without it, or on another route.
    for (const hash of ['#/broadcast?room=vip-abc', '#/room/vip-abc?desktop=1']) {
      window.history.replaceState(null, '', `/${hash}`);
      applyRouteDesktop(parseRoute(window.location.hash));
      expect(window.location.hash).toBe(hash);
    }
  });
});
