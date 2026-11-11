// The React glue between RoomSession (transport) and the room store.
// One session per mount, keyed on the target; every callback lands in the
// store, and the hook hands back the commands. The session dials on mount
// (an explicit event — CODE-REVIEW's effect rule), never on re-render: the
// nickname is read at mount through a ref and a later change travels as a
// command (setNickname), while the target and the grant are dial inputs — a
// change of either is a deliberate re-dial, keyed on content below.

import { useCallback, useEffect, useMemo, useRef } from 'react';
import { RoomSession, type RoomSessionGrant, type RoomTarget } from '../../transport/room-session';
import {
  CLOSE_CODE_SERVER_DRAINING,
  ROOM_COMMAND_VOICE_JOIN,
  ROOM_EVENT_COMMAND_REJECTED,
} from '../../transport/wire';
import { voicePlaybackSupported } from './voice/voicePlayback';
import { selectCertHashList, useTransportStore } from '../../state/transportStore';
import { useRoomStore } from '../../state/roomStore';
import { log } from '../../lib/logger';
import { DRAINING_NOTE, RECONNECTING_NOTE } from './roomCopy';
import { leaveUnreachableLocalRelay, noteLocalRelay, willFollowOffers } from '../pop/switch';

// R74 (docs/76 item 43.5): a PoP a followed offer chose that cannot serve
// the room (unreachable, refused, lost, or not holding it) sends the tab
// back home; the store change redials there. A full room or a refused key
// is the room's answer, the same from home.
function leaveLocalRelayAfter(kind: string): void {
  if (kind === 'full' || kind === 'forbidden') return;
  if (useTransportStore.getState().resolvedSource === 'pop') leaveUnreachableLocalRelay();
}

export interface RoomCommands {
  attach: (broadcastId: string, resumeTokenHex: string, label: string) => void;
  detach: (broadcastId: string) => void;
  setNickname: (nickname: string) => void;
  endRoom: () => void;
  // R79: asks for a voice ticket; false when no session can carry it now.
  voiceJoin: () => boolean;
}

// R79: where the room session hands the voice client its answers. Read
// through a ref, so a re-created object never re-dials.
export interface RoomVoiceEvents {
  onTicket: (url: string, ticket: Uint8Array) => void;
  // A CommandRejected for VoiceJoin — the voice panel's to explain, not the
  // room's generic rejection toast.
  onRejected: (reason: number, message: string) => void;
}

export interface UseRoomSessionArgs {
  target: RoomTarget | null; // null ⇒ no session (e.g. waiting for a nickname)
  nickname: string;
  clientKind: number;
  grant: RoomSessionGrant | null;
  // Bump to force a re-dial with an unchanged target and grant — the "try
  // that key again" case. Any change re-dials; the value itself
  // means nothing.
  dialNonce?: number;
  voiceEvents?: RoomVoiceEvents | null;
}

export function useRoomSession({
  target,
  nickname,
  clientKind,
  grant,
  dialNonce = 0,
  voiceEvents = null,
}: UseRoomSessionArgs): RoomCommands {
  // Same subscription discipline as useViewerConnection: a server change is
  // a deliberate re-dial, and the room's tiles re-dial with it.
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore(selectCertHashList);
  const sessionRef = useRef<RoomSession | null>(null);
  const nicknameRef = useRef(nickname);
  nicknameRef.current = nickname;
  const grantRef = useRef(grant);
  grantRef.current = grant;
  const clientKindRef = useRef(clientKind);
  clientKindRef.current = clientKind;
  const voiceEventsRef = useRef(voiceEvents);
  voiceEventsRef.current = voiceEvents;

  // A stable identity for the target so a re-created object with the same
  // content does not re-dial.
  const targetKey = target === null ? '' : JSON.stringify(target);
  const targetRef = useRef(target);
  targetRef.current = target;

  // The grant is the same kind of thing: content identity, and a CHANGE is a
  // deliberate re-dial (a server change already is one). It has to be — the
  // grant rides RoomHello, so a secret supplied inside the room cannot travel
  // as a command (see RoomView's gated-out state).
  const grantKey = grant === null ? '' : JSON.stringify(grant);

  useEffect(() => {
    const store = useRoomStore.getState();
    store.reset();
    const t = targetRef.current;
    if (t === null) return;
    store.setStatus('connecting');
    // Dial a macrotask later: StrictMode's development mount → cleanup →
    // mount would otherwise reach the relay twice, and a room mint that
    // arrives twice is refused the second time.
    let session: RoomSession | null = null;
    const dial = setTimeout(() => {
      const s = new RoomSession(
        {
          serverUrl,
          certHashHex,
          target: t,
          nickname: nicknameRef.current,
          clientKind: clientKindRef.current,
          grant: grantRef.current,
          // R74 (docs/76 item 43.7): lr=1 while this tab would follow an offer.
          followOffers: willFollowOffers(),
          // R79 (docs/80 §11.1 C2): ask for the voice roster where this
          // browser could play voice at all.
          wantVoice: voicePlaybackSupported(),
        },
        {
          onConnected: () => {},
          onLocalRelay: (ev) => noteLocalRelay(ev, 'room'),
          onState: (state) => useRoomStore.getState().replaceState(state),
          onEvent: (ev) => {
            if (ev.kind === ROOM_EVENT_COMMAND_REJECTED && ev.command === ROOM_COMMAND_VOICE_JOIN) {
              voiceEventsRef.current?.onRejected(ev.reason, ev.message);
              return;
            }
            useRoomStore.getState().applyEvent(ev);
          },
          onVoiceTicket: (url, ticket) => voiceEventsRef.current?.onTicket(url, ticket),
          onReconnecting: (info) => {
            log.info('room reconnecting:', info.reason);
            useRoomStore
              .getState()
              .setStatus(
                'reconnecting',
                info.closeCode === CLOSE_CODE_SERVER_DRAINING ? DRAINING_NOTE : RECONNECTING_NOTE,
              );
          },
          onEnded: (reason) => useRoomStore.getState().setEnded(reason),
          onError: (err) => {
            log.error(`room session failed (${err.kind}):`, err.message);
            useRoomStore.getState().setError(err.kind, err.message);
            leaveLocalRelayAfter(err.kind);
          },
        },
      );
      session = s;
      sessionRef.current = s;
      s.start().catch((err) => {
        if (sessionRef.current !== s) return;
        log.error(`room session failed (${err.kind}):`, err.message);
        useRoomStore.getState().setError(err.kind, err.message);
        leaveLocalRelayAfter(err.kind);
      });
    }, 0);
    return () => {
      clearTimeout(dial);
      if (session !== null) {
        if (sessionRef.current === session) sessionRef.current = null;
        session.stop();
      }
      // The store outlives this screen; a later room view must not render
      // (and dial the tiles of) this room before its own session resets it.
      useRoomStore.getState().reset();
    };
    // targetKey / grantKey stand in for `target` and `grant` (content
    // identity, see above).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetKey, grantKey, dialNonce, serverUrl, certHashHex]);

  const attach = useCallback((id: string, token: string, label: string) => {
    sessionRef.current?.attach(id, token, label);
  }, []);
  const detach = useCallback((id: string) => sessionRef.current?.detach(id), []);
  const setNickname = useCallback((n: string) => sessionRef.current?.setNickname(n), []);
  const endRoom = useCallback(() => sessionRef.current?.endRoom(), []);
  const voiceJoin = useCallback(() => sessionRef.current?.sendVoiceJoin() ?? false, []);

  // One stable object: the room screen's attach effect depends on it.
  return useMemo(
    () => ({ attach, detach, setNickname, endRoom, voiceJoin }),
    [attach, detach, setNickname, endRoom, voiceJoin],
  );
}
