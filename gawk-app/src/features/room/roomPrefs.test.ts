// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  loadNickname,
  loadRoomAllMode,
  loadRoomMode,
  loadRoomPicks,
  loadRoomPreset,
  loadTileVolume,
  loadVideosOff,
  MAX_HIDDEN_PER_ROOM,
  MAX_PICK_ROOMS,
  sanitizeNickname,
  saveNickname,
  saveRoomAllMode,
  saveRoomMode,
  saveRoomPicks,
  saveRoomPreset,
  saveTileVolume,
  saveVideosOff,
} from './roomPrefs';
import { ROOM_CLIENT_WEB_VIEWER, ROOM_PROTOCOL_VERSION, encodeRoomHello } from '../../transport/wire';

beforeEach(() => localStorage.clear());

describe('room prefs', () => {
  it('remembers the nickname under gawk:nickname, sanitized', () => {
    expect(loadNickname()).toBeNull();
    saveNickname('  tuhis   k ');
    expect(localStorage.getItem('gawk:nickname')).toBe('tuhis k');
    expect(loadNickname()).toBe('tuhis k');
    saveNickname('   ');
    expect(loadNickname()).toBeNull();
  });

  it('bounds the nickname to the wire limit in bytes', () => {
    expect(sanitizeNickname('x'.repeat(40))).toHaveLength(32);
    // 3-byte characters: 10 fit (11 would be 33 bytes).
    expect(sanitizeNickname('€'.repeat(20))).toBe('€'.repeat(10));
  });

  it('never splits a character to fit, which the wire would reject', () => {
    const clean = sanitizeNickname('a'.repeat(29) + '😀');
    expect(clean).toBe('a'.repeat(29));
    expect(() =>
      encodeRoomHello({ protocol: ROOM_PROTOCOL_VERSION, clientKind: ROOM_CLIENT_WEB_VIEWER, wantCaps: 0, nickname: clean }),
    ).not.toThrow();
  });

  it('defaults the mode to grid and only accepts known modes', () => {
    expect(loadRoomMode()).toBe('grid');
    saveRoomMode('focus');
    expect(loadRoomMode()).toBe('focus');
    localStorage.setItem('gawk:room-mode', 'sideways');
    expect(loadRoomMode()).toBe('grid');
    saveRoomMode('fit');
    expect(loadRoomMode()).toBe('fit');
  });

  it('remembers the last all-POV mode, grid by default (docs/87 D2)', () => {
    expect(loadRoomAllMode()).toBe('grid');
    saveRoomAllMode('fit');
    expect(loadRoomAllMode()).toBe('fit');
    localStorage.setItem('gawk:room-grid', 'focus');
    expect(loadRoomAllMode()).toBe('grid');
  });

  // R86 (docs/90 D6): Hide videos was a fourth mode until None took it over.
  it('loads a stored hidden mode as None, on the last all-POV layout, once', () => {
    localStorage.setItem('gawk:room-grid', 'fit');
    localStorage.setItem('gawk:room-mode', 'hidden');
    expect(loadVideosOff()).toBe(true);
    expect(loadRoomMode()).toBe('fit');
    expect(localStorage.getItem('gawk:room-mode')).toBe('fit');
    // Leaving None sticks: the old value is gone, so it cannot come back.
    saveVideosOff(false);
    expect(loadVideosOff()).toBe(false);
    expect(localStorage.getItem('gawk:room-videos')).toBeNull();

    localStorage.clear();
    localStorage.setItem('gawk:room-mode', 'hidden');
    expect(loadRoomMode()).toBe('grid');
    expect(loadVideosOff()).toBe(true);
  });

  it('keeps the stored hidden mode as None when None cannot be written (a full quota)', () => {
    localStorage.setItem('gawk:room-mode', 'hidden');
    const setItem = Storage.prototype.setItem;
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, k: string, v: string) {
      if (k === 'gawk:room-videos') throw new DOMException('full', 'QuotaExceededError');
      setItem.call(this, k, v);
    });
    try {
      expect(loadVideosOff()).toBe(true);
      expect(loadRoomMode()).toBe('grid');
      expect(localStorage.getItem('gawk:room-mode')).toBe('hidden');
      expect(loadVideosOff()).toBe(true);
      // Leaving None clears it all the same.
      saveVideosOff(false);
      expect(loadVideosOff()).toBe(false);
    } finally {
      spy.mockRestore();
    }
  });

  it('remembers None across rooms, off by default', () => {
    expect(loadVideosOff()).toBe(false);
    saveVideosOff(true);
    expect(localStorage.getItem('gawk:room-videos')).toBe('off');
    expect(loadVideosOff()).toBe(true);
  });

  it('keeps each room’s picks by its code, and forgets a room back to All streams', () => {
    expect(loadRoomPicks('AB2CD3')).toEqual({ choose: false, hidden: [] });
    saveRoomPicks('AB2CD3', { choose: true, hidden: ['AAAAAA'] });
    saveRoomPicks('devroom', { choose: false, hidden: ['BBBBBB'] });
    expect(loadRoomPicks('AB2CD3')).toEqual({ choose: true, hidden: ['AAAAAA'] });
    // All streams keeps the set for the way back to Choose (D5).
    expect(loadRoomPicks('devroom')).toEqual({ choose: false, hidden: ['BBBBBB'] });
    expect(loadRoomPicks('ZZZZZZ')).toEqual({ choose: false, hidden: [] });

    saveRoomPicks('AB2CD3', { choose: false, hidden: [] });
    expect(loadRoomPicks('AB2CD3')).toEqual({ choose: false, hidden: [] });
    expect(JSON.parse(localStorage.getItem('gawk:room-picks')!)).toHaveLength(1);
    saveRoomPicks('devroom', { choose: false, hidden: [] });
    expect(localStorage.getItem('gawk:room-picks')).toBeNull();

    // A room not yet known by a code (a mint before its first state) has
    // nothing to remember under.
    saveRoomPicks('', { choose: true, hidden: ['AAAAAA'] });
    expect(loadRoomPicks('')).toEqual({ choose: false, hidden: [] });
    expect(localStorage.getItem('gawk:room-picks')).toBeNull();
  });

  it('caps the rooms remembered, dropping the least recently changed, and the IDs per room', () => {
    // All-digit codes too: an object keyed by code would order those first.
    const codes = Array.from({ length: MAX_PICK_ROOMS + 2 }, (_, i) => String(100000 + i));
    for (const c of codes) saveRoomPicks(c, { choose: true, hidden: [c] });
    expect(loadRoomPicks(codes[0])).toEqual({ choose: false, hidden: [] });
    expect(loadRoomPicks(codes[1])).toEqual({ choose: false, hidden: [] });
    expect(loadRoomPicks(codes[2])).toEqual({ choose: true, hidden: [codes[2]] });
    // Changing a room makes it the most recent.
    saveRoomPicks(codes[2], { choose: true, hidden: ['X'] });
    saveRoomPicks('NEW', { choose: true, hidden: [] });
    expect(loadRoomPicks(codes[2])).toEqual({ choose: true, hidden: ['X'] });
    expect(loadRoomPicks(codes[3])).toEqual({ choose: false, hidden: [] });

    const many = Array.from({ length: MAX_HIDDEN_PER_ROOM + 5 }, (_, i) => `ID${i}`);
    saveRoomPicks('AB2CD3', { choose: true, hidden: many });
    expect(loadRoomPicks('AB2CD3').hidden).toEqual(many.slice(-MAX_HIDDEN_PER_ROOM));
  });

  it('reads malformed picks as nothing remembered', () => {
    for (const raw of ['{', '{"AB2CD3":{"choose":true}}', '[null,1,"x",{"code":5},{"code":""}]']) {
      localStorage.setItem('gawk:room-picks', raw);
      expect(loadRoomPicks('AB2CD3')).toEqual({ choose: false, hidden: [] });
    }
    localStorage.setItem('gawk:room-picks', '[{"code":"AB2CD3","choose":"yes","hidden":["A",2,null,"B"]}]');
    expect(loadRoomPicks('AB2CD3')).toEqual({ choose: false, hidden: ['A', 'B'] });
  });

  it('defaults the preset to balanced and refuses unknown ids', () => {
    expect(loadRoomPreset()).toBe('balanced');
    saveRoomPreset('lowest');
    expect(loadRoomPreset()).toBe('lowest');
    localStorage.setItem('gawk:room-preset', 'turbo');
    expect(loadRoomPreset()).toBe('balanced');
  });

  it('keeps a per-broadcast tile volume, clamped', () => {
    expect(loadTileVolume('AB2CD3')).toBe(1);
    saveTileVolume('AB2CD3', 0.3);
    expect(localStorage.getItem('gawk:room-volume:AB2CD3')).toBe('0.3');
    expect(loadTileVolume('AB2CD3')).toBe(0.3);
    saveTileVolume('AB2CD3', 4);
    expect(loadTileVolume('AB2CD3')).toBe(1);
    localStorage.setItem('gawk:room-volume:AB2CD3', 'loud');
    expect(loadTileVolume('AB2CD3')).toBe(1);
  });
});
