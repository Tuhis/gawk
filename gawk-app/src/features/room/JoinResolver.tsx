import { useEffect, useState } from 'react';
import styles from './room.module.css';
import { GlassPanel } from '../../ui/GlassPanel';
import { RoomSession } from '../../transport/room-session';
import { ROOM_CLIENT_WEB_VIEWER } from '../../transport/wire';
import { useTransportStore } from '../../state/transportStore';
import { relayQuerySuffix } from '../../lib/shareLink';
import { log } from '../../lib/logger';

// R42 (docs/44 D19): `#/join/<code>` — a typed six-character code that is
// either a room or a broadcast. The relay is HTTP/3-only, so there is no
// dependable fetch to ask; a WebTransport CONNECT to /room/{code} is the
// probe. A RoomState means "room": hop to #/room/<code>. A refusal (404 for
// a broadcast code, or anything else the browser hides) means "not a room":
// hop to #/view/<code>, where the viewer says "streamer offline" if it is
// nothing at all. One extra handshake, paid only by the join box.
//
// The probe session is closed before the hop: the room screen opens its own
// (with the remembered nickname); the empty grace covers the gap.
export function JoinResolver({ code }: { code: string }) {
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore((s) => s.certHashHex);
  const [note, setNote] = useState<string | null>(null);

  useEffect(() => {
    let active = true;
    const suffix = relayQuerySuffix();
    const go = (hash: string) => {
      if (!active) return;
      active = false;
      // replace(), not assignment: Back must not land on the resolver.
      window.location.replace(`${window.location.pathname}${window.location.search}${hash}${suffix}`);
    };
    const session = new RoomSession(
      { serverUrl, certHashHex, target: { kind: 'join', code }, nickname: '', clientKind: ROOM_CLIENT_WEB_VIEWER },
      {
        onConnected: () => {},
        onState: () => {
          session.stop();
          go(`#/room/${code}`);
        },
        onEvent: () => {},
        onReconnecting: () => {},
        onEnded: () => go(`#/view/${code}`),
        onError: () => go(`#/view/${code}`),
      },
    );
    session.start().catch((e: unknown) => {
      log.info('join probe: not a room —', e instanceof Error ? e.message : String(e));
      go(`#/view/${code}`);
    });
    // A relay that upgrades but never answers: fall through to the viewer
    // rather than spinning forever.
    const guard = setTimeout(() => {
      if (!active) return;
      setNote('Taking longer than usual…');
      session.stop();
      go(`#/view/${code}`);
    }, 8000);
    return () => {
      active = false;
      clearTimeout(guard);
      session.stop();
    };
  }, [code, serverUrl, certHashHex]);

  return (
    <div className={styles.root}>
      <div className={styles.center}>
        <GlassPanel className={styles.card}>
          <div className={styles.spinner} aria-hidden="true" />
          <p className={styles.cardText}>Looking up {code}…</p>
          {note && <p className={styles.cardText}>{note}</p>}
        </GlassPanel>
      </div>
    </div>
  );
}
