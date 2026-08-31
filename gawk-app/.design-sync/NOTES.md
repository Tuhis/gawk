# design-sync notes — gawk-app

Repo-specific gotchas for future syncs. Read this before re-running the driver.

## The big one: this repo has no library build

`gawk-app` is a **private React app**, not a published component library:
`package.json` has `"private": true`, no `main`/`module`/`exports`, no `types`,
and `npm run build` produces a **Vite app bundle** in `dist/` — not a library
entry and not a `.d.ts` tree. The converter's normal package path can't find an
entry, and `exportedNames()` hard-crashes on `node_modules/gawk-app/package.json`
(npm never self-installs a package into its own `node_modules`).

Three generated, gitignored artifacts bridge that gap. **`.design-sync/gen-entry.mjs`
creates all of them from `cfg.componentSrcMap`, so they can never drift from the
config**:

| Artifact | Why |
|---|---|
| `.ds-entry.ts` | esbuild's bundle entry — value re-exports of every component. Passed as `--entry`, which is *also* what makes `PKG_DIR` resolve to the repo root instead of the missing `node_modules/gawk-app`. |
| `index.d.ts` | the types entry `projectFor()` looks for at `<pkgDir>/index.d.ts`. Re-exports the emitted tree. |
| `.ds-types/` | the `tsc --emitDeclarationOnly` output, via `.design-sync/tsconfig.dts.json`. |

`findTypesRoot()` falls through to `pkgDir` here (no `build/ts`, `dist/types`,
`types`, `lib`, and `dist/` holds no `.d.ts`), so the glob `<repo>/**/*.d.ts`
picks up `.ds-types/**` and `index.d.ts` together. **Don't create a `dist/types/`
directory** — `findTypesRoot` would return it while `projectFor` still looks for
the entry at the package root, and the two would disagree.

### The re-sync command for this repo

```sh
node .design-sync/gen-entry.mjs                       # regenerate .ds-entry.ts + index.d.ts
npx tsc -p .design-sync/tsconfig.dts.json             # regenerate .ds-types/
export DS_CHROMIUM_PATH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
node .ds-sync/resync.mjs --config .design-sync/config.json --node-modules ./node_modules \
  --entry ./.ds-entry.ts --out ./ds-bundle --remote .design-sync/.cache/remote-sync.json
```

**`--entry ./.ds-entry.ts` is mandatory on every run.** Without it the build
crashes before it starts.

## Browser for the render check

No playwright browser is installed and none is needed: `playwright` is installed
into `.ds-sync/` with `PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1`, and both
`package-validate.mjs` and `package-capture.mjs` honour **`DS_CHROMIUM_PATH`**.
Point it at the system Chrome (path above). This mirrors what the repo's own
`.claude/skills/verify` skill does (playwright-core + system Chrome, no
download) and saves the ~200 MB browser fetch.

## Previews: the dark-canvas rule

gawk is a **dark-only** design system — `global.css` sets `color-scheme: dark`
and paints `html, body, #root` with `--bg` / `--text`. Preview cells render in
isolation and do **not** inherit that, so **every authored preview paints its own
canvas**:

```tsx
const canvas: React.CSSProperties = {
  background: 'var(--bg)',
  color: 'var(--text)',
  fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  minHeight: '100%',
  padding: '28px',
};
```

Skip it and the card lies: `.primary` is a near-white fill and `.ghost` is
`--muted` gray, both of which read as broken on a default white page.

**Anything that floats over live pixels** (`GlassPanel`, `StatsPanel`,
`StatsOverlay`, `BroadcasterStatsOverlay`, `IconButton`, `ContextMenu`,
`ServerPickerPanel`) is staged over a **gradient stand-in for the stream**
instead of flat black — the backdrop blur and translucency are invisible on a
flat surface, so a flat card misrepresents the component.

## Driving stores from previews

Several components read zustand stores and render **nothing** (or nothing
interesting) on default state. `.design-sync/preview-support-exports.json` lists
the stores merged into `.ds-entry.ts` so previews can drive them:
`useTransportStore`, `useBroadcastSettingsStore`, `usePipelineStore`. They're
hooks, so `isComponentName` keeps them out of the component list — they just ride
along on `window.GawkApp`.

This file exists **instead of a config key** because `package-build.mjs`
validates `config.json`'s top-level keys strictly and rejects unknown ones.

- **`ServerPickerPanel` calls `reloadFromStorage()` on mount** (line ~198), which
  wipes anything poked straight into the store. Seed it through the store's real
  `addServer` / `selectServer` actions, which persist. Cells share an origin, so
  clear previous custom entries first or they accumulate across captures.
- `ServerIndicator` returns `null` on the default server with no link note — that
  is the point of it, so every cell sets a non-default state.
- `ServerChip` renders only when `allowCustomRelays()` is true; it defaults to
  true when unset, so no config is needed.

## Known render warns (benign — a warn NOT listed here is new)

- `[RENDER_THIN] CaptureControls: variants render identically` — **false
  positive, visually verified.** The three cells clearly differ (blue "Start
  Capture" / red "Stop" / dimmed disabled "Stop"). The measured text is nearly
  identical because the status pill is the only text that changes.
- `[RENDER_THIN] ServerChip: variants render identically` — **false positive,
  visually verified.** Quiet reads a muted "Server", prominent reads an amber
  host name.
- `[DTS_STYLE_SYSTEM] filtering @types/react props` — expected; the components
  spread `HTMLAttributes`/`SVGProps`.
- `[DTS] parsed 1 .d.ts files` — expected. Only `index.d.ts` is added by the
  glob root; ts-morph resolves the rest of `.ds-types/` transitively through it.

## Card-mode overrides

`cfg.overrides` exists entirely to satisfy `[GRID_OVERFLOW]`. `column` for
components wider than a grid cell; `single` + `primaryStory` for anything
`position: fixed` or portalled, which no grid layout can present. Re-derive from
the validator's own message if the set changes — don't guess.

## States that can't be captured statically

- **`ViewerSettingsPanel`** — the `interpolationAvailable={null}` state lives
  inside the ADVANCED disclosure, which is collapsed by default and can't be
  opened in a static render. That cell was dropped; the prop is documented in the
  `.d.ts` instead.
- **`BroadcasterStatsOverlay`** — the `audioSupported={false}` row sits below the
  panel's internal scroll fold, so an `AudioUnsupported` cell is pixel-identical
  to `Healthy`. Dropped for the same reason.
- **`LadderPicker` / `EncoderSettingsPanel`** — per-option probe annotations live
  inside native `<select>` options and never paint in a screenshot. Inherent to
  the control, not a preview defect.

## Re-sync risks — what can silently go stale

- **`componentSrcMap` is a hand-maintained list of 35 components plus 11 explicit
  `null` exclusions** (the app screens: `ViewerScreen`, `BroadcasterScreen`,
  `LandingPage`, `BroadcastPage`, `ViewPage`, `TermsPage`, `LoopbackPage`, plus
  `DebugIndex`, `DecodedPreview`, `SourcePreview`, `App`). New components in
  `src/ui/` or `src/features/` are **not** picked up automatically — there is no
  `.d.ts` export list to discover them from. Re-check `src/ui/` and
  `src/features/*/` against the map on every sync.
- **Name collision**: `src/ui/StatsPanel.tsx` and
  `src/features/loopback/components/StatsPanel.tsx` both export `StatsPanel`.
  The map pins the `src/ui` one. If the loopback copy ever needs syncing it must
  be renamed first — `componentSrcMap` is keyed by component name.
- **Preview stats objects are hand-written `as never` casts** of `ViewerStats`,
  `BroadcastStats` and `EncoderConfigured`. They are NOT type-checked (esbuild
  strips types without checking), so a renamed or added field shows up as a
  literal `undefined` in the card rather than a build error. This already bit
  once: `BroadcasterStatsOverlay` rendered `Encoding: undefinedxundefined`
  because `encoderInfo` lacked `width`/`height`/`framerate`. **Read the review
  sheets for the word "undefined" after any change to those transport types.**
- **`ServerSettings` / `ServerPickerPanel` cards show `localhost:4433`** because
  `defaultServerUrl()` resolves to the dev default in this build. Harmless, but
  if the card should show the production relay the runtime config has to be set
  before capture.
- The declaration tree is regenerated by `tsc`; the repo pins `typescript: ~7.0.0`.
  A major TS bump can change the emitted `.d.ts` shape and therefore the props
  the design agent sees.
- `tsconfig.dts.json` relaxes `noUnusedLocals`/`noUnusedParameters` (declaration
  emit only). It extends `tsconfig.app.json`, so a change there propagates here.
