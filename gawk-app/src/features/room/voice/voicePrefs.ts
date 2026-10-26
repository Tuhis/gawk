// The per-browser voice preferences (R79, docs/80 OD10, D22):
//
//   gawk:voice-mode            vad | ptt — voice activity or push-to-talk
//   gawk:voice-ptt-key         the push-to-talk key, a KeyboardEvent.code
//                              (layout-independent: "KeyV" is the same key
//                              on QWERTY and AZERTY)
//   gawk:voice-volume:<name>   one person's level, 0..1, keyed by their Mumble
//                              name (the one stable handle a native Mumble
//                              user has across sessions)

import { readStored, writeStored } from '../../../lib/storage';

export type TalkMode = 'vad' | 'ptt';

const MODE_KEY = 'gawk:voice-mode';
const PTT_KEY = 'gawk:voice-ptt-key';
const VOLUME_PREFIX = 'gawk:voice-volume:';

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
