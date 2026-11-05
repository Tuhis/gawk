import { useEffect, useState } from 'react';

import { fetchMe } from '../api/client.ts';
import type { Coverage, Me } from '../api/types.ts';
import { useAuthStore } from '../auth/auth.ts';
import { absoluteTime, dur, shortDate, timeZoneLabel } from '../lib/format.ts';
import { href, useRoute, type ViewName } from '../router/router.ts';
import { CLOCK_SKEW_WARN_MS, useMetaStore } from '../state/metaStore.ts';
import { isStale, useLiveStore } from '../state/liveStore.ts';
import { useThemeStore, type ThemePref } from '../state/theme.ts';
import { useUiStore } from '../state/uiStore.ts';
import styles from './Chrome.module.css';

// The page furniture: the header (brand, sections, search, the feed's state,
// the clock, pause, theme and — in OIDC mode only — who is signed in), and the
// coverage note.
//
// UD13 is the shape here: **the live fleet page stays the landing view.** TM8's
// surface is what you open when someone says "it's stuttering", and it does
// that job. History, Explore, Fleet and Rules are peer sections behind this
// nav, reachable in one click — R36 restyled them and moved nothing.

const NAV: Array<{ view: ViewName; label: string; hint: string }> = [
  { view: 'live', label: 'Live', hint: 'Is anything wrong right now?' },
  { view: 'history', label: 'History', hint: 'It was bad at 21:04. Show me.' },
  { view: 'fleet', label: 'Fleet', hint: 'Did the whole fleet have a bad minute?' },
  { view: 'explore', label: 'Explore', hint: 'Plot any recorded field.' },
  { view: 'rules', label: 'Rules', hint: 'What is a verdict resting on?' },
  { view: 'sql', label: 'SQL', hint: 'Ad-hoc queries over the partitions.' },
];

/** The whole header. */
export function Header() {
  return (
    <header className={styles.top}>
      <a className={styles.brand} href={href('live')} aria-label="gawk telemetry — live">
        <span className={styles.mark} aria-hidden>
          <i />
        </span>
        gawk <span className={styles.brandSub}>telemetry</span>
      </a>
      <Nav />
      <span className={styles.grow} />
      <SearchButton />
      <div className={styles.status}>
        <FeedState />
        <ClockNote />
        <PauseButton />
        <ThemeToggle />
        <SignedIn />
      </div>
    </header>
  );
}

export function Nav() {
  const route = useRoute();
  const sql = useMetaStore((s) => s.meta?.sql);
  // An engine that failed to open keeps its tab: the console says why, where
  // a missing tab reads as a feature nobody enabled.
  const sqlFailed = useMetaStore((s) => s.meta?.sqlFailed);
  const items = NAV.filter((n) => n.view !== 'sql' || sql !== false || sqlFailed);
  // `session`, `broadcast` and `room` are reached FROM the sections rather
  // than from the nav, but they still have to light one up — landing on a
  // permalink and seeing no section selected reads as being lost.
  const active: ViewName =
    route.view === 'session' || route.view === 'broadcast' || route.view === 'room'
      ? 'history'
      : route.view;

  return (
    <nav className={styles.tabs} aria-label="Sections">
      {items.map((n) => (
        <a
          key={n.view}
          href={href(n.view)}
          title={n.hint}
          className={`${styles.tab} ${active === n.view ? styles.tabOn : ''}`}
          aria-current={active === n.view ? 'page' : undefined}
        >
          {n.label}
        </a>
      ))}
    </nav>
  );
}

/** The command palette's mouse path; ⌘K is the keyboard one (TH11). */
function SearchButton() {
  const setOpen = useUiStore((s) => s.setPaletteOpen);
  const mac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform);
  return (
    <button
      type="button"
      className={styles.search}
      onClick={() => setOpen(true)}
      aria-label="Open the command palette"
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden>
        <circle cx="7" cy="7" r="4.5" />
        <path d="M10.5 10.5L14 14" />
      </svg>
      <span className={styles.searchText}>Session, broadcast or join code</span>
      <span className={styles.kbd}>{mac ? '⌘K' : 'Ctrl K'}</span>
    </button>
  );
}

/**
 * How the page is being fed, and whether that feed is healthy. A stale or
 * failed feed is a fault, so it borrows a severity hue; a healthy one stays
 * neutral. The background-gap note (TH11) lives here too, as a chip whose
 * whole sentence is one hover or focus away — R31 truncated it at 46ch.
 */
function FeedState() {
  const mode = useLiveStore((s) => s.mode);
  const error = useLiveStore((s) => s.error);
  const lastOkAt = useLiveStore((s) => s.lastOkAt);
  const gapMs = useLiveStore((s) => s.gapMs);
  const paused = useLiveStore((s) => s.paused);
  // Staleness is about time passing with NOTHING arriving, which by
  // definition changes no store: a slow tick re-asks the question.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 2000);
    return () => clearInterval(id);
  }, []);
  // A paused page is frozen on purpose; its feed is not stale, it is ignored.
  const stale = !paused && isStale(lastOkAt, now, mode);
  const tone = error ? styles.feedBad : stale ? styles.feedWarn : '';
  const label = error ? 'feed down' : stale ? 'feed stale' : mode === 'connecting' ? 'connecting' : `live · ${mode}`;
  const gapText = gapMs
    ? `This tab was in the background for ${dur(gapMs)}. That gap was never observed and is not drawn.`
    : '';

  return (
    <>
      <span className={`${styles.feed} ${tone}`} title={error ? `The live feed is unavailable: ${error}` : feedHint(mode)}>
        <i aria-hidden />
        {label}
      </span>
      {gapMs ? (
        <span className={styles.gapChip} title={gapText} tabIndex={0} aria-label={gapText}>
          unobserved {dur(gapMs)}
        </span>
      ) : null}
    </>
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
  const sentence = `Times are in ${timeZoneLabel()}. This browser’s clock is ${dur(Math.abs(skew))} ${
    skew > 0 ? 'ahead of' : 'behind'
  } the service’s, so “ago” readings are off by that much.`;
  return (
    <span className={`${styles.tz} ${styles.tzBad}`} title={sentence} tabIndex={0} aria-label={sentence}>
      clock {dur(Math.abs(skew))} {skew > 0 ? 'ahead' : 'behind'}
    </span>
  );
}

/**
 * TH11's pause (Q11): one key freezes every poll and every chart, and the
 * frozen instant is stated. Nothing moves while it is read.
 */
export function PauseButton() {
  const paused = useLiveStore((s) => s.paused);
  const pausedAtMs = useLiveStore((s) => s.pausedAtMs);
  const setPaused = useLiveStore((s) => s.setPaused);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== ' ' && e.key.toLowerCase() !== 'p') return;
      // Space in a text field is a space. A shortcut that eats it would make
      // the SQL console and the annotation box unusable.
      const el = e.target as HTMLElement | null;
      if (
        el &&
        (el.tagName === 'INPUT' ||
          el.tagName === 'TEXTAREA' ||
          el.tagName === 'SELECT' ||
          el.tagName === 'BUTTON' ||
          el.isContentEditable)
      )
        return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      e.preventDefault();
      setPaused(!useLiveStore.getState().paused);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [setPaused]);

  return (
    <button
      type="button"
      className={`${styles.pause} ${paused ? styles.paused : ''}`}
      onClick={() => setPaused(!paused)}
      title="Freeze every poll and every chart (space)"
      aria-pressed={paused}
    >
      {paused ? `frozen at ${absoluteTime(pausedAtMs)}` : 'pause'}
      {!paused && <span className={styles.kbd}>space</span>}
    </button>
  );
}

const THEME_NEXT: Record<ThemePref, ThemePref> = { dark: 'light', light: 'system', system: 'dark' };

/** Dark (the default, UD14), light, or whatever the OS says. One click cycles. */
function ThemeToggle() {
  const pref = useThemeStore((s) => s.pref);
  const setPref = useThemeStore((s) => s.setPref);
  const next = THEME_NEXT[pref];
  const label = `Theme: ${pref === 'system' ? 'follows the system' : pref}. Switch to ${
    next === 'system' ? 'the system’s' : next
  }.`;
  return (
    <button type="button" className={styles.iconBtn} onClick={() => setPref(next)} title={label} aria-label={label}>
      {pref === 'dark' ? (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden>
          <path d="M13.5 9.5A5.5 5.5 0 0 1 6.5 2.5a5.5 5.5 0 1 0 7 7z" />
        </svg>
      ) : pref === 'light' ? (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden>
          <circle cx="8" cy="8" r="3" />
          <path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1 1M11.6 11.6l1 1M3.4 12.6l1-1M11.6 4.4l1-1" />
        </svg>
      ) : (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden>
          <rect x="2" y="3" width="12" height="8.5" rx="1.5" />
          <path d="M6 14h4" />
        </svg>
      )}
    </button>
  );
}

/**
 * Who is signed in, and the way out (docs/55 TO3). OIDC mode only: in none and
 * basic mode there is no session here to end — basic auth's credential is the
 * browser's own — so this renders nothing and the header is as it always was.
 */
export function SignedIn() {
  const mode = useAuthStore((s) => s.mode);
  const session = useAuthStore((s) => s.session);
  const [me, setMe] = useState<Me | null>(null);
  const oidc = mode === 'oidc' && session !== null;

  useEffect(() => {
    if (!oidc) return;
    const ctrl = new AbortController();
    // Best effort: the button matters, the name is a courtesy.
    fetchMe(ctrl.signal).then(setMe, () => {});
    return () => ctrl.abort();
  }, [oidc]);

  if (!oidc) return null;
  const who = me ? me.email || me.subject : '';
  return (
    <span className={styles.who}>
      {who ? (
        <span className={styles.whoName} title={who}>
          {who}
        </span>
      ) : null}
      <button type="button" className={styles.pause} onClick={() => void session.logout()}>
        sign out
      </button>
    </span>
  );
}

/**
 * The read listener cannot verify ANY token because it has never reached the
 * identity provider (`401 idp_unavailable`, docs/55 D8). Said plainly, because
 * the alternative is a page of "HTTP 401"s that reads as the operator's own
 * login having failed — and signing in again would not help.
 */
export function IdpBanner() {
  const reason = useAuthStore((s) => s.idpUnavailable);
  if (reason === null) return null;
  return (
    <p className={styles.idp} role="alert">
      The telemetry service cannot reach its identity provider, so it cannot check anyone’s
      sign-in right now ({reason}). This is on the server’s side; the page keeps retrying.
    </p>
  );
}

/**
 * UD10 on screen: what this answer does NOT rest on — narrowed by R36 to the
 * cases where it is a limit worth stating (docs/86 TU4).
 *
 * The failure it exists to prevent is still concluding "nothing was wrong"
 * from "nothing was kept", so it is never dismissible. What changed is WHEN it
 * fires. A range that reaches back before this service existed is not a caveat
 * but the service's age: the views draw that as the axis extent ("data since
 * …", see `DataSince`), and this note stays silent for it. Retention — rows
 * that are rollup-only because their raw samples were pruned — is said once,
 * quietly. A service with nothing stored at all says so.
 */
export function CoverageNote({ coverage }: { coverage?: Coverage }) {
  if (!coverage?.note) return null;
  // A range that reaches past the service's start reports `before-data` even
  // when retention has ALSO pruned raw samples inside it; the retention line
  // still applies then, and says so.
  const pruned =
    coverage.reason === 'raw-pruned' ||
    (coverage.reason === 'before-data' && coverage.rawFromMs > (coverage.rollupsFromMs ?? 0));
  if (coverage.reason === 'before-data' && !pruned) return null;
  if (pruned && coverage.retentionDays) {
    return (
      <p className={styles.coverage} role="note" title={coverage.note}>
        Raw samples older than {coverage.retentionDays} days are pruned; rollups and verdicts remain, so
        older rows are rollup-only.
      </p>
    );
  }
  return (
    <p className={styles.coverage} role="note">
      {coverage.note}
    </p>
  );
}

/**
 * The service's age, stated as the extent of a range rather than as a warning
 * (R36 item 3). Rendered beside a range control when the range reaches back
 * further than anything stored.
 */
export function DataSince({ coverage, fromMs }: { coverage?: Coverage; fromMs: number }) {
  const since = coverage?.rollupsFromMs;
  if (!since || fromMs >= since) return null;
  return (
    <span className={styles.since} title="This service was installed after the start of this range: there is nothing before this to show, and nothing missing.">
      data since {shortDate(since)}
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
