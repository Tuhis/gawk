// R42 (docs/44 §4.9): the per-browser room preferences. Three keys, all
// under the `gawk:` prefix the viewer's settings use, each read defensively
// (private mode, quota) so a storage failure only costs the memory.
//
//   gawk:nickname                 the participant's remembered nickname (D10)
//   gawk:room-mode                grid | focus | hidden — the layout mode
//   gawk:room-preset              the playback preset the focused / grid
//                                 tiles use (small tiles always run 'lowest')
//   gawk:room-volume:<broadcast>  a tile's own level, 0..1 (docs/44 §4.7)

import { MAX_ROOM_NICKNAME_LEN } from '../../transport/wire';
import { DEFAULT_PRESET, PRESETS, type PresetId } from '../viewer/playbackPresets';

export type RoomMode = 'grid' | 'focus' | 'hidden';

const NICKNAME_KEY = 'gawk:nickname';
const MODE_KEY = 'gawk:room-mode';
const PRESET_KEY = 'gawk:room-preset';
const VOLUME_PREFIX = 'gawk:room-volume:';

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string | null): void {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // private mode etc. — the choice still holds for this session
  }
}

// Trim and bound to the wire's nickname limit (bytes, so a long emoji name
// is cut conservatively by code units first, then by encoded length).
export function sanitizeNickname(raw: string): string {
  let s = raw.trim().replace(/\s+/g, ' ');
  while (s.length > 0 && new TextEncoder().encode(s).length > MAX_ROOM_NICKNAME_LEN) {
    s = s.slice(0, -1);
  }
  return s;
}

export function loadNickname(): string | null {
  const v = read(NICKNAME_KEY);
  if (v === null) return null;
  const clean = sanitizeNickname(v);
  return clean === '' ? null : clean;
}

export function saveNickname(nickname: string): void {
  const clean = sanitizeNickname(nickname);
  write(NICKNAME_KEY, clean === '' ? null : clean);
}

export function loadRoomMode(): RoomMode {
  const v = read(MODE_KEY);
  return v === 'focus' || v === 'hidden' ? v : 'grid';
}

export function saveRoomMode(mode: RoomMode): void {
  write(MODE_KEY, mode);
}

export function loadRoomPreset(): PresetId {
  const v = read(PRESET_KEY);
  return PRESETS.some((p) => p.id === v) ? (v as PresetId) : DEFAULT_PRESET;
}

export function saveRoomPreset(id: PresetId): void {
  write(PRESET_KEY, id);
}

export function loadTileVolume(broadcastId: string): number {
  const raw = read(VOLUME_PREFIX + broadcastId);
  if (raw === null) return 1;
  const v = Number(raw);
  return Number.isFinite(v) ? Math.max(0, Math.min(1, v)) : 1;
}

export function saveTileVolume(broadcastId: string, volume: number): void {
  write(VOLUME_PREFIX + broadcastId, String(Math.max(0, Math.min(1, volume))));
}
