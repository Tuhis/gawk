// The per-browser room preferences:
//
//   gawk:nickname                 the participant's remembered nickname
//   gawk:room-mode                grid | focus | hidden — the layout mode
//   gawk:room-preset              the playback preset the focused / grid
//                                 tiles use (small tiles drop its pacing)
//   gawk:room-volume:<broadcast>  a tile's own level, 0..1

import { readStored, writeStored } from '../../lib/storage';
import { MAX_ROOM_NICKNAME_LEN } from '../../transport/wire';
import { DEFAULT_PRESET, PRESETS, type PresetId } from '../viewer/playbackPresets';

export type RoomMode = 'grid' | 'focus' | 'hidden';

const NICKNAME_KEY = 'gawk:nickname';
const MODE_KEY = 'gawk:room-mode';
const PRESET_KEY = 'gawk:room-preset';
const VOLUME_PREFIX = 'gawk:room-volume:';

// Collapse whitespace and bound to `maxBytes` of UTF-8, the unit the wire
// limits are in. Cuts whole characters: half a surrogate pair is not valid
// UTF-8, and the room encoder refuses it.
export function sanitizeRoomText(raw: string, maxBytes: number): string {
  const chars = Array.from(raw.trim().replace(/\s+/g, ' '));
  const encoder = new TextEncoder();
  while (chars.length > 0 && encoder.encode(chars.join('')).length > maxBytes) chars.pop();
  return chars.join('').trim();
}

export function sanitizeNickname(raw: string): string {
  return sanitizeRoomText(raw, MAX_ROOM_NICKNAME_LEN);
}

export function loadNickname(): string | null {
  const v = readStored(NICKNAME_KEY);
  if (v === null) return null;
  const clean = sanitizeNickname(v);
  return clean === '' ? null : clean;
}

export function saveNickname(nickname: string): void {
  const clean = sanitizeNickname(nickname);
  writeStored(NICKNAME_KEY, clean === '' ? null : clean);
}

export function loadRoomMode(): RoomMode {
  const v = readStored(MODE_KEY);
  return v === 'focus' || v === 'hidden' ? v : 'grid';
}

export function saveRoomMode(mode: RoomMode): void {
  writeStored(MODE_KEY, mode);
}

export function loadRoomPreset(): PresetId {
  const v = readStored(PRESET_KEY);
  return PRESETS.some((p) => p.id === v) ? (v as PresetId) : DEFAULT_PRESET;
}

export function saveRoomPreset(id: PresetId): void {
  writeStored(PRESET_KEY, id);
}

export function loadTileVolume(broadcastId: string): number {
  const raw = readStored(VOLUME_PREFIX + broadcastId);
  if (raw === null) return 1;
  const v = Number(raw);
  return Number.isFinite(v) ? Math.max(0, Math.min(1, v)) : 1;
}

export function saveTileVolume(broadcastId: string, volume: number): void {
  writeStored(VOLUME_PREFIX + broadcastId, String(Math.max(0, Math.min(1, volume))));
}
