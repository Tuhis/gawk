// R33 (docs/83 §5.3 option D, OD9): the way in to the OBS page, a footnote
// under the broadcaster's pre-start card and outside its glass. It is for
// people who already know what OBS is; the card itself stays as it is (U1).
//
// A returning user (U6): with a key saved for this server the line names its
// code, and when the one status read on mount says OBS is live, it becomes a
// glass pill with the LIVE badge. That read happens once, on mount, and only
// when a key is saved (U7).
import { useEffect, useState } from 'react';
import b from '../broadcaster/broadcaster.module.css';
import s from './obs.module.css';
import { getWhipStatusUrl } from '../../config';
import { hashWithPathKeepingQuery } from '../../routing';
import { AdvancedChip } from './AdvancedChip';
import { loadObsKey } from './key';
import { readWhipStatus } from './status';

export function ObsWayIn({ serverUrl }: { serverUrl: string }) {
  const saved = loadObsKey(serverUrl);
  // The key the mount's read was for: a live answer only counts for it.
  const [live, setLive] = useState<{ key: string } | null>(null);
  // The server the page mounted on: the one read is for its key.
  const [mountServer] = useState(serverUrl);

  useEffect(() => {
    const key = loadObsKey(mountServer);
    const statusUrl = getWhipStatusUrl();
    if (key === null || statusUrl === '') return;
    const ctrl = new AbortController();
    void readWhipStatus(statusUrl, key.key, ctrl.signal).then((read) => {
      if (ctrl.signal.aborted) return;
      if (read.kind === 'ok' && read.status.state === 'live') setLive({ key: key.key });
    });
    return () => ctrl.abort();
    // Mount only (mountServer never changes): one read per visit to the
    // broadcaster page (U7). A server switch hides the way in instead.
  }, [mountServer]);

  const href = hashWithPathKeepingQuery(window.location.hash, '/broadcast/obs');

  if (saved !== null && live !== null && live.key === saved.key) {
    return (
      <p className={s.wayInLive} data-testid="obs-way-in">
        <span className={b.liveBadge}>
          <span className={b.liveDot} aria-hidden="true" />
          LIVE
        </span>
        <span>
          OBS is streaming on <code>{saved.broadcastId}</code> ·{' '}
          <a href={href} className={s.link}>
            Open
          </a>
        </span>
      </p>
    );
  }
  if (saved !== null) {
    return (
      <p className={s.wayIn} data-testid="obs-way-in">
        <span>
          Your OBS key for <code>{saved.broadcastId}</code> is saved ·{' '}
          <a href={href} className={s.link}>
            Open
          </a>
        </span>
        <AdvancedChip />
      </p>
    );
  }
  return (
    <p className={s.wayIn} data-testid="obs-way-in">
      <span>
        Already stream with OBS Studio?{' '}
        <a href={href} className={s.link}>
          Stream from OBS
        </a>
      </span>
      <AdvancedChip />
    </p>
  );
}
