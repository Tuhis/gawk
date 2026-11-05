import { Fragment, useMemo } from 'react';

import type { LegView, SessionView, Sparks } from '../api/types.ts';
import { membershipKey, useColumns, useWidth, type ColumnSpec } from '../lib/columns.ts';
import { ago, bitrate, EMPTY, fps, ms, shortId } from '../lib/format.ts';
import { freshnessTone } from '../lib/severity.ts';
import { href } from '../router/router.ts';
import { DragLine, Grip, WidthsNote } from './Columns.tsx';
import { SeverityBadge } from './SeverityBadge.tsx';
import { Sparkline } from './Sparkline.tsx';
import t from './tables.module.css';
import styles from './SessionTable.module.css';

interface Props {
  sessions: SessionView[];
  sparks: Sparks | null;
}

// The live session table. R31 made its last column a LINK to the session's own
// page — every sample since the session started, from disk — instead of an
// inline ten-minute client-side graph (docs/36 §1.1).
//
// R36 (docs/86):
//   * TU2 — widths are measured from the rows when the set of sessions
//     changes, never as values tick, and every header has a grip. R31's
//     `table-layout: fixed` with percentage widths cut the client and the
//     verdict off with no way to widen them.
//   * TU7 — a 2-minute frame-rate sparkline per row, as UD14 promised.
//   * TU5 — a striped viewer is ONE row with its legs as sub-rows beneath:
//     the legs carry its deltas, so its own row reads near-idle without them.
//   * TU6 — a viewer served by a PoP says so.

function dipCell(m: Record<string, number> | undefined): string {
  if (!m || m.fpsDipEpisodes === undefined) return EMPTY;
  if (!m.fpsDipEpisodes) return '0';
  const worst = m.fpsDipWorstFps === undefined ? '' : ` ↓${fps(m.fpsDipWorstFps)}`;
  return `${m.fpsDipEpisodes}×${worst}`;
}

function fpsCell(s: SessionView): string {
  const m = s.metrics ?? {};
  return s.role === 'broadcaster'
    ? `${fps(m.captureFps)} → ${fps(m.encoderFps)}${m.targetFps === undefined ? '' : ` / ${fps(m.targetFps)}`}`
    : `${fps(m.receivedFps)} → ${fps(m.decoderFps)}`;
}

function latencyCell(s: SessionView): string {
  const m = s.metrics ?? {};
  return s.role === 'broadcaster' ? bitrate(m.targetBitrateBps) : ms(m.capToRenderMs);
}

function clientCell(s: SessionView): string {
  return [s.browser, s.os].filter(Boolean).join(' / ') || EMPTY;
}

function deliveryCell(s: SessionView): string {
  const cfg = s.config ?? {};
  return cfg.deliveryMode ?? cfg.acceleration ?? EMPTY;
}

// Samples are the widest a ticking cell can get, so a value growing from
// "9 ms" to "142 ms" never re-flows the row (TH11). The pill is uppercase and
// semibold where the canvas measures sentence case, hence its `extra`.
const COLUMNS: ColumnSpec<SessionView>[] = [
  { key: 'state', label: 'state', samples: ['? UNKNOWN'], extra: 22 },
  // Broadcaster renders at weight 500; the canvas measures at 400.
  { key: 'role', label: 'role', text: (s) => s.role, samples: ['broadcaster'], extra: 8 },
  {
    key: 'session',
    label: 'session',
    // The tags are sans at 10.5 px beside a mono id: measured as sans text,
    // with room for the tags' own padding.
    text: (s) => shortId(s.sessionId) + (s.legs?.length ? ' striped ×3 · 2 pods' : '') + (s.pop ? ' PoP hel1' : ''),
    extra: 28,
  },
  { key: 'client', label: 'client', text: clientCell, max: 220, squeeze: true, squeezeMin: 110 },
  { key: 'fps', label: 'fps', text: fpsCell, samples: ['144 → 144 / 144'] },
  { key: 'spark', label: 'last 2 min', samples: ['x'], min: 120, max: 120 },
  { key: 'dips', label: 'dips', text: (s) => dipCell(s.metrics), samples: ['99× ↓60'] },
  { key: 'latency', label: 'latency', text: latencyCell, samples: ['99.9 Mbps', '9999 ms'] },
  { key: 'delivery', label: 'delivery', text: deliveryCell, max: 120 },
  { key: 'seen', label: 'seen client / relay', samples: ['59m ago / never'] },
  { key: 'verdict', label: 'verdict', flex: true, flexMin: 160 },
  { key: 'detail', label: '', samples: ['detail →'], min: 76 },
];

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

export function SessionTable({ sessions, sparks }: Props) {
  // A new LOAD is a change in WHO is on the table — a joiner, a leaver — never
  // a value ticking. Widths are measured then, and frozen in between.
  const loadKey = useMemo(() => membershipKey(sessions.map((s) => s.sessionId)), [sessions]);
  const [frame, width] = useWidth();
  const columns = useColumns('live-sessions', COLUMNS, sessions, loadKey, width);

  return (
    <div className={t.wrap} ref={frame}>
      <table className={t.tbl} style={{ minWidth: columns.minTotal }}>
        <colgroup>
          {COLUMNS.map((c) => (
            <col key={c.key} style={c.flex ? undefined : { width: columns.widths[c.key] }} />
          ))}
        </colgroup>
        <thead>
          <tr>
            {COLUMNS.map((c) => (
              <th key={c.key} className={['fps', 'dips', 'latency'].includes(c.key) ? t.r : undefined}>
                {c.label}
                {!c.flex && c.key !== 'detail' && <Grip columns={columns} col={c} />}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <Fragment key={s.sessionId}>
              <Row s={s} spark={sparks?.sessions[s.sessionId]?.values} />
              {(s.legs ?? []).map((leg) => (
                <LegRow key={`${leg.member}-${leg.pod}`} leg={leg} />
              ))}
            </Fragment>
          ))}
        </tbody>
      </table>
      <DragLine columns={columns} cols={COLUMNS} />
      {columns.overridden.length > 0 && (
        <div className={`${t.foot} ${styles.foot}`}>
          <span className={styles.grow} />
          <WidthsNote columns={columns} cols={COLUMNS} />
        </div>
      )}
    </div>
  );
}

function Row({ s, spark }: { s: SessionView; spark?: Array<number | null> }) {
  const isBroadcaster = s.role === 'broadcaster';
  const pods = new Set((s.legs ?? []).map((l) => l.pod)).size;
  return (
    <tr>
      <td>
        <SeverityBadge severity={s.severity} />
      </td>
      <td className={isBroadcaster ? styles.roleBroadcaster : t.dim}>{s.role}</td>
      <td>
        <span className={t.mono} title={s.sessionId}>
          {shortId(s.sessionId)}
        </span>
        {s.legs?.length ? (
          <span
            className={styles.tagAccent}
            title={`One viewer carried on 1 + ${s.legs.length} relay subscribers: its deltas ride the legs below, so its own row reads near-idle.`}
          >
            striped ×{s.legs.length}
            {pods > 1 ? ` · ${pods} pods` : ''}
          </span>
        ) : null}
        {s.pop ? (
          <span className={styles.tagAccent} title="Served by a point of presence: its relay-side figures are on the PoP.">
            PoP {s.pop}
          </span>
        ) : null}
      </td>
      <td className={t.dim} title={clientCell(s)}>
        {clientCell(s)}
      </td>
      <td className={t.r}>{fpsCell(s)}</td>
      <td>
        <Sparkline values={spark} label={`${s.role} ${shortId(s.sessionId)} frame rate, last two minutes`} />
      </td>
      <td className={t.r}>{dipCell(s.metrics)}</td>
      <td className={t.r}>{latencyCell(s)}</td>
      <td className={t.dim} title={deliveryCell(s)}>
        {deliveryCell(s)}
      </td>
      <td>
        <Freshness state={s.clientState} ageMs={s.clientAgeMs} />
        <span className={styles.sep}>/</span>
        <Freshness state={s.relayState} ageMs={s.relayAgeMs} />
      </td>
      <td className={`${t.trunc} ${styles.verdict}`} title={s.verdict}>
        {s.verdict || EMPTY}
      </td>
      <td>
        <a href={href('session', s.sessionId)} title="Everything known about this session, from disk — from its first sample">
          detail →
        </a>
      </td>
    </tr>
  );
}

/** One stripe leg under its viewer: plumbing, so it never counts as a viewer. */
function LegRow({ leg }: { leg: LegView }) {
  return (
    <tr className={styles.legRow}>
      <td />
      <td colSpan={2}>
        <span className={styles.legLine} aria-hidden />
        leg {leg.member + 1} / {leg.n}
      </td>
      <td className={t.mono} colSpan={2} title={`${leg.pod} (${leg.role})`}>
        {leg.pod} · {leg.role}
      </td>
      <td />
      <td className={t.r} colSpan={2} title="Datagrams waiting in this leg's relay queue">
        queue {leg.queueDepth}
      </td>
      <td
        colSpan={2}
        className={leg.datagramsDropped ? styles.legDrop : undefined}
        title="The relay's drops for this leg over its last scrape window"
      >
        {leg.datagramsDropped === undefined ? 'drops: not yet scraped twice' : `${leg.datagramsDropped.toLocaleString()} dropped`}
      </td>
      <td colSpan={2} className={styles.legNote}>
        seen {ago(leg.relayAgeMs)}
      </td>
    </tr>
  );
}
