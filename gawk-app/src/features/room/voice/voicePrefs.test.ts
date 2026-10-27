// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import {
  DEFAULT_LEVEL_THRESHOLD_DB,
  DEFAULT_NOISE_MODE,
  DEFAULT_SPEECH_THRESHOLD,
  clampLevelThresholdDb,
  clampSpeechThreshold,
  clearAudioPrefs,
  loadLevelThresholdDb,
  loadNoiseMode,
  loadSpeechThreshold,
  saveLevelThresholdDb,
  saveNoiseMode,
  saveSpeechThreshold,
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

describe('audio preferences (R80, docs/81 D17)', () => {
  it('noise suppression defaults to RNNoise and persists Browser and Off', () => {
    expect(DEFAULT_NOISE_MODE).toBe('rnnoise');
    expect(loadNoiseMode()).toBe('rnnoise');
    saveNoiseMode('browser');
    expect(loadNoiseMode()).toBe('browser');
    saveNoiseMode('off');
    expect(localStorage.getItem('gawk:voice-noise')).toBe('off');
    localStorage.setItem('gawk:voice-noise', 'krisp');
    expect(loadNoiseMode()).toBe('rnnoise');
  });

  it('the speech threshold defaults to 0.90 and is clamped to 0.50..0.99', () => {
    expect(loadSpeechThreshold()).toBe(DEFAULT_SPEECH_THRESHOLD);
    expect(DEFAULT_SPEECH_THRESHOLD).toBe(0.9);
    saveSpeechThreshold(0.75);
    expect(loadSpeechThreshold()).toBe(0.75);
    saveSpeechThreshold(2);
    expect(loadSpeechThreshold()).toBe(0.99);
    localStorage.setItem('gawk:voice-speech-threshold', '0.1');
    expect(loadSpeechThreshold()).toBe(0.5);
    for (const junk of ['x', '', 'NaN']) {
      localStorage.setItem('gawk:voice-speech-threshold', junk);
      expect(loadSpeechThreshold()).toBe(DEFAULT_SPEECH_THRESHOLD);
    }
    expect(clampSpeechThreshold(Number.NaN)).toBe(DEFAULT_SPEECH_THRESHOLD);
  });

  it('the level threshold defaults to -40 dBFS and is clamped to -70..-10', () => {
    expect(loadLevelThresholdDb()).toBe(DEFAULT_LEVEL_THRESHOLD_DB);
    saveLevelThresholdDb(-55);
    expect(loadLevelThresholdDb()).toBe(-55);
    saveLevelThresholdDb(-200);
    expect(loadLevelThresholdDb()).toBe(-70);
    expect(clampLevelThresholdDb(0)).toBe(-10);
    expect(clampLevelThresholdDb(Number.POSITIVE_INFINITY)).toBe(DEFAULT_LEVEL_THRESHOLD_DB);
  });

  it('reset removes all three, so a later default reaches this browser', () => {
    saveNoiseMode('off');
    saveSpeechThreshold(0.6);
    saveLevelThresholdDb(-30);
    savePttKey('KeyT');
    clearAudioPrefs();
    expect(localStorage.getItem('gawk:voice-noise')).toBeNull();
    expect(localStorage.getItem('gawk:voice-speech-threshold')).toBeNull();
    expect(localStorage.getItem('gawk:voice-level-threshold')).toBeNull();
    expect(loadPttKey()).toBe('KeyT');
  });
});
