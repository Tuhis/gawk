import { useEffect, useMemo, useState } from 'react';

import { fetchSession } from '../api/client.ts';
import type { FieldDoc, Timeline } from '../api/types.ts';
import { TimeChart, type Series } from '../charts/TimeChart.tsx';
import { SERIES_COLORS } from '../charts/echarts.ts';
import { DesktopOnly } from '../components/Chrome.tsx';
import { download, seriesToCsv } from '../lib/export.ts';
import { toRate } from '../lib/series.ts';
import { shortId, timeZoneLabel } from '../lib/format.ts';
import { isSessionId, useDocumentTitle, useUrlState } from '../router/router.ts';
import { fieldDoc, useMetaStore } from '../state/metaStore.ts';
import styles from './ExploreView.module.css';
import view from './view.module.css';

// TH5: any recorded field, over time, for one or more sessions.
//
// ~13 fields were plottable before this; ~80 exist. The gap was never a UI
// limitation — it was that the UI had a hard-coded list, and the service typed
// every field but exposed no KIND. So the fix is server-side (`/v1/fields`) and
// this view is its consumer.
//
// **The behaviour follows from the catalogue, not from a switch here:**
//
//   * a **counter** is offered as a RATE by default, with the cumulative form
//     one click away — plotting a cumulative counter as a line is near-useless,
//     and what an operator wants is how fast it moved;
//   * a **bool** renders as a step band, never as a line between 0 and 1;
//   * **units drive the axes**, so two series with different units get separate
//     ones instead of a nonsense shared scale;
//   * an **unknown field** — one this build has no type for, kept verbatim on
//     disk by D15 — appears in its own labelled group rather than vanishing.

export function ExploreView() {
  useDocumentTitle('explore — gawk telemetry');
  const catalogue = useMetaStore((s) => s.fields);
  const [sessionsParam, setSessions] = useUrlState('sessions');
  const [fieldsParam, setFields] = useUrlState('fields');
  const [cumulative, setCumulative] = useUrlState('cumulative');
  const [pending, setPending] = useState(sessionsParam);
  const [timelines, setTimelines] = useState<Record<string, Timeline>>({});
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const sessionIds = useMemo(
    () => sessionsParam.split(',').map((s) => s.trim()).filter(isSessionId),
    [sessionsParam],
  );
  const selected = useMemo(
    () => fieldsParam.split(',').map((s) => s.trim()).filter(Boolean),
    [fieldsParam],
  );

  useEffect(() => {
    if (sessionIds.length === 0) return;
    const ac = new AbortController();
    setLoading(true);
    Promise.all(
      sessionIds.map((id) =>
        // The named fields, at full resolution: this view exists to look
        // closely, and the default 40-point projection is a shape, not a look.
        fetchSession(id, { fields: selected.length ? selected : undefined, full: true }, ac.signal)
          .then((tl) => [id, tl] as const)
          .catch(() => null),
      ),
    )
      .then((pairs) => {
        const next: Record<string, Timeline> = {};
        for (const p of pairs) if (p) next[p[0]] = p[1];
        setTimelines(next);
        setError(Object.keys(next).length === 0 ? 'none of those sessions could be read' : null);
      })
      .finally(() => setLoading(false));
    return () => ac.abort();
  }, [sessionIds, selected]);

  // The picker's contents come from the catalogue plus whatever the loaded
  // sessions actually reported — which is how a field this build has never
  // heard of still reaches a chart.
  const { known, unknown } = useMemo(() => {
    const observed = new Set<string>();
    for (const tl of Object.values(timelines)) for (const f of tl.available ?? []) observed.add(f);
    const roles = new Set(Object.values(timelines).map((t) => t.role));
    const known = catalogue.filter(
      (f) => roles.size === 0 || f.roles.some((r) => roles.has(r)),
    );
    const knownNames = new Set(catalogue.map((f) => f.name));
    const unknown = [...observed].filter((n) => !knownNames.has(n) && n !== 'tMs').sort();
    return { known, unknown };
  }, [catalogue, timelines]);

  const series = useMemo(
    () => buildSeries(timelines, selected, catalogue, cumulative === '1'),
    [timelines, selected, catalogue, cumulative],
  );

  const toggleField = (name: string) => {
    const next = selected.includes(name) ? selected.filter((f) => f !== name) : [...selected, name];
    setFields(next.join(','));
  };

  return (
    <div className={view.page}>
      <header className={view.head}>
        <h1 className={view.title}>Explore</h1>
        <span className={view.subtitle}>times in {timeZoneLabel()}</span>
        <span className={view.spacer} />
        <button
          type="button"
          className={`${view.button} ${cumulative === '1' ? view.buttonOn : ''}`}
          onClick={() => setCumulative(cumulative === '1' ? '' : '1')}
          title="Counters are shown as a rate by default; this shows the cumulative form"
        >
          cumulative counters
        </button>
        <button
          type="button"
          className={view.button}
          disabled={series.length === 0}
          onClick={() => download('explore.csv', toCsv(series), 'text/csv')}
        >
          export csv
        </button>
      </header>

      <DesktopOnly what="The metric explorer" />

      <form
        className={view.controls}
        onSubmit={(e) => {
          e.preventDefault();
          setSessions(pending);
        }}
      >
        <label className={view.control}>
          sessions
          <input
            className={view.text}
            style={{ maxWidth: '32rem', width: '32rem' }}
            value={pending}
            placeholder="one or more session ids, comma-separated"
            onChange={(e) => setPending(e.target.value)}
          />
        </label>
        <button type="submit" className={view.button}>
          load
        </button>
        {sessionIds.length > 0 && (
          <span className={view.control}>
            {sessionIds.map((id) => (
              <span key={id} className={styles.chip}>
                {shortId(id)}
              </span>
            ))}
          </span>
        )}
      </form>

      {error && <p className={view.error}>{error}</p>}
      {loading && <p className={view.loading}>Reading sessions…</p>}

      {sessionIds.length === 0 ? (
        <p className={view.rollupOnly}>
          Paste one or more session ids — or arrive here from a finding’s evidence, which links
          straight in with the field already selected.
        </p>
      ) : (
        <>
          <div className={styles.layout}>
            <aside className={styles.picker}>
              <h2 className={view.panelTitle}>Fields</h2>
              <ul className={styles.fieldList}>
                {known.map((f) => (
                  <li key={f.name}>
                    <label className={styles.field} title={f.description}>
                      <input
                        type="checkbox"
                        checked={selected.includes(f.name)}
                        onChange={() => toggleField(f.name)}
                      />
                      <span className={styles.fieldName}>{f.name}</span>
                      <span className={styles.fieldKind}>
                        {f.semantic}
                        {f.unit ? ` · ${f.unit}` : ''}
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
              {unknown.length > 0 && (
                <>
                  <h2 className={view.panelTitle} title="D15 keeps a field this build has no type for verbatim on disk; it is plottable, but its kind and unit are not known here">
                    Unknown type
                  </h2>
                  <ul className={styles.fieldList}>
                    {unknown.map((name) => (
                      <li key={name}>
                        <label className={styles.field}>
                          <input
                            type="checkbox"
                            checked={selected.includes(name)}
                            onChange={() => toggleField(name)}
                          />
                          <span className={styles.fieldName}>{name}</span>
                          <span className={styles.fieldKind}>untyped here</span>
                        </label>
                      </li>
                    ))}
                  </ul>
                </>
              )}
            </aside>

            <div className={styles.plot}>
              {selected.length === 0 ? (
                <p className={view.rollupOnly}>Pick a field.</p>
              ) : (
                <TimeChart
                  series={series}
                  height={360}
                  showZoom
                  unit={unitOf(selected[0], catalogue)}
                  unitRight={series.some((s) => s.axis === 1) ? 'other unit' : undefined}
                  note={noteFor(selected, catalogue, cumulative === '1', timelines)}
                  empty="No values for these fields in these sessions."
                  ariaLabel="metric explorer"
                />
              )}
            </div>
          </div>
        </>
      )}
    </div>
  );
}

function unitOf(name: string, catalogue: FieldDoc[]): string | undefined {
  return fieldDoc(catalogue, name)?.unit;
}

/**
 * Build the plotted series.
 *
 * The same metric across several sessions lands on ONE axis — the A/B move,
 * the viewer that is fine beside the one that is not — which is why the series
 * key includes the session and the label carries both.
 */
function buildSeries(
  timelines: Record<string, Timeline>,
  selected: string[],
  catalogue: FieldDoc[],
  cumulative: boolean,
): Series[] {
  const out: Series[] = [];
  const units = new Set<string>();
  for (const name of selected) units.add(fieldDoc(catalogue, name)?.unit ?? '');
  const unitList = [...units];

  let colour = 0;
  for (const [id, tl] of Object.entries(timelines)) {
    const start = tl.startedAtMs ?? 0;
    for (const name of selected) {
      const doc = fieldDoc(catalogue, name);
      const isCounter = doc?.semantic === 'counter';
      const isBool = doc?.semantic === 'bool';
      const raw: Array<[number, number | null]> = tl.points.map((p) => [
        start + (p.tMs ?? 0),
        p[name] === undefined ? null : p[name],
      ]);
      if (raw.every(([, v]) => v === null)) continue;

      // A counter's LEVEL says almost nothing; its first difference is the
      // interesting series. Differencing here rather than server-side keeps the
      // cumulative form one click away rather than a second request.
      const data = isCounter && !cumulative ? toRate(raw) : raw;
      const unit = doc?.unit ?? '';
      out.push({
        key: `${id}:${name}`,
        label: Object.keys(timelines).length > 1 ? `${shortId(id, 6)} ${name}` : name,
        data,
        color: SERIES_COLORS[colour++ % SERIES_COLORS.length],
        unit: isCounter && !cumulative ? `${unit}/s` : unit,
        // A bool is a STATE: a step line holds its value until it changes,
        // which is what happened. A straight interpolation between 0 and 1
        // would draw a transition that never occurred.
        step: isBool,
        axis: unitList.length > 1 && unitList.indexOf(unit) > 0 ? 1 : 0,
      });
    }
  }
  return out;
}

function noteFor(
  selected: string[],
  catalogue: FieldDoc[],
  cumulative: boolean,
  timelines: Record<string, Timeline>,
): string | undefined {
  const parts: string[] = [];
  const counters = selected.filter((n) => fieldDoc(catalogue, n)?.semantic === 'counter');
  if (counters.length && !cumulative) {
    parts.push(`${counters.join(', ')} shown as a per-second rate; a counter reset draws a break`);
  }
  // UD9: a downsampled chart must SAY it is showing worst-of-N, or a reader
  // takes the line for raw.
  const down = Object.values(timelines).find((t) => t.downsampled || t.truncated);
  if (down?.note) parts.push(down.note);
  return parts.length ? parts.join(' · ') : undefined;
}

function toCsv(series: Series[]): string {
  const times = new Set<number>();
  for (const s of series) for (const [t] of s.data) times.add(t);
  const sorted = [...times].sort((a, b) => a - b);
  const index = series.map((s) => new Map(s.data));
  return seriesToCsv(
    ['atIso', ...series.map((s) => s.label)],
    sorted.map((t) => [new Date(t).toISOString(), ...index.map((m) => m.get(t) ?? '')]),
  );
}
