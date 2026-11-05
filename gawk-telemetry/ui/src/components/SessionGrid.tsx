import type { HistoryRow } from '../api/types.ts';
import type { Columns } from '../lib/columns.ts';
import { compactTime, dur, EMPTY, shortId } from '../lib/format.ts';
import { href } from '../router/router.ts';
import { Grip } from './Columns.tsx';
import { SeverityBadge } from './SeverityBadge.tsx';
import { client, HISTORY_COLUMNS } from './sessionGridColumns.ts';
import styles from './SessionGrid.module.css';

// The history row (TH3), shared by the history browser and the room view
// (R42) — one row shape, one renderer, one set of widths per table.
//
// It is a grid rather than a <table> because the history browser virtualizes
// it and a <tr> cannot be absolutely positioned. R31 gave the header and the
// rows the same hardcoded track list (`24px 15ch 7ch …`), but the header sat
// outside the scroller, so its `fr` tracks resolved against a wider box and
// every header drifted 20–30 px left of its column. R36 (docs/86 TU2): the
// tracks are measured pixels except the verdict, the header shares the rows'
// horizontal scroll, and only the verdict ellipsises.

/** The header row, with a grip on every boundary but the last. */
export function GridHead({ columns }: { columns: Columns }) {
  return (
    <div className={styles.head} style={{ gridTemplateColumns: columns.template }} role="row">
      {HISTORY_COLUMNS.map((c) => (
        <span
          key={c.key}
          role="columnheader"
          className={`${styles.th} ${c.key === 'duration' || c.key === 'stalls' ? styles.r : ''}`}
        >
          {c.label}
          {!c.flex && <Grip columns={columns} col={c} />}
        </span>
      ))}
    </div>
  );
}

/** One history row as a link to its session. */
export function GridRow({ row, columns }: { row: HistoryRow; columns: Columns }) {
  return (
    <a className={styles.row} style={{ gridTemplateColumns: columns.template }} href={href('session', row.sessionId)}>
      <span className={styles.td}>
        <SeverityBadge severity={row.severity} />
      </span>
      <span className={styles.td}>{compactTime(row.startedAtMs)}</span>
      <span className={`${styles.td} ${styles.r}`}>{dur(row.durationMs)}</span>
      <span className={`${styles.td} ${row.role === 'broadcaster' ? styles.strong : styles.dim}`}>{row.role}</span>
      <span className={`${styles.td} ${styles.id}`}>
        {shortId(row.sessionId)}
        {/* UD10's boundary, drawn on the row itself: this one has a permanent
            verdict and no samples left behind it. */}
        {row.rollupOnly && (
          <span className={styles.tag} title="Raw samples pruned — the rollup and its verdict remain">
            rollup
          </span>
        )}
        {row.live && (
          <span className={styles.live} title="Still running">
            <i aria-hidden />
            live
          </span>
        )}
      </span>
      <span className={styles.td} title={client(row)}>
        {row.browser ?? EMPTY}
        {row.os ? <span className={styles.dimmer}> · {row.os}</span> : null}
      </span>
      <span className={`${styles.td} ${styles.mono} ${styles.dim}`}>{row.appVersion ?? EMPTY}</span>
      <span className={`${styles.td} ${styles.r}`}>{row.stalls}</span>
      <span className={`${styles.td} ${styles.verdict}`} title={row.distrust || row.verdict}>
        {row.distrust ? `⚠ ${row.distrust}` : row.verdict || EMPTY}
      </span>
    </a>
  );
}
