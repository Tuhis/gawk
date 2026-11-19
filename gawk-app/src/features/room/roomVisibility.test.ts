import { describe, expect, it } from 'vitest';
import { revealStream, setVisibility, shownStreams, toggleStream, visibilityOf, type VisState } from './roomVisibility';
import type { RoomAttachment } from '../../transport/wire';

const att = (broadcastId: string): RoomAttachment => ({ broadcastId, label: broadcastId.toLowerCase(), live: true, viewerCount: 0 });
const ROOM = [att('A'), att('B'), att('C')];
const ALL: VisState = { videosOff: false, picks: { choose: false, hidden: [] } };
const ids = (s: VisState) => shownStreams(ROOM, s).map((n) => `${n.index}${n.attachment.broadcastId}`);

describe('room visibility (docs/90)', () => {
  it('shows everyone, the unhidden, or nobody; numbers stay the room’s (D4, D7)', () => {
    expect(ids(ALL)).toEqual(['1A', '2B', '3C']);
    const some: VisState = { videosOff: false, picks: { choose: true, hidden: ['B'] } };
    expect(visibilityOf(some)).toBe('some');
    expect(ids(some)).toEqual(['1A', '3C']);
    // The set is kept under All streams, not applied.
    expect(ids({ ...some, picks: { ...some.picks, choose: false } })).toEqual(['1A', '2B', '3C']);
    expect(ids({ ...some, videosOff: true })).toEqual([]);
    expect(visibilityOf({ ...some, videosOff: true })).toBe('none');
  });

  it('a stream that joins later arrives shown', () => {
    const some: VisState = { videosOff: false, picks: { choose: true, hidden: ['B'] } };
    expect(shownStreams([...ROOM, att('D')], some).map((n) => n.attachment.broadcastId)).toEqual(['A', 'C', 'D']);
  });

  it('switches between the three, keeping the set (D5)', () => {
    const some: VisState = { videosOff: false, picks: { choose: true, hidden: ['B'] } };
    const none = setVisibility(some, 'none');
    expect(none).toEqual({ videosOff: true, picks: some.picks });
    // Off None, the custom set comes back.
    expect(setVisibility(none, 'some')).toEqual(some);
    const all = setVisibility(none, 'all');
    expect(all).toEqual({ videosOff: false, picks: { choose: false, hidden: ['B'] } });
    expect(setVisibility(all, 'some')).toEqual(some);
    // Unchanged is the same object, so nothing is saved for nothing.
    expect(setVisibility(some, 'some')).toBe(some);
    expect(setVisibility(none, 'none')).toBe(none);
  });

  it('Shift+number hides a stream on the stage and shows one off it (D7)', () => {
    // From All streams: Choose with only that one hidden, whatever set was
    // kept from before.
    const kept: VisState = { videosOff: false, picks: { choose: false, hidden: ['A'] } };
    expect(toggleStream(kept, 'B')).toEqual({ videosOff: false, picks: { choose: true, hidden: ['B'] } });
    const some: VisState = { videosOff: false, picks: { choose: true, hidden: ['B'] } };
    expect(toggleStream(some, 'C')).toEqual({ videosOff: false, picks: { choose: true, hidden: ['B', 'C'] } });
    expect(toggleStream(some, 'B')).toEqual({ videosOff: false, picks: { choose: true, hidden: [] } });
    // Under None every stream is off the stage: the key brings that one
    // back, with the rest as they were.
    expect(toggleStream({ ...some, videosOff: true }, 'A')).toEqual(some);
    expect(toggleStream({ ...some, videosOff: true }, 'B')).toEqual({ videosOff: false, picks: { choose: true, hidden: [] } });
  });

  it('a number or a layout pick reveals: leaves None, unhides that stream (D7, D8)', () => {
    const some: VisState = { videosOff: true, picks: { choose: true, hidden: ['B'] } };
    expect(revealStream(some, null)).toEqual({ videosOff: false, picks: some.picks });
    expect(revealStream(some, 'B')).toEqual({ videosOff: false, picks: { choose: true, hidden: [] } });
    // Nothing to do is the same object.
    expect(revealStream(ALL, 'A')).toBe(ALL);
    const kept: VisState = { videosOff: false, picks: { choose: false, hidden: ['B'] } };
    expect(revealStream(kept, 'B')).toBe(kept);
  });
});
