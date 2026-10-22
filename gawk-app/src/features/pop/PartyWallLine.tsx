// A broadcaster on a PoP (docs/76 §7.3): "Live on Assembly 2026's local
// relay", and on a PoP with a party wall "On the party wall · Hide" (or
// "Hidden from the party wall · Show"). The state comes from the publish
// session's LocalRelayInfo (0x18), then follows this tab's own Hide / Show.
// Copy is provisional until PP1.

import { useState } from 'react';

import { sanitizeIdentityName } from '../servers/probe';
import { selectCertHashList, useTransportStore } from '../../state/transportStore';
import { usePopStore } from './popStore';
import { sendWallCommand, setWallHidden, type WallCommand } from './wall';

interface Props {
  broadcastId: string | null;
  resumeTokenHex: string | null;
  label: string;
  className?: string;
  // Test seam.
  send?: (cmd: WallCommand) => Promise<boolean>;
}

export function PartyWallLine({ broadcastId, resumeTokenHex, label, className, send = sendWallCommand }: Props) {
  const serverUrl = useTransportStore((s) => s.serverUrl);
  const certHashHex = useTransportStore(selectCertHashList);
  const record = usePopStore((s) => s.info);
  // This tab's own Hide / Show, until the PoP's next 0x18 (it sends one
  // whenever the wall state changes) says what it actually did.
  const [own, setOwn] = useState<{ after: unknown; onWall: boolean } | null>(null);
  const [busy, setBusy] = useState(false);
  if (record === null || record.relayUrl !== serverUrl) return null;
  const { info } = record;
  const name = sanitizeIdentityName(info.name) || 'the party';
  const wallCode = info.wallCode;
  const onWall = own !== null && own.after === info ? own.onWall : info.onWall;

  const toggle = async () => {
    if (!broadcastId || !resumeTokenHex || busy) return;
    const hide = onWall;
    setBusy(true);
    const ok = await send({ serverUrl, certHashHex, wallCode, broadcastId, resumeTokenHex, label, hide });
    setBusy(false);
    if (!ok) return;
    setWallHidden(wallCode, hide);
    setOwn({ after: info, onWall: !hide });
  };

  return (
    <span className={className} data-testid="party-wall-line">
      <span>Live on {name}’s local relay</span>
      {wallCode !== '' && (
        <span>
          {' · '}
          {onWall ? 'On the party wall' : 'Hidden from the party wall'}
          {' · '}
          <button type="button" onClick={() => void toggle()} disabled={busy || !broadcastId || !resumeTokenHex}>
            {onWall ? 'Hide' : 'Show'}
          </button>
        </span>
      )}
    </span>
  );
}
