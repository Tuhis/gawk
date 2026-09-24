// R42: the React glue between RoomSession (transport) and the room store.
// One session per mount, keyed on the target; every callback lands in the
// store, and the hook hands back the commands. The session dials on mount
// (an explicit event — CODE-REVIEW's effect rule), never on re-render: the
// nickname is read at mount through a ref and a later change travels as a
// command (setNickname), while the target and the grant are dial inputs — a
// change of either is a deliberate re-dial, keyed on content below.

import { useCallback, useEffect, useMemo, useRef } from 'react';
import { RoomSession, type RoomSessionGrant, type RoomTarget } from '../../transport/room-session';
import { CLOSE_CODE_SERVER_DRAINING } from '../../transport/wire';
import { useTransportStore } from '../../state/transportStore';
import { useRoomStore } from '../../state/roomStore';
import { log } from '../../lib/logger';
import { DRAINING_NOTE, RECONNECTING_NOTE } from './roomCopy';

export interface RoomCommands {
  attach: (broadcastId: string, resumeTokenHex: string, label: string) => void;
  detach: (broadcastId: string) => void;
  setNickname: (nickname: string) => void;
  endRoom: () => void;
  // The code the session is joined to (a mint learns it from the first state).
  code: () => string | null;
}

export interface UseRoomSessionArgs {
  target: RoomTarget | null; // null ⇒ no session (e.g. waiting for a nickname)
  nickname: string;
  clientKind: number;
  grant: RoomSessionGrant | null;
  // Bump to force a re-dial with an unchanged target and grant — the "try
  // that key again" case (docs/44 D8). Any change re-dials; the value itself
  // means nothing.
  dialNonce?: number;
}

export function useRoomSession({ target, nickname, clientKind, grant, dialNonce = 0 }: UseRoomSessionArgs): RoomCommands {
  // Same subscription discipline as useViewerConnection: a server change is
  // a deliberate re-dial, and the room's tiles re-dial with it.
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const sessionRef = useRef<RoomSession | null>(null);
  const nicknameRef = useRef(nickname);
  nicknameRef.current = nickname;
  const grantRef = useRef(grant);
  grantRef.current = grant;
  const clientKindRef = useRef(clientKind);
  clientKindRef.current = clientKind;

  // A stable identity for the target so a re-created object with the same
  // content does not re-dial.
  const targetKey = target === null ? '' : JSON.stringify(target);
  const targetRef = useRef(target);
  targetRef.current = target;

  // The grant is the same kind of thing: content identity, and a CHANGE is a
  // deliberate re-dial (a server change already is one). It has to be — the
  // grant rides RoomHello, so a secret supplied inside the room cannot travel
  // as a command (docs/44 D8; RoomView's gated-out state).
  const grantKey = grant === null ? '' : JSON.stringify(grant);

  useEffect(() => {
    const store = useRoomStore.getState();
    store.reset();
    const t = targetRef.current;
    if (t === null) return;
    store.setStatus('connecting');
    const session = new RoomSession(
      {
        serverUrl,
        certHashHex,
        target: t,
        nickname: nicknameRef.current,
        clientKind: clientKindRef.current,
        grant: grantRef.current,
      },
      {
        onConnected: () => {},
        onState: (state) => useRoomStore.getState().replaceState(state),
        onEvent: (ev) => useRoomStore.getState().applyEvent(ev),
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
        },
      },
    );
    sessionRef.current = session;
    session.start().catch((err) => {
      if (sessionRef.current !== session) return;
      log.error(`room session failed (${err.kind}):`, err.message);
      useRoomStore.getState().setError(err.kind, err.message);
    });
    return () => {
      if (sessionRef.current === session) sessionRef.current = null;
      session.stop();
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
  const code = useCallback(() => sessionRef.current?.code ?? null, []);

  // One stable object: the room screen's attach effect depends on it.
  return useMemo(() => ({ attach, detach, setNickname, endRoom, code }), [attach, detach, setNickname, endRoom, code]);
}
