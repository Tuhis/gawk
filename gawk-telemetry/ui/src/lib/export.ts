import type { Annotation, Finding, Report, Timeline } from '../api/types.ts';
import { absoluteTime, timeZoneLabel, withUnit } from './format.ts';

// TH11's export half.
//
// The shape it has to fit is this project's own habit: field findings end up in
// `docs/*.md`. So the markdown a finding produces is written to be PASTED —
// evidence rows intact, provenance intact, confidence stated, annotations
// alongside — rather than to look tidy in isolation.
//
// Two rules the formats share:
//
//   * **Every timestamp is absolute and carries its zone** (UD5). An exported
//     artifact outlives the tab that made it, and "21:04" without a zone is a
//     number someone will misread later.
//   * **An absent value exports as an em dash, not as 0.** The absent-vs-zero
//     distinction is the one thing this whole surface is built to preserve; an
//     export that flattens it would launder a "not measured" into a "measured
//     zero" the moment it left the page.

/** Trigger a download of `text` as `filename`. */
export function download(filename: string, text: string, mime = 'text/plain') {
  const blob = new Blob([text], { type: `${mime};charset=utf-8` });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoked on the next tick rather than immediately: Safari has been observed
  // to cancel an in-flight download when the object URL is revoked
  // synchronously after click().
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

/** One finding as markdown, evidence included. */
export function findingToMarkdown(f: Finding): string {
  const lines: string[] = [];
  lines.push(`**${f.severity.toUpperCase()}** — ${f.verdict}`);
  lines.push('');
  lines.push(`- rule: \`${f.id}\``);
  if (f.confidence !== undefined) {
    lines.push(`- confidence: ${Math.round(f.confidence * 100)} %`);
  }
  if (f.evidence?.length) {
    lines.push('- evidence:');
    for (const e of f.evidence) {
      const value = e.text ? e.text : withUnit(e.value, e.unit);
      const from = e.from ? ` _(${e.from})_` : '';
      const cmp = e.comparison ? ` — ${e.comparison}` : '';
      lines.push(`  - \`${e.signal}\` = ${value}${from}${cmp}`);
    }
  }
  if (f.action) {
    lines.push(`- action: ${f.action}`);
  }
  return lines.join('\n');
}

/** A whole report as markdown: findings, what passed, what could not run. */
export function reportToMarkdown(
  rep: Report,
  opts: { title?: string; notes?: Annotation[] } = {},
): string {
  const out: string[] = [];
  out.push(`## ${opts.title ?? `${rep.scope} ${rep.subject}`}`);
  out.push('');
  out.push(`_Exported ${absoluteTime(Date.now())} ${timeZoneLabel()}_`);
  out.push('');
  if (rep.caveats?.length) {
    out.push('> **Caveats**');
    for (const c of rep.caveats) out.push(`> - ${c}`);
    out.push('');
  }
  if (rep.findings?.length) {
    for (const f of rep.findings) {
      out.push(findingToMarkdown(f));
      out.push('');
    }
  } else {
    out.push(`No rule fired. ${rep.passed?.length ?? 0} check(s) passed.`);
    out.push('');
  }
  // `passed` and `unavailable` are exported, not dropped: a healthy verdict
  // without its basis is indistinguishable from an analysis that never ran,
  // and that distinction is the entire point of reporting health positively.
  if (rep.passed?.length) {
    out.push(`**Passed** (${rep.passed.length}): ${rep.passed.map((p) => `\`${p}\``).join(', ')}`);
    out.push('');
  }
  if (rep.unavailable?.length) {
    out.push('**Could not be evaluated**');
    for (const u of rep.unavailable) {
      out.push(`- \`${u.id}\` — missing ${u.missingSignals.map((s) => `\`${s}\``).join(', ')}`);
    }
    out.push('');
  }
  if (opts.notes?.length) {
    out.push('**Notes**');
    for (const n of opts.notes) {
      const at = n.atMs ? `${absoluteTime(n.atMs)} — ` : '';
      out.push(`- ${at}${n.text}${n.author ? ` _(${n.author})_` : ''}`);
    }
    out.push('');
  }
  return out.join('\n');
}

/** A chart's data as CSV. Absolute timestamps, one column per series. */
export function seriesToCsv(
  columns: string[],
  rows: Array<Array<number | string | null | undefined>>,
): string {
  const esc = (v: unknown) => {
    if (v === null || v === undefined) return '';
    const s = String(v);
    return /[",\n]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s;
  };
  return [columns.map(esc).join(','), ...rows.map((r) => r.map(esc).join(','))].join('\n');
}

/** A session timeline as CSV, with absolute time as the first column. */
export function timelineToCsv(tl: Timeline): string {
  const start = tl.startedAtMs ?? 0;
  const fields = tl.fields.filter((f) => f !== 'tMs');
  const rows = tl.points.map((p) => [
    new Date(start + (p.tMs ?? 0)).toISOString(),
    p.tMs ?? '',
    ...fields.map((f) => (p[f] === undefined ? '' : p[f])),
  ]);
  return seriesToCsv(['atIso', 'tMs', ...fields], rows);
}

/** A session bundle as JSON, readable by `jq` and reopenable by the UI. */
export function sessionBundle(
  tl: Timeline,
  report: Report | null,
  notes: Annotation[],
): string {
  return JSON.stringify(
    {
      exportedAtMs: Date.now(),
      timezone: timeZoneLabel(),
      session: tl,
      report,
      annotations: notes,
    },
    null,
    2,
  );
}
