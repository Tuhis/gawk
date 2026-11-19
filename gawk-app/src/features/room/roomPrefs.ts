// The per-browser room preferences:
//
//   gawk:nickname                 the participant's remembered nickname
//   gawk:room-mode                grid | fit | focus — the layout mode (a
//                                 stored `hidden`, from before R86, loads as
//                                 None: docs/90 D6)
//   gawk:room-grid                grid | fit — the last all-POV mode chosen,
//                                 where `0` returns to (R83, docs/87 D2)
//   gawk:room-videos              `off` while None is on, in every room (the
//                                 old Hide videos; docs/90 D5)
//   gawk:room-picks               per room code, whether Choose is on and
//                                 the broadcast IDs hidden there (docs/90 D5)
//   gawk:room-preset              the playback preset the focused / grid
//                                 tiles use (small tiles drop its pacing)
//   gawk:room-volume:<broadcast>  a tile's own level, 0..1

import { readStored, writeStored } from '../../lib/storage';
import { MAX_ROOM_NICKNAME_LEN } from '../../transport/wire';
import { DEFAULT_PRESET, PRESETS, type PresetId } from '../viewer/playbackPresets';

export type RoomMode = 'grid' | 'fit' | 'focus';
// The modes that show every POV at once.
export type RoomAllMode = 'grid' | 'fit';

// R86 (docs/90): which streams are on the stage. 'some' is Choose: every
// stream but the hidden ones.
export type RoomVisibility = 'all' | 'some' | 'none';

// A room's Choose state. `hidden` is kept while All streams is on, so going
// back to Choose restores it (D5).
export interface RoomPicks {
  choose: boolean;
  hidden: string[];
}

export const NO_PICKS: RoomPicks = { choose: false, hidden: [] };

const NICKNAME_KEY = 'gawk:nickname';
const MODE_KEY = 'gawk:room-mode';
const GRID_KEY = 'gawk:room-grid';
const PRESET_KEY = 'gawk:room-preset';
const VIDEOS_KEY = 'gawk:room-videos';
const PICKS_KEY = 'gawk:room-picks';
// D5: the rooms remembered, most recently changed last, and the hidden IDs
// kept per room.
export const MAX_PICK_ROOMS = 20;
export const MAX_HIDDEN_PER_ROOM = 64;
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

// D6: before R86 Hide videos was a fourth layout mode. A stored one becomes
// None, with the layout `0` would have returned to. Runs before every read
// of either key, and once it has rewritten the mode it never runs again.
function migrateHiddenMode(): void {
  if (readStored(MODE_KEY) !== 'hidden') return;
  writeStored(VIDEOS_KEY, 'off');
  writeStored(MODE_KEY, loadRoomAllMode());
}

export function loadRoomMode(): RoomMode {
  migrateHiddenMode();
  const v = readStored(MODE_KEY);
  return v === 'fit' || v === 'focus' ? v : 'grid';
}

export function saveRoomMode(mode: RoomMode): void {
  writeStored(MODE_KEY, mode);
}

export function loadRoomAllMode(): RoomAllMode {
  return readStored(GRID_KEY) === 'fit' ? 'fit' : 'grid';
}

export function saveRoomAllMode(mode: RoomAllMode): void {
  writeStored(GRID_KEY, mode);
}

// None: no stream on the stage, in any room.
export function loadVideosOff(): boolean {
  migrateHiddenMode();
  return readStored(VIDEOS_KEY) === 'off';
}

export function saveVideosOff(off: boolean): void {
  writeStored(VIDEOS_KEY, off ? 'off' : null);
}

interface StoredPicks {
  code: string;
  choose: boolean;
  hidden: string[];
}

// The stored list, most recently changed last. An array, not an object keyed
// by code: an all-digit code would be an integer key, which objects order
// first whatever its age. Anything malformed reads as nothing remembered.
function readPicks(): StoredPicks[] {
  const raw = readStored(PICKS_KEY);
  if (raw === null) return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: StoredPicks[] = [];
  for (const e of parsed as unknown[]) {
    if (typeof e !== 'object' || e === null) continue;
    const { code, choose, hidden } = e as Record<string, unknown>;
    if (typeof code !== 'string' || code === '') continue;
    out.push({
      code,
      choose: choose === true,
      hidden: Array.isArray(hidden)
        ? hidden.filter((id): id is string => typeof id === 'string').slice(-MAX_HIDDEN_PER_ROOM)
        : [],
    });
  }
  return out;
}

export function loadRoomPicks(code: string): RoomPicks {
  if (code === '') return NO_PICKS;
  const e = readPicks().find((p) => p.code === code);
  return e ? { choose: e.choose, hidden: e.hidden } : NO_PICKS;
}

// Moves the room to the end of the list, then drops the oldest past the cap.
// A room back to All streams with nothing hidden is forgotten.
export function saveRoomPicks(code: string, picks: RoomPicks): void {
  if (code === '') return;
  const list = readPicks().filter((p) => p.code !== code);
  if (picks.choose || picks.hidden.length > 0) {
    list.push({ code, choose: picks.choose, hidden: picks.hidden.slice(-MAX_HIDDEN_PER_ROOM) });
  }
  const kept = list.slice(-MAX_PICK_ROOMS);
  writeStored(PICKS_KEY, kept.length > 0 ? JSON.stringify(kept) : null);
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
