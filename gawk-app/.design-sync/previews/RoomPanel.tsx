import { RoomPanel } from 'gawk-app';

// RoomPanel is the room's people-and-chat side panel: `position: absolute`,
// pinned top/right/bottom at 340px wide. Each cell supplies the positioned
// stage the room screen provides, painted as a stand-in for the video grid.
//
// `snapshot` is a RoomState as the relay sends it. The bit fields drive what
// the panel shows — hand-written here, so they are NOT type-checked:
//   flags: 0x01 dynamic room (Copy room code), 0x02 you created it (Detach on
//          every stream, End room)
//   caps:  0x01 chat (reserves the chat slot)
//   participant.kind: 0 web viewer, 1 web broadcaster, 2 native broadcaster
//   participant.flags: 0x01 speaking, 0x02 streaming
const stage: React.CSSProperties = {
  position: 'relative',
  height: '700px',
  background:
    'radial-gradient(circle at 25% 30%, #2b3a6b 0%, transparent 55%),' +
    'radial-gradient(circle at 75% 70%, #6b2b45 0%, transparent 55%),' +
    'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  overflow: 'hidden',
};

const empty = new Uint8Array(0);
const noop = () => {};
const handlers = {
  onClose: noop,
  onDetach: noop,
  onSetNickname: noop,
  onCopyLink: noop,
  onCopyCode: noop,
  onEndRoom: noop,
};

const attachments = [
  { broadcastId: 'K7QMXP', label: 'tuhis', live: true, viewerCount: 12 },
  { broadcastId: 'R3HVNA', label: 'mika', live: true, viewerCount: 4 },
  { broadcastId: 'WD9TLC', label: 'sanni', live: false, viewerCount: 0 },
];

const participants = [
  { id: 1, kind: 1, flags: 0x02 | 0x01, nickname: 'tuhis', identity: '' },
  { id: 2, kind: 2, flags: 0x02, nickname: 'mika', identity: '' },
  { id: 3, kind: 2, flags: 0x02, nickname: 'sanni', identity: '' },
  { id: 4, kind: 0, flags: 0, nickname: 'guest-4821', identity: '' },
  { id: 5, kind: 0, flags: 0, nickname: 'jonna', identity: '' },
];

/** The creator of a dynamic room: Detach on every stream, Copy room code, End room. */
export const Creator = () => (
  <div style={stage}>
    <RoomPanel
      {...handlers}
      snapshot={{
        flags: 0x01 | 0x02,
        caps: 0,
        seq: 42,
        yourId: 1,
        code: 'FRIDAY',
        displayName: 'Friday night co-op',
        creatorToken: empty,
        key: empty,
        attachments,
        participants,
      } as never}
      nickname="tuhis"
      ownBroadcastId="K7QMXP"
      onStartStreaming={null}
    />
  </div>
);

/** A viewer in a static room: no detach or end, plus "Start streaming here". */
export const Viewer = () => (
  <div style={stage}>
    <RoomPanel
      {...handlers}
      snapshot={{
        flags: 0,
        caps: 0,
        seq: 7,
        yourId: 5,
        code: '',
        displayName: 'LAN party',
        creatorToken: empty,
        key: empty,
        attachments: attachments.slice(0, 2),
        participants,
      } as never}
      nickname="jonna"
      ownBroadcastId={null}
      onStartStreaming={noop}
    />
  </div>
);

/** An empty room that advertises chat: the placeholder stream note and the reserved chat slot. */
export const EmptyWithChat = () => (
  <div style={stage}>
    <RoomPanel
      {...handlers}
      snapshot={{
        flags: 0x01,
        caps: 0x01,
        seq: 1,
        yourId: 4,
        code: 'QUIET1',
        displayName: '',
        creatorToken: empty,
        key: empty,
        attachments: [],
        participants: [participants[3]],
      } as never}
      nickname="guest-4821"
      ownBroadcastId={null}
      onStartStreaming={noop}
    />
  </div>
);
