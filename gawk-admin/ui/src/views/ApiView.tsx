import { useState, useSyncExternalStore } from 'react';
import { RedocStandalone } from 'redoc';

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
 * the reference far more often than it is a REPL. The loss is real — you
 * cannot execute a call from this page any more — and `docs/self-hosting.md`
 * §9.8's `curl` recipe is what replaces it. Two things came free with the
 * swap: the third-party bundle no longer needs the operator's access token
 * (it does not execute requests, so it is never handed one), and the whole
 * same-origin guard that protected that token is gone with it.
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
      {/* The wrapper scopes ApiView.css: those overrides reach into Redoc's
          own DOM, and must not leak into the portal's views. */}
      <div className="gawk-redoc">
        {/* Remounting on a scheme change is deliberate: Redoc resolves its
            theme when it initialises, so handing the same instance new options
            would leave half the page on the old palette. */}
        <ThemedRedoc key={scheme} />
      </div>
    </section>
  );
}
