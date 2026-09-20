import { useState, useSyncExternalStore } from 'react';
import { RedocStandalone } from 'redoc';

import { ApiConsole } from './ApiConsole.tsx';
import { ASYNCAPI_URL, OPENAPI_URL, readPalette, redocOptions } from './apiDocs.ts';
import ui from '../styles/ui.module.css';
import './ApiView.css';

/**
 * Re-render when the OS colour scheme changes.
 *
 * Redoc resolves its theme once, from the options it is handed, so a page
 * themed at mount stays that way when the operator's laptop flips to light at
 * sunset — dark text on a light panel, or worse. The rest of the portal needs
 * none of this: it is CSS variables all the way down and the browser does the
 * work. This is the cost of theming a component that does not read them.
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
 * Redoc, themed once per mount.
 *
 * The options are built in a `useState` INITIALISER, not on every render, and
 * that is load-bearing rather than tidy. Redoc's `StoreBuilder` memoises
 * `new AppStore(spec, specUrl, options)` on `[resolvedSpec, specUrl, options]`
 * BY REFERENCE, so a fresh options object re-normalises the whole document and
 * hands Redoc a new store: expanded responses collapse, the sidebar selection
 * resets, the page jumps. A child cannot promise its parent will not re-render
 * — `Portal` re-renders on every hash change and every session transition, and
 * renders `<ApiView />` fresh each time — so the options have to be pinned
 * here, where nothing above can reach them.
 *
 * `ApiView` remounts this with `key={scheme}`, which is what re-reads the
 * palette when the OS flips between light and dark.
 */
function ThemedRedoc() {
  const [options] = useState(() => redocOptions(readPalette()));
  return (
    <RedocStandalone specUrl={OPENAPI_URL} options={options} onLoaded={logRenderError} />
  );
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
 * **Everything here is served by this binary.** The bundle is built into the
 * SPA, not loaded from a CDN — the portal's CSP is `default-src 'self'` and
 * the no-external-assets test enforces it as a build property — and no
 * user-supplied document is ever loaded.
 *
 * **This module is the whole reason the view is lazily imported.** Redoc is
 * ~1 MB of JavaScript; the moderation views must not pay for a page they may
 * never open, so `App.tsx` imports this with `React.lazy` and it lands in its
 * own chunk that the entry chunk does not reference.
 */
export default function ApiView() {
  const scheme = useSyncExternalStore(subscribeToScheme, currentScheme, () => 'dark');
  // Closed on every visit: the drawer is a tool picked up for one call, not
  // a mode, and a page that reopens it would put a form over the reference
  // for the operator who came to read.
  const [consoleOpen, setConsoleOpen] = useState(false);

  return (
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
          own DOM, and must not leak into the portal's views. `withConsole`
          narrows it while the drawer is open so Redoc reflows beside the
          drawer instead of under it. */}
      <div className={consoleOpen ? 'gawk-redoc withConsole' : 'gawk-redoc'}>
        {/* Remounting on a scheme change is deliberate: Redoc resolves its
            theme when it initialises, so handing the same instance new options
            would leave half the page on the old palette. */}
        <ThemedRedoc key={scheme} />
      </div>
    </section>
  );
}
