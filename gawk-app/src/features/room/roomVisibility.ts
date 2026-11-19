// R86 (docs/90): which of a room's streams are on the stage, as pure
// functions over the two stored facts — None (global) and the room's picks.
// RoomScreen holds the state; this module decides what it means.

import type { RoomAttachment } from '../../transport/wire';
import type { RoomPicks, RoomVisibility } from './roomPrefs';

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

export function visibilityOf(s: VisState): RoomVisibility {
  if (s.videosOff) return 'none';
  return s.picks.choose ? 'some' : 'all';
}

// The streams on the stage, in room order. New streams are not in the hidden
// set, so they arrive shown (D4).
export function shownStreams(attachments: RoomAttachment[], s: VisState): NumberedStream[] {
  const visibility = visibilityOf(s);
  if (visibility === 'none') return [];
  const numbered = attachments.map((attachment, i) => ({ attachment, index: i + 1 }));
  if (visibility === 'all') return numbered;
  return numbered.filter((n) => !s.picks.hidden.includes(n.attachment.broadcastId));
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
// bringing it back would hide streams the user is looking at (D7).
export function toggleStream(s: VisState, id: string): VisState {
  const visibility = visibilityOf(s);
  const onStage = visibility === 'all' || (visibility === 'some' && !s.picks.hidden.includes(id));
  if (!onStage) return revealStream(s, id);
  return {
    videosOff: false,
    picks: { choose: true, hidden: visibility === 'all' ? [id] : [...s.picks.hidden, id] },
  };
}
