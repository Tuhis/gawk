import { useEffect, useState, useSyncExternalStore } from 'react';
import { HashRouter } from 'react-router';
import { RedocStandalone } from 'redoc';

import { ApiConsole } from './ApiConsole.tsx';
import { ASYNCAPI_URL, DOCS_BASE_PATH, OPENAPI_URL, legacyDocsRoute, redocOptions } from './apiDocs.ts';
import ui from '../styles/ui.module.css';
import './ApiView.css';

/**
 * Remount Redoc when the OS colour scheme changes.
 *
 * Redoc 3 decides light or dark ONCE, from `prefers-color-scheme`, when it
 * mounts, and paints its own palette for that mode. `ApiView.css` points the
 * surfaces and text at the portal's variables, which follow the OS live — but
 * everything it does not reach (code tokens, status chips, admonitions) stays
 * on the mode Redoc picked, so a laptop flipping to light at sunset would
 * leave a page half in each. Remounting re-reads the scheme. The rest of the
 * portal needs none of this: it is CSS variables all the way down.
 */
const schemeQuery = '(prefers-color-scheme: light)';

function subscribeToScheme(onChange: () => void) {
  if (typeof window === 'undefined' || !window.matchMedia) return () => undefined;
  const media = window.matchMedia(schemeQuery);
  media.addEventListener('change', onChange);
  return () => media.removeEventListener('change', onChange);
}

function currentScheme(): 'light' | 'dark' {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark';
  return window.matchMedia(schemeQuery).matches ? 'light' : 'dark';
}

/** Module-level so `onLoaded` keeps one identity across renders. */
function logRenderError(error?: Error) {
  if (error) {
    // Redoc renders its own error panel; this is for the console, where
    // somebody debugging a sub-path deployment will look.
    console.error('the API contract could not be rendered', error);
  }
}

/**
 * Redoc, configured once per mount.
 *
 * The options are built in a `useState` initialiser, not on every render.
 * Redoc 3 reads them once — its loading effect depends on the document and
 * the base path only — so a fresh object per render would be ignored, not
 * applied; pinning it says so. `ApiView` remounts this with `key={scheme}`.
 */
function ThemedRedoc() {
  const [options] = useState(redocOptions);
  return (
    <RedocStandalone
      specUrl={OPENAPI_URL}
      basePath={DOCS_BASE_PATH}
      options={options}
      onLoaded={logRenderError}
    />
  );
}

/**
 * Publish the portal header's live height as `--gawk-header-h` on <html>
 * while the API page is mounted (ApiView.css reads it).
 *
 * Redoc's sidebar assumes it owns the viewport: sticky at the top on a wide
 * screen, fixed over the page as a menu on a narrow one. Here the portal's
 * sticky header owns that top, and its height is not a constant — 52px wide,
 * then 70px, 105px and more as its nav wraps — so a fixed offset left the
 * sidebar's search box, or the mobile menu, under the header's nav. The
 * header is App's (the page has exactly one); measuring it keeps this view
 * from restating App's layout.
 */
function usePortalHeaderHeight() {
  useEffect(() => {
    const header = document.querySelector('header');
    if (!header || typeof ResizeObserver === 'undefined') return;
    const root = document.documentElement.style;
    const apply = () =>
      root.setProperty('--gawk-header-h', `${Math.round(header.getBoundingClientRect().height)}px`);
    apply();
    const observer = new ResizeObserver(apply);
    observer.observe(header);
    return () => {
      observer.disconnect();
      root.removeProperty('--gawk-header-h');
    };
  }, []);
}

/**
 * Rewrite a Redoc 2 fragment in the address bar to the Redoc 3 route it means
 * (`legacyDocsRoute`), so a bookmark from before the upgrade still lands on
 * its operation. `replaceState`, not an assignment: the old URL is not a
 * page the Back button should return to.
 */
function upgradeLegacyHash() {
  if (typeof window === 'undefined') return;
  const route = legacyDocsRoute(window.location.hash);
  if (route) window.history.replaceState(window.history.state, '', `#${route}`);
}

/**
 * The API page (R48, docs/49 D5, revised 2026-09-17).
 *
 * It renders the contract this same deployment serves at
 * `/api/v1/openapi.json` — the copy carrying this deployment's base URL and
 * its configured role names, not the repository's symbolic ones.
 *
 * **Redoc, not Swagger UI.** The first cut used Swagger UI for its "Try it
 * out" button; the owner's call was that the page had to read well, and it is
 * the reference far more often than it is a REPL. What the swap gave up —
 * sending a call from the page — came back on 2026-09-21 as the in-house
 * **Console** drawer (`ApiConsole.tsx`), which is the part of that button
 * worth having without the part that cost: Redoc still executes nothing and
 * is still handed no token; the console sends through the session's own
 * `authorizedFetch`, exactly as a view does.
 *
 * **Redoc 3 runs inside a `HashRouter`** (docs/49 D5, revised 2026-11-01).
 * It is a react-router application, and without a router of ours it mounts a
 * `BrowserRouter` that rewrites `location.pathname`. Ours keeps every docs
 * route in the hash, under `#/api` (`apiDocs.ts`, `DOCS_BASE_PATH`), and the
 * Console drawer renders inside the same router so it can follow it.
 *
 * **Everything here is served by this binary.** The bundle is built into the
 * SPA, not loaded from a CDN — the portal's CSP is `default-src 'self'` and
 * the no-external-assets test enforces it as a build property — and no
 * user-supplied document is ever loaded.
 *
 * **This module is the whole reason the view is lazily imported.** Redoc is
 * ~2 MB of JavaScript; the moderation views must not pay for a page they may
 * never open, so `App.tsx` imports this with `React.lazy` and it lands in its
 * own chunk that the entry chunk does not reference.
 */
export default function ApiView() {
  const scheme = useSyncExternalStore(subscribeToScheme, currentScheme, () => 'dark');
  // Closed on every visit: the drawer is a tool picked up for one call, not
  // a mode, and a page that reopens it would put a form over the reference
  // for the operator who came to read.
  const [consoleOpen, setConsoleOpen] = useState(false);
  // Before the router below reads the location — so a useState initialiser,
  // the one hook that runs ahead of the first render's children. Idempotent,
  // so StrictMode's double call is harmless.
  useState(upgradeLegacyHash);
  usePortalHeaderHeight();

  return (
    <HashRouter>
      <section>
        <div className={ui.head}>
          <h1>API</h1>
          <span className={ui.sub}>
            the contract this deployment serves —{' '}
            <a href={OPENAPI_URL} target="_blank" rel="noreferrer">
              openapi.json
            </a>
            ; its events —{' '}
            <a href={ASYNCAPI_URL} target="_blank" rel="noreferrer">
              asyncapi.json
            </a>
          </span>
        </div>
        {/* The toggle FLOATS rather than sitting in the head strip: the reference
            is long, and the moment an operator wants to send the call they are
            reading about is exactly when the strip has scrolled away. It is
            hidden while the drawer is open — the drawer has its own close, and
            the drawer covers where the pill sits anyway. */}
        {consoleOpen ? (
          <ApiConsole onClose={() => setConsoleOpen(false)} />
        ) : (
          <button type="button" className="gawk-console-toggle" onClick={() => setConsoleOpen(true)}>
            Console
          </button>
        )}
        {/* The wrapper scopes ApiView.css: those overrides reach into Redoc's
            own DOM (and, via :has(), the root it themes from), and must not
            leak into the portal's views. `withConsole` hides Redoc's samples
            column while the drawer sits over it. */}
        <div className={consoleOpen ? 'gawk-redoc withConsole' : 'gawk-redoc'}>
          {/* Remounting on a scheme change is deliberate: Redoc picks light or
              dark when it mounts (see subscribeToScheme). */}
          <ThemedRedoc key={scheme} />
        </div>
      </section>
    </HashRouter>
  );
}
