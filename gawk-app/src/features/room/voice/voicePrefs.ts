// The per-browser voice preferences (R79, docs/80 OD10, D22):
//
//   gawk:voice-mode            vad | ptt — voice activity or push-to-talk
//   gawk:voice-ptt-key         the push-to-talk key, a KeyboardEvent.code
//                              (layout-independent: "KeyV" is the same key
//                              on QWERTY and AZERTY)
//   gawk:voice-volume:<name>   one person's level, 0..1, keyed by their Mumble
//                              name (the one stable handle a native Mumble
//                              user has across sessions)
//
// and R80's (docs/81 D17):
//
//   gawk:voice-noise              rnnoise | browser | off — the noise
//                                 suppression the microphone runs through
//   gawk:voice-speech-threshold   the speech detector's `on`, 0.50..0.99
//   gawk:voice-level-threshold    the level gate's threshold, dBFS

import { readStored, writeStored } from '../../../lib/storage';

export type TalkMode = 'vad' | 'ptt';
// RNNoise in the capture worklet; the browser's own suppression; neither
// (docs/81 D11).
export type NoiseMode = 'rnnoise' | 'browser' | 'off';

const MODE_KEY = 'gawk:voice-mode';
const PTT_KEY = 'gawk:voice-ptt-key';
const VOLUME_PREFIX = 'gawk:voice-volume:';
const NOISE_KEY = 'gawk:voice-noise';
const SPEECH_THRESHOLD_KEY = 'gawk:voice-speech-threshold';
const LEVEL_THRESHOLD_KEY = 'gawk:voice-level-threshold';

// docs/81 OD2: RNNoise is on by default.
export const DEFAULT_NOISE_MODE: NoiseMode = 'rnnoise';
// docs/81 D9: the speech detector's `on`, and the slider's range.
export const DEFAULT_SPEECH_THRESHOLD = 0.9;
export const MIN_SPEECH_THRESHOLD = 0.5;
export const MAX_SPEECH_THRESHOLD = 0.99;
// docs/81 D10: the level gate's threshold, dBFS (R79's 0.01 RMS), and the
// slider's range.
export const DEFAULT_LEVEL_THRESHOLD_DB = -40;
export const MIN_LEVEL_THRESHOLD_DB = -70;
export const MAX_LEVEL_THRESHOLD_DB = -10;

// Holding V talks: not one of the room's own hotkeys (0–4, f), and in reach
// of a hand on WASD.
export const DEFAULT_PTT_KEY = 'KeyV';

export function loadTalkMode(): TalkMode {
  return readStored(MODE_KEY) === 'ptt' ? 'ptt' : 'vad';
}

export function saveTalkMode(mode: TalkMode): void {
  writeStored(MODE_KEY, mode);
}

// Keys that make no sense to hold for talking: they type, move focus or
// leave a dialog.
const REFUSED_KEYS = new Set(['Tab', 'Escape', 'Enter', 'NumpadEnter', 'Backspace']);

export function validPttKey(code: string): boolean {
  return code !== '' && !REFUSED_KEYS.has(code) && /^[A-Za-z0-9]+$/.test(code);
}

export function loadPttKey(): string {
  const v = readStored(PTT_KEY);
  return v !== null && validPttKey(v) ? v : DEFAULT_PTT_KEY;
}

export function savePttKey(code: string): void {
  if (validPttKey(code)) writeStored(PTT_KEY, code);
}

// "KeyV" → "V", "Digit5" → "5", "Space" → "Space", "ShiftLeft" → "Left Shift".
export function pttKeyLabel(code: string): string {
  if (/^Key[A-Z]$/.test(code)) return code.slice(3);
  if (/^Digit[0-9]$/.test(code)) return code.slice(5);
  const side = /^(Shift|Control|Alt|Meta)(Left|Right)$/.exec(code);
  if (side) return `${side[2]} ${side[1] === 'Control' ? 'Ctrl' : side[1]}`;
  return code;
}

export function loadSpeakerVolume(name: string): number {
  const raw = readStored(VOLUME_PREFIX + name);
  if (raw === null) return 1;
  const v = Number(raw);
  return Number.isFinite(v) ? Math.max(0, Math.min(1, v)) : 1;
}

export function saveSpeakerVolume(name: string, volume: number): void {
  writeStored(VOLUME_PREFIX + name, String(Math.max(0, Math.min(1, volume))));
}

export function loadNoiseMode(): NoiseMode {
  const v = readStored(NOISE_KEY);
  return v === 'rnnoise' || v === 'browser' || v === 'off' ? v : DEFAULT_NOISE_MODE;
}

export function saveNoiseMode(mode: NoiseMode): void {
  writeStored(NOISE_KEY, mode);
}

// A stored number, clamped into [min, max]; the default when absent or junk.
function loadNumber(key: string, min: number, max: number, dflt: number): number {
  const raw = readStored(key);
  if (raw === null || raw.trim() === '') return dflt;
  const v = Number(raw);
  return Number.isFinite(v) ? clamp(v, min, max) : dflt;
}

function clamp(v: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, v));
}

export function clampSpeechThreshold(v: number): number {
  return Number.isFinite(v) ? clamp(v, MIN_SPEECH_THRESHOLD, MAX_SPEECH_THRESHOLD) : DEFAULT_SPEECH_THRESHOLD;
}

export function clampLevelThresholdDb(v: number): number {
  return Number.isFinite(v) ? clamp(v, MIN_LEVEL_THRESHOLD_DB, MAX_LEVEL_THRESHOLD_DB) : DEFAULT_LEVEL_THRESHOLD_DB;
}

export function loadSpeechThreshold(): number {
  return loadNumber(SPEECH_THRESHOLD_KEY, MIN_SPEECH_THRESHOLD, MAX_SPEECH_THRESHOLD, DEFAULT_SPEECH_THRESHOLD);
}

export function saveSpeechThreshold(v: number): void {
  writeStored(SPEECH_THRESHOLD_KEY, String(clampSpeechThreshold(v)));
}

export function loadLevelThresholdDb(): number {
  return loadNumber(LEVEL_THRESHOLD_KEY, MIN_LEVEL_THRESHOLD_DB, MAX_LEVEL_THRESHOLD_DB, DEFAULT_LEVEL_THRESHOLD_DB);
}

export function saveLevelThresholdDb(v: number): void {
  writeStored(LEVEL_THRESHOLD_KEY, String(clampLevelThresholdDb(v)));
}

// Reset to defaults (docs/81 D15): the stored values go, so a later change of
// a default reaches this browser too.
export function clearAudioPrefs(): void {
  writeStored(NOISE_KEY, null);
  writeStored(SPEECH_THRESHOLD_KEY, null);
  writeStored(LEVEL_THRESHOLD_KEY, null);
}
