// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import {
  loadNickname,
  loadRoomMode,
  loadRoomPreset,
  loadTileVolume,
  sanitizeNickname,
  saveNickname,
  saveRoomMode,
  saveRoomPreset,
  saveTileVolume,
} from './roomPrefs';

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
    // 3-byte characters: 11 fit (33 would not).
    expect(sanitizeNickname('€'.repeat(20))).toBe('€'.repeat(10));
  });

  it('defaults the mode to grid and only accepts known modes', () => {
    expect(loadRoomMode()).toBe('grid');
    saveRoomMode('hidden');
    expect(loadRoomMode()).toBe('hidden');
    localStorage.setItem('gawk:room-mode', 'sideways');
    expect(loadRoomMode()).toBe('grid');
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
