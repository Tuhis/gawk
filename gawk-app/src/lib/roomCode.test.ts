import { describe, expect, it } from 'vitest';
import { isValidRoomCode, isValidRoomSlug, parseRoomLink } from './roomCode';

describe('room codes', () => {
  it('accepts a static slug of 3–32 [A-Za-z0-9-]', () => {
    expect(isValidRoomSlug('TuhisRoom')).toBe(true);
    expect(isValidRoomSlug('a-b')).toBe(true);
    expect(isValidRoomSlug('ab')).toBe(false);
    expect(isValidRoomSlug('x'.repeat(33))).toBe(false);
    expect(isValidRoomSlug('has space')).toBe(false);
    expect(isValidRoomSlug('under_score')).toBe(false);
  });

  it('accepts a broadcast-shaped dynamic code in either case', () => {
    expect(isValidRoomCode('AB2CD3')).toBe(true);
    expect(isValidRoomCode('ab2cd3')).toBe(true);
    // A six-character string outside the broadcast alphabet is still a
    // valid slug — well-formed enough to ask the relay, which answers 404.
    expect(isValidRoomCode('AB0CD1')).toBe(true);
  });
});

describe('parseRoomLink', () => {
  it('reads a code out of a full room link, a hash, a path or a bare code', () => {
    expect(parseRoomLink('https://gawk.ioio.fi/#/room/TuhisRoom')).toEqual({ code: 'TuhisRoom', grant: null });
    expect(parseRoomLink('#/room/AB2CD3')).toEqual({ code: 'AB2CD3', grant: null });
    expect(parseRoomLink('/room/ab2cd3?relay=https://x.example')).toEqual({ code: 'ab2cd3', grant: null });
    expect(parseRoomLink('  TuhisRoom ')).toEqual({ code: 'TuhisRoom', grant: null });
  });

  it('carries a ?rt= grant beside the code', () => {
    expect(parseRoomLink('https://g/#/room/AB2CD3?rt=a:secret&relay=https://x.example')).toEqual({
      code: 'AB2CD3',
      grant: 'a:secret',
    });
  });

  it('refuses junk', () => {
    expect(parseRoomLink('')).toBeNull();
    expect(parseRoomLink('https://g/#/view/AB2CD3')).toBeNull();
    expect(parseRoomLink('#/room/ab')).toBeNull();
    expect(parseRoomLink('not a code!')).toBeNull();
  });
});
