import { Fragment } from 'react';

import type { SessionView } from '../api/types.ts';
import type { HistoryMap } from '../lib/history.ts';
import { ago, bitrate, EMPTY, fps, ms, shortId } from '../lib/format.ts';
import { freshnessTone } from '../lib/severity.ts';
import { useUiStore } from '../state/uiStore.ts';
import { SeverityBadge } from './SeverityBadge.tsx';
import { SessionTimeline } from './SessionTimeline.tsx';
import styles from './SessionTable.module.css';

interface Props {
  sessions: SessionView[];
  history: HistoryMap;
}

function dipCell(m: Record<string, number> | undefined): string {
  if (!m || m.fpsDipEpisodes === undefined) return EMPTY;
  if (!m.fpsDipEpisodes) return '0';
  const worst = m.fpsDipWorstFps === undefined ? '' : ` ↓${fps(m.fpsDipWorstFps)}`;
  return `${m.fpsDipEpisodes}×${worst}`;
}

/** The two freshness readings, side by side and separately toned. */
function Freshness({ state, ageMs }: { state: string; ageMs: number }) {
  const tone = freshnessTone(state);
  const label = state === 'unknown' ? 'never' : ago(ageMs);
  return (
    <span className={`${styles.fresh} ${styles[tone]}`} title={state}>
      {label}
    </span>
  );
}

export function SessionTable({ sessions, history }: Props) {
  const openTimelines = useUiStore((s) => s.openTimelines);
  const toggleTimeline = useUiStore((s) => s.toggleTimeline);

  return (
    <table className={styles.table}>
      <colgroup>
        <col className={styles.cState} />
        <col className={styles.cRole} />
        <col className={styles.cSession} />
        <col className={styles.cClient} />
        <col className={styles.cNum} />
        <col className={styles.cNum} />
        <col className={styles.cNum} />
        <col className={styles.cDelivery} />
        <col className={styles.cFresh} />
        <col className={styles.cVerdict} />
        <col className={styles.cChart} />
      </colgroup>
      <thead>
        <tr>
          <th>state</th>
          <th>role</th>
          <th>session</th>
          <th>client</th>
          <th className={styles.right}>fps</th>
          <th className={styles.right}>dips</th>
          <th className={styles.right}>latency</th>
          <th>delivery</th>
          <th>seen c / r</th>
          <th>verdict</th>
          <th aria-label="timeline" />
        </tr>
      </thead>
      <tbody>
        {sessions.map((s) => {
          const m = s.metrics ?? {};
          const cfg = s.config ?? {};
          const isBroadcaster = s.role === 'broadcaster';
          const open = !!openTimelines[s.sessionId];
          return (
            <Fragment key={s.sessionId}>
              <tr className={open ? styles.rowOpen : undefined}>
                <td>
                  <SeverityBadge severity={s.severity} />
                </td>
                <td className={isBroadcaster ? styles.roleBroadcaster : styles.role}>{s.role}</td>
                <td className={styles.mono}>{shortId(s.sessionId)}</td>
                <td className={styles.dim}>
                  {[s.browser, s.os].filter(Boolean).join(' / ') || EMPTY}
                </td>
                <td className={`${styles.right} tnum`}>
                  {isBroadcaster
                    ? `${fps(m.captureFps)} → ${fps(m.encoderFps)}${
                        m.targetFps === undefined ? '' : ` / ${fps(m.targetFps)}`
                      }`
                    : `${fps(m.receivedFps)} → ${fps(m.decoderFps)}`}
                </td>
                <td className={`${styles.right} tnum`}>{dipCell(s.metrics)}</td>
                <td className={`${styles.right} tnum`}>
                  {isBroadcaster ? bitrate(m.targetBitrateBps) : ms(m.capToRenderMs)}
                </td>
                <td className={styles.dim}>{cfg.deliveryMode ?? cfg.acceleration ?? EMPTY}</td>
                <td>
                  <Freshness state={s.clientState} ageMs={s.clientAgeMs} />
                  <span className={styles.sep}>/</span>
                  <Freshness state={s.relayState} ageMs={s.relayAgeMs} />
                </td>
                <td className={styles.verdict} title={s.verdict}>
                  {s.verdict || EMPTY}
                </td>
                <td>
                  <button
                    type="button"
                    className={styles.chartBtn}
                    aria-expanded={open}
                    onClick={() => toggleTimeline(s.sessionId)}
                    title={open ? 'Hide timeline' : 'Show the last 10 minutes'}
                  >
                    {open ? '▾' : '▸'} graph
                  </button>
                </td>
              </tr>
              {open && (
                <tr className={styles.timelineRow}>
                  <td colSpan={11}>
                    <SessionTimeline session={s} history={history[s.sessionId]} />
                  </td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </tbody>
    </table>
  );
}
