import { useEffect, useState } from 'react';

import { fetchDiagnoseTrace, fetchRules } from '../api/client.ts';
import type { DiagnoseTrace, RuleDoc, Trace } from '../api/types.ts';
import { SeverityBadge } from '../components/SeverityBadge.tsx';
import { confidence, EMPTY, num, shortId } from '../lib/format.ts';
import { href, isSessionId, useDocumentTitle, useUrlState } from '../router/router.ts';
import styles from './RulesView.module.css';
import view from './view.module.css';

// TH6's second half and UD20: **full read-only rule transparency.**
//
// Every playbook rule, its thresholds, the signals it requires, its provenance
// and — where a session is named — a per-rule TRACE: what it read, what it
// compared against, and why it did or did not fire.
//
// **No tuning.** Stored verdicts were computed under the thresholds of their
// day, so editable thresholds would make history and live disagree unless every
// verdict also recorded the config it ran under. That is a real feature with a
// real cost, and it is not this one.
//
// This is also the mitigation for the risk docs/33 §8 names and TH6 raises: one
// rule engine, now visible in more places, is one bad rule wrong in more places
// at once. Evidence, provenance and a trace on screen are what let a human
// catch a rule that is confidently wrong.

export function RulesView() {
  useDocumentTitle('rules — gawk telemetry');
  const [rules, setRules] = useState<RuleDoc[]>([]);
  const [selected, setSelected] = useUrlState('rule');
  const [sessionId, setSessionId] = useUrlState('session');
  const [pending, setPending] = useState(sessionId);
  const [trace, setTrace] = useState<DiagnoseTrace | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetchRules()
      .then(setRules)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  useEffect(() => {
    if (!isSessionId(sessionId)) {
      setTrace(null);
      return;
    }
    const ac = new AbortController();
    fetchDiagnoseTrace(sessionId, ac.signal)
      .then(setTrace)
      .catch(() => setTrace(null));
    return () => ac.abort();
  }, [sessionId]);

  const traceById = new Map((trace?.trace ?? []).map((t) => [t.id, t]));

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Rules</h1>
        <span className={view.subtitle}>
          {rules.length} rules · read-only
        </span>
        <span className={view.spacer} />
        <form
          className={view.controls}
          onSubmit={(e) => {
            e.preventDefault();
            setSessionId(pending);
          }}
        >
          <label className={view.control}>
            trace against session
            <input
              className={view.text}
              style={{ width: '26rem', maxWidth: '26rem' }}
              value={pending}
              placeholder="session id"
              onChange={(e) => setPending(e.target.value)}
            />
          </label>
          <button type="submit" className={view.button}>
            trace
          </button>
        </form>
      </header>

      {error && <p className={view.error}>{error}</p>}

      <p className={view.note}>
        Thresholds come from the Go rules themselves, never from a second copy — a number here is
        the constant a verdict was actually computed with. They are not editable: a stored verdict
        ran under the thresholds of its day, and an editable threshold would make history and live
        disagree.
      </p>

      {trace && (
        <p className={view.note}>
          Tracing <a href={href('session', sessionId)}>{shortId(sessionId)}</a> —{' '}
          {trace.report.findings?.length ?? 0} fired, {trace.report.passed?.length ?? 0} passed,{' '}
          {trace.report.unavailable?.length ?? 0} unavailable.
        </p>
      )}

      <div className={styles.list}>
        {rules.map((r) => (
          <RuleCard
            key={r.id}
            rule={r}
            trace={traceById.get(r.id)}
            open={selected === r.id}
            onToggle={() => setSelected(selected === r.id ? '' : r.id)}
          />
        ))}
      </div>
    </div>
  );
}

function RuleCard({
  rule,
  trace,
  open,
  onToggle,
}: {
  rule: RuleDoc;
  trace?: Trace;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <article className={`${styles.rule} ${open ? styles.ruleOpen : ''}`} id={rule.id}>
      <header className={styles.head}>
        <button type="button" className={styles.disclosure} onClick={onToggle} aria-expanded={open}>
          {open ? '▾' : '▸'}
        </button>
        <span className={styles.id}>{rule.id}</span>
        <span className={styles.scope}>{rule.scope}</span>
        {rule.clientOnly && (
          <span
            className={styles.cap}
            title="This rule requires no relay signal, so no relay number can corroborate it — D7 caps its confidence."
          >
            client-only · max {confidence(rule.maxConfidence)}
          </span>
        )}
        <span className={view.spacer} />
        {trace && <TraceChip trace={trace} />}
      </header>

      <p className={styles.verdict}>{rule.verdict}</p>

      {open && (
        <div className={styles.body}>
          {rule.why && <p className={styles.why}>{rule.why}</p>}

          <div className={styles.cols}>
            <div>
              <h4 className={styles.h4}>Requires</h4>
              <ul className={styles.sigList}>
                {rule.requires.map((s) => (
                  <li key={s} className={styles.sig}>
                    {s}
                    {trace?.read?.[s] !== undefined && (
                      <span className={styles.read}> = {num(trace.read[s], 2)}</span>
                    )}
                    {trace?.readText?.[s] !== undefined && (
                      <span className={styles.read}> = {trace.readText[s]}</span>
                    )}
                    {trace && trace.missing?.includes(s) && (
                      <span className={styles.absent}> — absent</span>
                    )}
                  </li>
                ))}
                {rule.requires.length === 0 && <li className={styles.sig}>nothing</li>}
              </ul>
            </div>
            <div>
              <h4 className={styles.h4}>Thresholds</h4>
              {rule.thresholds?.length ? (
                <ul className={styles.sigList}>
                  {rule.thresholds.map((t) => (
                    <li key={t.name} className={styles.sig} title={t.note}>
                      {t.name} = <span className="tnum">{num(t.value, 3)}</span>
                      {t.unit ? ` ${t.unit}` : ''}
                    </li>
                  ))}
                </ul>
              ) : (
                <p className={styles.none}>
                  None — this rule fires on a signal being present or non-zero rather than on a
                  comparison.
                </p>
              )}
            </div>
          </div>

          {rule.action && (
            <>
              <h4 className={styles.h4}>Action</h4>
              <p className={styles.action}>{rule.action}</p>
            </>
          )}

          {trace && (
            <>
              <h4 className={styles.h4}>On this session</h4>
              {/* The point of the trace: a rule's SILENCE explained in terms of
                  the numbers it read, rather than left as trust. */}
              <p className={styles.traceText}>{explain(rule, trace)}</p>
            </>
          )}
        </div>
      )}
    </article>
  );
}

function TraceChip({ trace }: { trace: Trace }) {
  if (trace.outcome === 'fired') {
    return (
      <span className={styles.chipFired}>
        <SeverityBadge severity={trace.severity ?? 'warn'} /> fired
      </span>
    );
  }
  return <span className={`${styles.chip} ${styles[trace.outcome] ?? ''}`}>{trace.outcome}</span>;
}

function explain(rule: RuleDoc, trace: Trace): string {
  switch (trace.outcome) {
    case 'out-of-scope':
      return `This rule only applies to ${rule.scope} subjects, and this session is not one.`;
    case 'unavailable':
      return `It could not run: ${trace.missing?.join(', ')} ${
        (trace.missing?.length ?? 0) === 1 ? 'was' : 'were'
      } not present. A verdict never rests on a signal that does not exist, so it did not vote either way.`;
    case 'passed': {
      const read = Object.entries(trace.read ?? {})
        .map(([k, v]) => `${k} = ${num(v, 2)}`)
        .join(', ');
      const text = Object.entries(trace.readText ?? {})
        .map(([k, v]) => `${k} = ${v}`)
        .join(', ');
      const all = [read, text].filter(Boolean).join(', ');
      return all
        ? `It ran and did not fire. It read ${all}, and none of that crossed its thresholds.`
        : 'It ran and did not fire.';
    }
    case 'fired':
      return 'It fired. The evidence is on the session page, with each signal’s provenance.';
    default:
      return EMPTY;
  }
}
