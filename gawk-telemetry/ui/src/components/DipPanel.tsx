import { Fragment, useState } from 'react';

import type { DipExplanation, DipReport, Mover } from '../api/types.ts';
import { absoluteTime, compactTime, confidence, dur, withUnit } from '../lib/format.ts';
import styles from './DipPanel.module.css';
import view from '../views/view.module.css';

// TH9 / UD21: a dip, explained.
//
// The point is turning *"your fps dipped"* into *"and keyframe drops went 0 → 7
// while ingress loss went 0 → 1.2 %"*. Every number here is a projection of
// what was already measured; nothing is inferred.
//
// **The wording rule is enforced on the server**, in `correlate()`, and that is
// deliberate: two viewers dipping together is evidence about a shared leg, not
// proof of one, and a rule about phrasing that lived in a component would be
// re-litigated by every future component. This renders the sentence it is
// given, with its confidence beside it, and adds nothing.
//
// R36 (docs/86 TU8): ONE ROW PER EPISODE. R31 expanded every episode with all
// of its movers, which on a real session (13 episodes × ~15 movers) put the
// notes ~5 900 px down the page. A row shows when, how long, how deep, the
// three largest movers and who else dipped; opening it shows everything.

const TOP_MOVERS = 3;

export function DipTable({ report }: { report: DipReport }) {
  const [open, setOpen] = useState<number | null>(null);
  const first = report.episodes[0];

  if (report.episodes.length === 0) {
    return (
      <section className={view.panel} aria-label="Dips">
        <div className={view.panelH}>
          <h2 className={view.panelT}>Dips</h2>
        </div>
        {/* Never a blank panel: "we looked and it was steady" and "we could not
            look" are different facts, and the server says which. */}
        <p className={`${view.note} ${view.panelB}`}>{report.note ?? 'No dip episodes in this session.'}</p>
      </section>
    );
  }

  return (
    <section className={view.panel} aria-label="Dips">
      <div className={view.panelH}>
        <h2 className={view.panelT}>Dips</h2>
        <span className={view.note}>
          {report.episodes.length} collapse{report.episodes.length === 1 ? '' : 's'} of {report.primary} below half
          this session’s own baseline{first ? ` (${first.baseline.toFixed(0)})` : ''} · one row each, open one to see
          everything that moved
        </span>
      </div>
      <div className={styles.wrap}>
        <table className={styles.dips}>
          <thead>
            <tr>
              <th aria-label="open" />
              <th>at</th>
              <th className={styles.r}>for</th>
              <th className={styles.r}>down to</th>
              <th>largest movers</th>
              <th>others on the broadcast</th>
            </tr>
          </thead>
          <tbody>
            {report.episodes.map((ep, i) => (
              <Fragment key={ep.fromMs}>
                <tr className={open === i ? styles.open : undefined}>
                  <td>
                    <button
                      type="button"
                      className={styles.disc}
                      aria-expanded={open === i}
                      aria-label={open === i ? 'Hide what moved' : 'Show everything that moved'}
                      onClick={() => setOpen(open === i ? null : i)}
                    >
                      {open === i ? '▾' : '▸'}
                    </button>
                  </td>
                  <td className={styles.mono} title={absoluteTime(ep.fromMs)}>
                    {compactTime(ep.fromMs)}
                  </td>
                  <td className={styles.r}>{dur(ep.durationMs)}</td>
                  <td className={styles.r}>
                    {ep.worstValue.toFixed(1)}
                    <span className={view.dimmer}> / {ep.baseline.toFixed(0)}</span>
                  </td>
                  <td className={styles.movers}>
                    {(ep.movers ?? []).slice(0, TOP_MOVERS).map((m) => (
                      <span key={`${m.provenance}-${m.signal}`} className={styles.mv}>
                        <b>{m.signal}</b>
                        {moverText(m)}
                      </span>
                    ))}
                    {!ep.movers?.length && <span className={view.dimmer}>nothing else moved</span>}
                    {(ep.movers?.length ?? 0) + (ep.moversOmitted ?? 0) > TOP_MOVERS && (
                      <span className={view.dimmer}>
                        +{(ep.movers?.length ?? 0) - TOP_MOVERS + (ep.moversOmitted ?? 0)}
                      </span>
                    )}
                  </td>
                  <td className={peersClass(ep)} title={ep.correlation.statement}>
                    {peersText(ep)}
                  </td>
                </tr>
                {open === i && (
                  <tr className={styles.detail}>
                    <td colSpan={6}>
                      <Detail ep={ep} />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

function Detail({ ep }: { ep: DipExplanation }) {
  return (
    <>
      {ep.movers && ep.movers.length > 0 ? (
        <table className={styles.moverTable}>
          <thead>
            <tr>
              <th>signal</th>
              <th className={styles.r}>before → during</th>
              <th>kind</th>
              <th>from</th>
            </tr>
          </thead>
          <tbody>
            {ep.movers.map((m) => (
              <tr key={`${m.provenance}-${m.signal}`}>
                <td className={styles.mono}>{m.signal}</td>
                {/* "0 → 7" rather than "+7": the before/after pair is the
                    difference between a number and a story. */}
                <td className={styles.r}>
                  {withUnit(m.from, undefined, 0)} → {withUnit(m.to, m.unit, 0)}
                </td>
                <td className={view.dim}>{m.semantic}</td>
                <td>
                  <span className={`${view.prov} ${view[`prov_${m.provenance}`] ?? ''}`}>{m.provenance}</span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : (
        <p className={view.note}>
          Nothing else moved materially inside this window — the collapse is unexplained by the counters this session
          reported.
        </p>
      )}
      {ep.moversOmitted ? (
        <p className={view.note}>
          {ep.moversOmitted} further signal(s) moved and are not shown; the list is ranked by magnitude.
        </p>
      ) : null}
      <p className={`${view.note} ${styles.statement}`}>
        {ep.correlation.statement}
        {ep.correlation.peersReporting > 0 && (
          <>
            {' '}
            ({confidence(ep.correlation.confidence)} confidence, from {ep.correlation.peersReporting} reporting peer
            {ep.correlation.peersReporting === 1 ? '' : 's'})
          </>
        )}
      </p>
    </>
  );
}

function moverText(m: Mover): string {
  return `${withUnit(m.from, undefined, 0)} → ${withUnit(m.to, m.unit, 0)}`;
}

function peersText(ep: DipExplanation): string {
  const c = ep.correlation;
  if (c.peersReporting === 0) return 'no other session reporting';
  if (c.peersDipped === 0) return `alone — ${c.peersReporting} others steady`;
  if (c.peersDipped === c.peersReporting) return `all ${c.peersReporting} dipped too`;
  return `${c.peersDipped} of ${c.peersReporting} dipped too`;
}

function peersClass(ep: DipExplanation): string {
  const c = ep.correlation;
  return c.peersDipped > 0 ? styles.shared : view.dim;
}
