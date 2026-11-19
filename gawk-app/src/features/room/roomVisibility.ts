// R86 (docs/90): which of a room's streams are on the stage, as pure
// functions over the two stored facts — None (global) and the room's picks.
// RoomScreen holds the state; this module decides what it means. Every
// transition returns its input unchanged when it changes nothing.

import type { RoomAttachment } from '../../transport/wire';
import { MAX_HIDDEN_PER_ROOM, type RoomPicks, type RoomVisibility } from './roomPrefs';

export interface VisState {
  videosOff: boolean;
  picks: RoomPicks;
}

// A stream with its room number: the position in the room's order, 1-based,
// whatever is hidden around it (D7).
export interface NumberedStream {
  attachment: RoomAttachment;
  index: number;
}

export function numberStreams(attachments: RoomAttachment[]): NumberedStream[] {
  return attachments.map((attachment, i) => ({ attachment, index: i + 1 }));
}

export function visibilityOf(s: VisState): RoomVisibility {
  if (s.videosOff) return 'none';
  return s.picks.choose ? 'some' : 'all';
}

// The streams on the stage, in room order. New streams are not in the hidden
// set, so they arrive shown (D4).
export function shownStreams(streams: NumberedStream[], s: VisState): NumberedStream[] {
  const visibility = visibilityOf(s);
  if (visibility === 'none') return [];
  if (visibility === 'all') return streams;
  const hidden = new Set(s.picks.hidden);
  return streams.filter((n) => !hidden.has(n.attachment.broadcastId));
}

export function setVisibility(s: VisState, v: RoomVisibility): VisState {
  if (v === 'none') return s.videosOff ? s : { ...s, videosOff: true };
  const choose = v === 'some';
  if (!s.videosOff && s.picks.choose === choose) return s;
  return {
    videosOff: false,
    picks: s.picks.choose === choose ? s.picks : { ...s.picks, choose },
  };
}

// Leaves None and takes the stream out of the hidden set: what a number key
// or a layout pick does before it shows anything (D7, D8).
export function revealStream(s: VisState, id: string | null): VisState {
  const hidden = id !== null && s.picks.choose && s.picks.hidden.includes(id);
  if (!s.videosOff && !hidden) return s;
  return {
    videosOff: false,
    picks: hidden ? { ...s.picks, hidden: s.picks.hidden.filter((h) => h !== id) } : s.picks,
  };
}

// Shift+number and the popover's checkboxes: hide the stream if it is on the
// stage, else show it. Hiding from All streams starts Choose with only this
// stream hidden; the set kept from an earlier Choose is not in effect, so
// bringing it back would hide streams the user is looking at (D7). `inRoom`
// is the room's roster, for the cap.
export function toggleStream(s: VisState, id: string, inRoom: ReadonlySet<string>): VisState {
  const visibility = visibilityOf(s);
  const onStage = visibility === 'all' || (visibility === 'some' && !s.picks.hidden.includes(id));
  if (!onStage) return revealStream(s, id);
  return {
    videosOff: false,
    picks: { choose: true, hidden: visibility === 'all' ? [id] : capHidden([...s.picks.hidden, id], inRoom) },
  };
}

// The Shift+number keys: as toggleStream, but a key that shows the last
// hidden stream goes back to All streams, so a peek and its undo (Shift+2,
// Shift+2) leave the room as it was rather than in Choose with nothing hidden
// (D7). The checkboxes don't: the user is in Choose there, looking at it.
export function toggleStreamByKey(s: VisState, id: string, inRoom: ReadonlySet<string>): VisState {
  const next = toggleStream(s, id, inRoom);
  if (next === s || !s.picks.choose || next.videosOff || !next.picks.choose || next.picks.hidden.length > 0) return next;
  return { videosOff: false, picks: { choose: false, hidden: [] } };
}

// D5's cap, past which the oldest IDs go — but streams that have left the
// room first, so one hidden long ago and still here is not the one dropped.
function capHidden(hidden: string[], inRoom: ReadonlySet<string>): string[] {
  let over = hidden.length - MAX_HIDDEN_PER_ROOM;
  if (over <= 0) return hidden;
  return hidden.filter((h) => inRoom.has(h) || over-- <= 0);
}
