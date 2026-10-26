// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import {
  DEFAULT_PTT_KEY,
  loadPttKey,
  loadSpeakerVolume,
  loadTalkMode,
  pttKeyLabel,
  savePttKey,
  saveSpeakerVolume,
  saveTalkMode,
  validPttKey,
} from './voicePrefs';

beforeEach(() => localStorage.clear());

describe('voice preferences', () => {
  it('talk mode defaults to voice activity and persists push-to-talk', () => {
    expect(loadTalkMode()).toBe('vad');
    saveTalkMode('ptt');
    expect(loadTalkMode()).toBe('ptt');
    localStorage.setItem('gawk:voice-mode', 'nonsense');
    expect(loadTalkMode()).toBe('vad');
  });

  it('the push-to-talk key defaults to V, persists a valid code and refuses the rest', () => {
    expect(loadPttKey()).toBe(DEFAULT_PTT_KEY);
    savePttKey('KeyT');
    expect(loadPttKey()).toBe('KeyT');
    savePttKey('Tab');
    expect(loadPttKey()).toBe('KeyT');
    localStorage.setItem('gawk:voice-ptt-key', 'Escape');
    expect(loadPttKey()).toBe(DEFAULT_PTT_KEY);
    expect(validPttKey('')).toBe(false);
    expect(validPttKey('Key V')).toBe(false);
    expect(validPttKey('ShiftLeft')).toBe(true);
  });

  it('labels key codes for people', () => {
    expect(pttKeyLabel('KeyV')).toBe('V');
    expect(pttKeyLabel('Digit5')).toBe('5');
    expect(pttKeyLabel('ShiftLeft')).toBe('Left Shift');
    expect(pttKeyLabel('ControlRight')).toBe('Right Ctrl');
    expect(pttKeyLabel('Space')).toBe('Space');
  });

  it('per-person volume persists by Mumble name, clamped', () => {
    expect(loadSpeakerVolume('bob')).toBe(1);
    saveSpeakerVolume('bob', 0.4);
    expect(loadSpeakerVolume('bob')).toBe(0.4);
    saveSpeakerVolume('bob', 7);
    expect(loadSpeakerVolume('bob')).toBe(1);
    localStorage.setItem('gawk:voice-volume:eve', 'x');
    expect(loadSpeakerVolume('eve')).toBe(1);
    expect(loadSpeakerVolume('tuhis [web]')).toBe(1);
  });
});
