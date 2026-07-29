import { useEffect, useState } from 'react';

import type { Coverage } from '../api/types.ts';
import { absoluteTime, dur, timeZoneLabel } from '../lib/format.ts';
import { href, useRoute, type ViewName } from '../router/router.ts';
import { CLOCK_SKEW_WARN_MS, useMetaStore } from '../state/metaStore.ts';
import { useLiveStore } from '../state/liveStore.ts';
import styles from './Chrome.module.css';

// The page furniture: nav, the coverage banner, the pause control and the
// honesty strip.
//
// UD13 is the shape here: **the live fleet page stays the landing view.** TM8's
// surface is what you open when someone says "it's stuttering", and it does
// that job. History, Explore, Fleet and Rules become peer sections behind this
// nav, reachable in one click — nothing about the existing scan surface moves.

const NAV: Array<{ view: ViewName; label: string; hint: string }> = [
  { view: 'live', label: 'Live', hint: 'Is anything wrong right now?' },
  { view: 'history', label: 'History', hint: 'It was bad at 21:04. Show me.' },
  { view: 'fleet', label: 'Fleet', hint: 'Did the whole fleet have a bad minute?' },
  { view: 'explore', label: 'Explore', hint: 'Plot any recorded field.' },
  { view: 'rules', label: 'Rules', hint: 'What is a verdict resting on?' },
  { view: 'sql', label: 'SQL', hint: 'Ad-hoc queries over the partitions.' },
];

export function Nav() {
  const route = useRoute();
  const sql = useMetaStore((s) => s.meta?.sql);
  const items = NAV.filter((n) => n.view !== 'sql' || sql !== false);
  // `session` and `broadcast` are reached FROM the sections rather than from
  // the nav, but they still have to light one up — landing on a permalink and
  // seeing no section selected reads as being lost.
  const active: ViewName =
    route.view === 'session' || route.view === 'broadcast' ? 'history' : route.view;

  return (
    <nav className={styles.nav} aria-label="Sections">
      {items.map((n) => (
        <a
          key={n.view}
          href={href(n.view)}
          title={n.hint}
          className={`${styles.navItem} ${active === n.view ? styles.navActive : ''}`}
          aria-current={active === n.view ? 'page' : undefined}
        >
          {n.label}
        </a>
      ))}
    </nav>
  );
}

/**
 * UD10 on screen: what this answer does NOT rest on.
 *
 * Rendered whenever the server says something, and never summarised away. The
 * failure it exists to prevent is concluding "nothing was wrong" from "nothing
 * was kept" — which a bare empty table produces effortlessly.
 */
export function CoverageNote({ coverage }: { coverage?: Coverage }) {
  if (!coverage?.note) return null;
  return (
    <p className={styles.coverage} role="note">
      {coverage.note}
    </p>
  );
}

/**
 * TH11's pause (Q11): one key freezes every poll and every chart, and the
 * frozen instant is stated. Nothing moves while it is read.
 */
export function PauseBar() {
  const paused = useLiveStore((s) => s.paused);
  const pausedAtMs = useLiveStore((s) => s.pausedAtMs);
  const setPaused = useLiveStore((s) => s.setPaused);
  const mode = useLiveStore((s) => s.mode);
  const gapMs = useLiveStore((s) => s.gapMs);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== ' ' && e.key.toLowerCase() !== 'p') return;
      // Space in a text field is a space. A shortcut that eats it would make
      // the SQL console and the annotation box unusable.
      const el = e.target as HTMLElement | null;
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      e.preventDefault();
      setPaused(!useLiveStore.getState().paused);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [setPaused]);

  return (
    <div className={styles.pauseWrap}>
      {/* Occupies its slot whether or not it has anything to say, so the page
          below never jumps when the state changes. */}
      <span className={`${styles.gap} ${gapMs ? styles.gapOn : ''}`}>
        {gapMs
          ? `this tab was in the background for ${dur(gapMs)} — that gap was never observed and is not drawn`
          : ''}
      </span>
      <button
        type="button"
        className={`${styles.pause} ${paused ? styles.paused : ''}`}
        onClick={() => setPaused(!paused)}
        title="Freeze every poll and every chart (space)"
        aria-pressed={paused}
      >
        {paused ? `frozen at ${absoluteTime(pausedAtMs)}` : 'pause'}
      </button>
      <span className={styles.mode} title={feedHint(mode)}>
        {mode}
      </span>
    </div>
  );
}

function feedHint(mode: string): string {
  switch (mode) {
    case 'stream':
      return 'Live over SSE: only changes are sent, so an idle fleet costs a heartbeat.';
    case 'poll':
      return 'The stream is unavailable here, so the page is falling back to a 2 s poll. Nothing else changes.';
    default:
      return 'Connecting…';
  }
}

/**
 * The clock strip. Every historical timestamp on this page is absolute (UD5),
 * so the zone is stated once — and a browser clock that disagrees with the
 * service's is named rather than left to shift every reading silently.
 */
export function ClockNote() {
  const skew = useMetaStore((s) => s.clockSkewMs);
  if (Math.abs(skew) < CLOCK_SKEW_WARN_MS) {
    return <span className={styles.tz}>times in {timeZoneLabel()}</span>;
  }
  return (
    <span className={`${styles.tz} ${styles.tzBad}`}>
      times in {timeZoneLabel()} — this browser’s clock is {dur(Math.abs(skew))}{' '}
      {skew > 0 ? 'ahead of' : 'behind'} the service’s
    </span>
  );
}

/**
 * UD17: the dense views are desktop-only and SAY so rather than degrading into
 * something unreadable on a 390 px viewport.
 *
 * Read-only triage — the live list, severity, verdicts and a single-metric
 * chart — works on a phone. A five-lane synchronised timeline does not, and
 * pretending otherwise costs more than admitting it.
 */
export function DesktopOnly({ what }: { what: string }) {
  const [narrow, setNarrow] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 720px)');
    const update = () => setNarrow(mq.matches);
    update();
    mq.addEventListener('change', update);
    return () => mq.removeEventListener('change', update);
  }, []);
  if (!narrow) return null;
  return (
    <p className={styles.desktopOnly} role="note">
      {what} is desktop-only. It needs the width to be read honestly, and a
      squeezed version would mislead more than it showed.
    </p>
  );
}
