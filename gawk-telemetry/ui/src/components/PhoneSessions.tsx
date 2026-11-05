import { useState } from 'react';

import type { SessionView, Sparks } from '../api/types.ts';
import { EMPTY, fps, ms, shortId } from '../lib/format.ts';
import { rank } from '../lib/severity.ts';
import { href } from '../router/router.ts';
import { SeverityBadge } from './SeverityBadge.tsx';
import { Sparkline } from './Sparkline.tsx';
import styles from './PhoneSessions.module.css';

/**
 * A live card's sessions on a phone (UD17, R36 TU11): one tappable row each,
 * worst first, with the healthy rest folded into one line. The full table
 * needs a desktop's width to be read honestly; this is triage — what is
 * wrong, with whom, and a way into the session's own page.
 */
export function PhoneSessions({ sessions, sparks }: { sessions: SessionView[]; sparks: Sparks | null }) {
  const [all, setAll] = useState(false);
  const sorted = [...sessions].sort((a, b) => rank(b.severity) - rank(a.severity));
  const loud = sorted.filter((s) => s.severity === 'bad' || s.severity === 'warn');
  const shown = all || loud.length === 0 ? sorted : loud;
  const rest = sorted.length - shown.length;

  return (
    <div className={styles.list}>
      {shown.map((s) => {
        const m = s.metrics ?? {};
        const isB = s.role === 'broadcaster';
        const rate = isB ? `${fps(m.captureFps)} → ${fps(m.encoderFps)} fps` : `${fps(m.receivedFps)} → ${fps(m.decoderFps)} fps`;
        const facts = [
          rate,
          m.fpsDipEpisodes ? `${m.fpsDipEpisodes} dip${m.fpsDipEpisodes === 1 ? '' : 's'}` : '',
          s.legs?.length ? `striped ×${s.legs.length}` : '',
          !isB && m.capToRenderMs !== undefined ? ms(m.capToRenderMs) : '',
        ].filter(Boolean);
        return (
          <a key={s.sessionId} className={styles.row} href={href('session', s.sessionId)}>
            <SeverityBadge severity={s.severity} />
            <span className={styles.body}>
              <span>
                <span className={styles.id}>{shortId(s.sessionId)}</span>{' '}
                <span className={styles.who}>
                  {s.role} · {s.browser ?? EMPTY}
                </span>
              </span>
              <span className={styles.facts}>{facts.join(' · ')}</span>
            </span>
            <Sparkline values={sparks?.sessions[s.sessionId]?.values} width={56} height={20} />
          </a>
        );
      })}
      {rest > 0 && (
        <button type="button" className={styles.more} onClick={() => setAll(true)}>
          {rest} more session{rest === 1 ? '' : 's'}, all ok or unknown
        </button>
      )}
    </div>
  );
}
