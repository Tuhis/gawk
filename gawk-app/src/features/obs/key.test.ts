// @vitest-environment jsdom
//
// R33 (docs/83 D2, OD3, §5.7): the stream key's format and its per-server
// storage.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildObsKey, forgetObsKey, loadObsKey, maskObsKey, parseObsKey, saveObsKey } from './key';

const TOKEN = '0123456789abcdef0123456789abcdef';
const KEY = `gawk1_K7Q2XM_${TOKEN}`;

beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe('the key format (D2)', () => {
  it('builds gawk1_<ID>_<32 lowercase hex>', () => {
    expect(buildObsKey('K7Q2XM', TOKEN)).toEqual({ key: KEY, broadcastId: 'K7Q2XM', tokenHex: TOKEN });
  });

  it('refuses a token that is not 16 bytes, or an ID outside the alphabet', () => {
    expect(buildObsKey('K7Q2XM', TOKEN.slice(2))).toBeNull();
    expect(buildObsKey('K7Q2X0', TOKEN)).toBeNull();
  });

  it('checks a pasted key by format only, dropping surrounding whitespace', () => {
    expect(parseObsKey(`  ${KEY}\n`)?.key).toBe(KEY);
    for (const bad of [
      '',
      'K7Q2XM',
      `gawk2_K7Q2XM_${TOKEN}`,
      `gawk1_K7Q2XM_${TOKEN.toUpperCase()}`,
      `gawk1_K7Q2XM_${TOKEN}0`,
      `gawk1_K7Q2XM-${TOKEN}`,
      `xgawk1_K7Q2XM_${TOKEN}`,
    ]) {
      expect(parseObsKey(bad)).toBeNull();
    }
  });

  it('uppercases the code, as the gateway normalizes it', () => {
    expect(parseObsKey(`gawk1_k7q2xm_${TOKEN}`)).toEqual({ key: KEY, broadcastId: 'K7Q2XM', tokenHex: TOKEN });
  });

  it('masks everything but the code', () => {
    const masked = maskObsKey(parseObsKey(KEY)!);
    expect(masked).toBe('gawk1_K7Q2XM_••••');
    expect(masked).not.toContain(TOKEN.slice(0, 4));
  });
});

describe('storage (OD3, §5.7)', () => {
  const k = parseObsKey(KEY)!;

  it('keeps one key per server, by normalized origin', () => {
    saveObsKey('https://Relay.Example.com:4433/', k);
    expect(loadObsKey('https://relay.example.com:4433')).toEqual(k);
    expect(loadObsKey('https://other.example.com')).toBeNull();
    expect(localStorage.getItem('gawk.obsKey:https://relay.example.com:4433')).toBe(KEY);
  });

  it('forgets the key for that server only', () => {
    saveObsKey('https://a.example', k);
    saveObsKey('https://b.example', k);
    forgetObsKey('https://a.example');
    expect(loadObsKey('https://a.example')).toBeNull();
    expect(loadObsKey('https://b.example')).toEqual(k);
  });

  it('ignores a stored value that is not a key', () => {
    localStorage.setItem('gawk.obsKey:https://a.example', 'junk');
    expect(loadObsKey('https://a.example')).toBeNull();
  });

  it('survives storage that refuses every call', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    expect(() => saveObsKey('https://a.example', k)).not.toThrow();
    expect(loadObsKey('https://a.example')).toBeNull();
    expect(() => forgetObsKey('https://a.example')).not.toThrow();
  });
});
