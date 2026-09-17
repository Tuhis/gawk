// The Redoc configuration, separated from the view that mounts it (R48,
// docs/49 D5, revised 2026-09-17).
//
// It is its own module because the two things worth testing here — that the
// page reads this deployment's own document, and that the renderer is handed
// no credential — are properties of this object, not of React. Asserting them
// against a 1.1 MB third-party bundle rendered in jsdom would test the bundle.
//
// **There is no token here, and that is the point of the change.** Swagger UI
// executed requests, so it had to be handed the operator's access token
// through a `requestInterceptor`, with a same-origin guard so a misconfigured
// `-external-url` could not leak it. Redoc renders and does not execute, so
// the token never reaches the third-party bundle at all.

/**
 * Where the contract is served.
 *
 * RELATIVE — no leading slash — exactly like `client.ts`'s `BASE` and for the
 * same reason: `vite.config.ts` sets `base: './'`, so the whole SPA works
 * wherever it is mounted, and a root-absolute path here would make this one
 * page the only thing that breaks under an Ingress sub-path. Both the document
 * fetch and the link on the page would go to the origin root and 404.
 */
export const OPENAPI_URL = 'api/v1/openapi.json';

/**
 * The portal's palette, as the page is actually painting it.
 *
 * READ FROM THE LIVE CSS VARIABLES rather than restated here, because
 * `global.css` is the one place the portal's colours are decided and this page
 * has to follow both of its schemes. The variables are redefined under
 * `prefers-color-scheme: light`, so reading them at render time is also what
 * makes light mode work without a second table to keep in step.
 */
export interface Palette {
  bg: string;
  surface1: string;
  surface2: string;
  border: string;
  borderSoft: string;
  text: string;
  muted: string;
  faint: string;
  accent: string;
  accentHover: string;
  danger: string;
  warning: string;
  ok: string;
  mono: string;
  sans: string;
  /** Dark in BOTH schemes — see `--code-bg` in global.css. */
  codeBg: string;
  codeSurface: string;
  codeText: string;
  codeMuted: string;
}

/** The values `global.css` falls back to, for a test with no stylesheet. */
const FALLBACK: Palette = {
  bg: '#0a0b0d',
  surface1: '#121317',
  surface2: '#171920',
  border: '#262a33',
  borderSoft: '#1c2028',
  text: '#e8eaed',
  muted: '#9aa1ad',
  faint: '#626873',
  accent: '#6b8afe',
  accentHover: '#86a0ff',
  danger: '#e5484d',
  warning: '#d0a215',
  ok: '#30a46c',
  mono: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
  sans: 'system-ui, -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif',
  codeBg: '#0a0b0d',
  codeSurface: '#171920',
  codeText: '#e8eaed',
  codeMuted: '#9aa1ad',
};

const VARIABLES: Record<keyof Palette, string> = {
  bg: '--bg',
  surface1: '--surface-1',
  surface2: '--surface-2',
  border: '--border',
  borderSoft: '--border-soft',
  text: '--text',
  muted: '--muted',
  faint: '--faint',
  accent: '--accent',
  accentHover: '--accent-hover',
  danger: '--danger',
  warning: '--warning',
  ok: '--ok',
  mono: '--mono',
  sans: '--sans',
  codeBg: '--code-bg',
  codeSurface: '--code-surface',
  codeText: '--code-text',
  codeMuted: '--code-muted',
};

/** Reads the portal's palette off the document, falling back per-token. */
export function readPalette(element?: Element | null): Palette {
  const target =
    element ?? (typeof document === 'undefined' ? null : document.documentElement);
  if (!target || typeof getComputedStyle !== 'function') return { ...FALLBACK };
  const computed = getComputedStyle(target);
  const out = { ...FALLBACK };
  for (const [key, variable] of Object.entries(VARIABLES) as [keyof Palette, string][]) {
    const value = computed.getPropertyValue(variable).trim();
    if (value) out[key] = value;
  }
  return out;
}

/**
 * The options handed to Redoc, themed from the portal's own palette.
 *
 * **Redoc ships a light theme and paints no background on its middle panel**,
 * so out of the box it rendered its default `#333` prose straight onto the
 * portal's near-black canvas: legible in light mode, barely readable in dark,
 * with a white sidebar slab and a dead grey sample panel beside it. Every
 * colour below exists to fix that, and every one of them comes from the same
 * CSS variables the rest of the portal uses — so the page follows the portal
 * into either scheme instead of carrying a second palette that drifts.
 *
 * The non-colour options:
 *
 *   - `hideDownloadButton` is FALSE. Fetching the raw document is exactly what
 *     a bot author came to do, and the served copy is the one carrying this
 *     deployment's base URL and role names.
 *   - `expandResponses: '200,201'` puts the success shape on screen without a
 *     click, which is the shape somebody is reading for.
 *   - `nativeScrollbars` avoids Redoc's custom scrollbar, which fights the
 *     portal's own scrolling on a phone — and the portal is read from one
 *     (docs/42 §10).
 *   - `showExtensions` keeps `x-gawk-roles`, `x-gawk-requires` and
 *     `x-gawk-sensitive` on the page: they are what a consumer most needs.
 */
export function redocOptions(palette: Palette = readPalette()): Record<string, unknown> {
  const response = (accent: string) => ({
    color: accent,
    backgroundColor: palette.surface1,
    tabTextColor: accent,
  });

  return {
    // OFF, and not by preference: Redoc builds its search index in a worker
    // created from a `blob:` URL, and the portal's CSP is `default-src
    // 'self'` with no `worker-src`, so the browser refuses it and logs a
    // violation on every visit. The alternative is widening the CSP of the
    // page that holds an operator's access token to get search on a
    // documentation page (docs/42 §4.8 treats that CSP as load-bearing), and
    // that is the wrong way round. The sidebar lists every operation, and
    // the browser's own find-in-page works on the rendered document.
    disableSearch: true,
    hideDownloadButton: false,
    expandResponses: '200,201',
    nativeScrollbars: true,
    showExtensions: true,
    theme: {
      colors: {
        primary: { main: palette.accent },
        success: { main: palette.ok },
        warning: { main: palette.warning },
        error: { main: palette.danger },
        // Redoc uses these for the panel fills it draws itself; pointing them
        // at the portal's surfaces is what stops the white slabs.
        gray: { 50: palette.surface2, 100: palette.surface1 },
        border: { light: palette.borderSoft, dark: palette.border },
        text: { primary: palette.text, secondary: palette.muted },
        responses: {
          success: response(palette.ok),
          error: response(palette.danger),
          redirect: response(palette.warning),
          info: response(palette.accent),
        },
      },
      schema: {
        linesColor: palette.border,
        typeNameColor: palette.muted,
        typeTitleColor: palette.text,
        requireLabelColor: palette.danger,
        nestedBackground: palette.surface1,
        arrow: { color: palette.muted },
      },
      typography: {
        fontSize: '15px',
        fontFamily: palette.sans,
        headings: { fontFamily: palette.sans },
        links: {
          color: palette.accent,
          visited: palette.accent,
          hover: palette.accentHover,
        },
        code: {
          fontFamily: palette.mono,
          color: palette.text,
          backgroundColor: palette.surface2,
          wrap: true,
        },
      },
      sidebar: {
        width: '15rem',
        backgroundColor: palette.surface1,
        textColor: palette.muted,
        activeTextColor: palette.text,
        groupItems: { activeBackgroundColor: palette.surface2 },
        level1Items: { activeBackgroundColor: palette.surface2 },
        arrow: { color: palette.faint },
      },
      // The sample panel, DARK IN BOTH SCHEMES. Redoc syntax-highlights the
      // JSON samples with token colours chosen against a dark panel — on a
      // light one the light tokens vanish, which is exactly what happened
      // when this followed the portal's surfaces: 102 elements of white-on-
      // white in light mode. `--code-*` are the portal's scheme-independent
      // code surfaces for this reason.
      rightPanel: {
        backgroundColor: palette.codeSurface,
        textColor: palette.codeText,
      },
      codeBlock: { backgroundColor: palette.codeBg },
      fab: { backgroundColor: palette.codeSurface, color: palette.codeText },
    },
  };
}
