import { describe, expect, it } from 'vitest';
import { appendClientIdentity, detectClientIdentity } from './client-identity';

const UA = {
  chromeWindows:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36',
  edgeWindows:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0',
  firefoxLinux: 'Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0',
  safariMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15',
  chromeMac:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36',
  safariIphone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1',
  chromeIphone:
    'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/150.0.0.0 Mobile/15E148 Safari/604.1',
  chromeAndroid:
    'Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Mobile Safari/537.36',
  chromeOS:
    'Mozilla/5.0 (X11; CrOS x86_64 16000.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36',
};

describe('detectClientIdentity', () => {
  it.each([
    ['chromeWindows', 'windows', 'chromium'],
    ['edgeWindows', 'windows', 'chromium'],
    ['firefoxLinux', 'linux', 'firefox'],
    ['safariMac', 'macos', 'safari'],
    ['chromeMac', 'macos', 'chromium'],
    ['safariIphone', 'ios', 'safari'],
    // Every iOS browser is WebKit: the engine is what the label names.
    ['chromeIphone', 'ios', 'safari'],
    ['chromeAndroid', 'android', 'chromium'],
    ['chromeOS', 'chromeos', 'chromium'],
  ] as const)('%s → %s/%s', (key, os, browser) => {
    expect(detectClientIdentity(UA[key])).toEqual({ os, browser });
  });

  it('leaves what it cannot recognise unset', () => {
    expect(detectClientIdentity('')).toEqual({ os: undefined, browser: undefined });
    expect(detectClientIdentity('curl/8.0')).toEqual({ os: undefined, browser: undefined });
  });
});

describe('appendClientIdentity', () => {
  it('adds app, os and browser, keeping existing params', () => {
    const url = new URL('https://relay.test:4433/publish?secret=s');
    appendClientIdentity(url, UA.firefoxLinux);
    expect(url.searchParams.get('secret')).toBe('s');
    expect(url.searchParams.get('app')).toBe('web');
    expect(url.searchParams.get('os')).toBe('linux');
    expect(url.searchParams.get('browser')).toBe('firefox');
  });

  it('omits what it could not detect rather than sending a guess', () => {
    const url = new URL('https://relay.test:4433/subscribe/K7XQ2M');
    appendClientIdentity(url, 'curl/8.0');
    expect(url.searchParams.get('app')).toBe('web');
    expect(url.searchParams.has('os')).toBe(false);
    expect(url.searchParams.has('browser')).toBe(false);
  });
});
