import type { DipReport } from '../api/types.ts';
import { absoluteTime, confidence, dur, withUnit } from '../lib/format.ts';
import styles from './DipPanel.module.css';

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

export function DipPanel({ report }: { report: DipReport }) {
  if (report.episodes.length === 0) {
    return (
      <section className={styles.panel}>
        <h2 className={styles.title}>Dips</h2>
        {/* Never a blank panel: "we looked and it was steady" and "we could not
            look" are different facts, and the server says which. */}
        <p className={styles.none}>{report.note ?? 'No dip episodes in this session.'}</p>
      </section>
    );
  }

  return (
    <section className={styles.panel}>
      <h2 className={styles.title}>
        Dips ({report.episodes.length}) — collapses in {report.primary} below half this session’s own
        baseline
      </h2>
      {report.episodes.map((ep) => (
        <article key={ep.fromMs} className={styles.episode}>
          <header className={styles.epHead}>
            <span className={styles.when}>{absoluteTime(ep.fromMs)}</span>
            <span className={styles.dim}>for {dur(ep.durationMs)}</span>
            <span className={styles.dim}>
              down to {ep.worstValue.toFixed(1)} from a baseline of {ep.baseline.toFixed(0)}
            </span>
          </header>

          {ep.movers && ep.movers.length > 0 ? (
            <table className={styles.movers}>
              <colgroup>
                <col style={{ width: '38%' }} />
                <col style={{ width: '32%' }} />
                <col style={{ width: '14%' }} />
                <col style={{ width: '16%' }} />
              </colgroup>
              <tbody>
                {ep.movers.map((m) => (
                  <tr key={`${m.provenance}-${m.signal}`}>
                    <th scope="row" className={styles.signal}>
                      {m.signal}
                    </th>
                    <td className={`${styles.value} tnum`}>
                      {/* "0 → 7" rather than "+7": the before/after pair is the
                          difference between a number and a story. */}
                      {withUnit(m.from, undefined, 0)} → {withUnit(m.to, m.unit, 0)}
                    </td>
                    <td className={styles.dim}>{m.semantic}</td>
                    <td>
                      <span className={`${styles.chip} ${styles[m.provenance] ?? ''}`}>
                        {m.provenance}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <p className={styles.none}>
              Nothing else moved materially inside this window — the collapse is unexplained by the
              counters this session reported.
            </p>
          )}
          {ep.moversOmitted ? (
            <p className={styles.none}>
              {ep.moversOmitted} further signal(s) moved and are not shown; the list is ranked by
              magnitude.
            </p>
          ) : null}

          <p className={styles.correlation}>
            {ep.correlation.statement}
            {ep.correlation.peersReporting > 0 && (
              <span className={styles.dim}>
                {' '}
                (confidence {confidence(ep.correlation.confidence)}, from{' '}
                {ep.correlation.peersReporting} reporting peer
                {ep.correlation.peersReporting === 1 ? '' : 's'})
              </span>
            )}
          </p>
        </article>
      ))}
    </section>
  );
}
