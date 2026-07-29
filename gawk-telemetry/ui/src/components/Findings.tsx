import { useState } from 'react';

import type { Evidence, Finding, Report } from '../api/types.ts';
import { SeverityBadge } from './SeverityBadge.tsx';
import { confidence, withUnit } from '../lib/format.ts';
import { copyToClipboard, findingToMarkdown, reportToMarkdown } from '../lib/export.ts';
import { href } from '../router/router.ts';
import styles from './Findings.module.css';

// TH6: findings render as ARGUMENTS, not as assertions.
//
// The defect this replaces was small and total: `BroadcastCard` rendered
// `finding.verdict` as a bare sentence, and `Evidence`, `Confidence`, `Action`,
// `Report.Passed` and `Report.Unavailable` — D6/D7's entire provenance
// apparatus, the thing that makes a verdict inspectable rather than believed —
// never reached a screen. The API argued; the dashboard asserted.
//
// So every element below exists to answer "what is that resting on?":
//
//   * each evidence row carries value, unit, comparison and a PROVENANCE chip,
//     because a relay-counted number and a client's account of itself are
//     different kinds of claim (D7);
//   * a client-only verdict is visibly weaker, with its cap explained rather
//     than left as an unexplained 60 %;
//   * `passed` is shown, so a healthy session is distinguishable from one
//     nothing ever analysed;
//   * `unavailable` names the missing signal, so a silence has a reason.

interface FindingProps {
  finding: Finding;
  /** Session id, so an evidence signal can link into the explorer. */
  sessionId?: string;
  fromMs?: number;
  toMs?: number;
}

export function FindingCard({ finding, sessionId, fromMs, toMs }: FindingProps) {
  const [copied, setCopied] = useState(false);
  const anchored = finding.evidence?.some((e) => e.from === 'relay') ?? false;

  return (
    <article className={styles.finding}>
      <header className={styles.head}>
        <SeverityBadge severity={finding.severity} />
        <span className={styles.verdict}>{finding.verdict}</span>
        <span className={styles.spacer} />
        <span
          className={`${styles.confidence} ${anchored ? '' : styles.capped}`}
          title={
            anchored
              ? 'A relay counter corroborates this verdict.'
              : 'No relay number corroborates this: a wedged client’s own accounting is the least reliable evidence in the system, so confidence is capped at 60 % (D7).'
          }
        >
          {confidence(finding.confidence)}
          {anchored ? ' relay-anchored' : ' client only'}
        </span>
        <button
          type="button"
          className={styles.copy}
          onClick={() => {
            void copyToClipboard(findingToMarkdown(finding)).then((ok) => setCopied(ok));
          }}
          title="Copy as markdown, evidence included — for pasting into docs/"
        >
          {copied ? 'copied' : 'copy md'}
        </button>
      </header>

      <p className={styles.ruleId}>
        <a href={href('rules', undefined, { rule: finding.id })} className={styles.ruleLink}>
          {finding.id}
        </a>
      </p>

      {finding.evidence && finding.evidence.length > 0 && (
        <table className={styles.evidence}>
          <colgroup>
            <col style={{ width: '34%' }} />
            <col style={{ width: '20%' }} />
            <col style={{ width: '12%' }} />
            <col style={{ width: '34%' }} />
          </colgroup>
          <tbody>
            {finding.evidence.map((e, i) => (
              <EvidenceRow key={`${e.signal}-${i}`} evidence={e} sessionId={sessionId} fromMs={fromMs} toMs={toMs} />
            ))}
          </tbody>
        </table>
      )}

      {finding.action && <p className={styles.action}>{finding.action}</p>}
    </article>
  );
}

function EvidenceRow({
  evidence,
  sessionId,
  fromMs,
  toMs,
}: {
  evidence: Evidence;
  sessionId?: string;
  fromMs?: number;
  toMs?: number;
}) {
  // The shortest path from "why do you say that" to "let me look": the signal
  // name is a link into the explorer with that field plotted over this
  // session's range.
  const signal = evidence.signal.includes('.')
    ? evidence.signal.slice(evidence.signal.lastIndexOf('.') + 1)
    : evidence.signal;
  const explore =
    sessionId && evidence.from !== 'relay'
      ? href('explore', undefined, { sessions: sessionId, fields: signal, from: fromMs, to: toMs })
      : null;

  return (
    <tr>
      <th scope="row" className={styles.signal}>
        {explore ? (
          <a href={explore} title="Plot this signal over the session">
            {evidence.signal}
          </a>
        ) : (
          evidence.signal
        )}
      </th>
      <td className={`${styles.value} tnum`}>
        {evidence.text ? evidence.text : withUnit(evidence.value, evidence.unit)}
      </td>
      <td>
        <span className={`${styles.chip} ${styles[evidence.from ?? 'derived']}`}>
          {evidence.from ?? 'derived'}
        </span>
      </td>
      <td className={styles.comparison}>{evidence.comparison ?? ''}</td>
    </tr>
  );
}

interface ReportProps {
  report: Report;
  sessionId?: string;
  fromMs?: number;
  toMs?: number;
  title?: string;
}

/** A whole report: findings, caveats, what passed, and what could not run. */
export function ReportPanel({ report, sessionId, fromMs, toMs, title }: ReportProps) {
  const [showPassed, setShowPassed] = useState(false);
  const [copied, setCopied] = useState(false);

  return (
    <section className={styles.report}>
      {report.caveats && report.caveats.length > 0 && (
        <ul className={styles.caveats}>
          {report.caveats.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
      )}

      {report.findings && report.findings.length > 0 ? (
        report.findings.map((f) => (
          <FindingCard key={f.id} finding={f} sessionId={sessionId} fromMs={fromMs} toMs={toMs} />
        ))
      ) : (
        <p className={styles.healthy}>
          {/* Positive, with its basis. "No issues found" and "nothing was ever
              analysed" are different claims and this is where they separate. */}
          No rule fired.{' '}
          {report.passed?.length
            ? `${report.passed.length} check${report.passed.length === 1 ? '' : 's'} passed.`
            : 'And no check could be evaluated — this is an absence of analysis, not health.'}
        </p>
      )}

      <footer className={styles.reportFoot}>
        <button type="button" className={styles.toggle} onClick={() => setShowPassed((v) => !v)}>
          {showPassed ? '▾' : '▸'} {report.passed?.length ?? 0} passed ·{' '}
          {report.unavailable?.length ?? 0} unavailable
        </button>
        <button
          type="button"
          className={styles.copy}
          onClick={() => {
            void copyToClipboard(reportToMarkdown(report, { title })).then((ok) => setCopied(ok));
          }}
        >
          {copied ? 'copied' : 'copy report md'}
        </button>
      </footer>

      {showPassed && (
        <div className={styles.checks}>
          <div>
            <h4 className={styles.checksTitle}>Passed</h4>
            {report.passed?.length ? (
              <ul className={styles.checkList}>
                {report.passed.map((id) => (
                  <li key={id}>
                    <a href={href('rules', undefined, { rule: id })}>{id}</a>
                  </li>
                ))}
              </ul>
            ) : (
              <p className={styles.none}>None.</p>
            )}
          </div>
          <div>
            <h4 className={styles.checksTitle}>Could not be evaluated</h4>
            {report.unavailable?.length ? (
              <ul className={styles.checkList}>
                {report.unavailable.map((u) => (
                  <li key={u.id}>
                    <a href={href('rules', undefined, { rule: u.id })}>{u.id}</a>
                    <span className={styles.missing}> missing {u.missingSignals.join(', ')}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <p className={styles.none}>None — every rule in scope had what it needed.</p>
            )}
          </div>
        </div>
      )}
    </section>
  );
}
