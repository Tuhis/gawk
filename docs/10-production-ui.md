# R6 — Production UI: Design + Implementation Plan

Design doc for [ROADMAP R6](../ROADMAP.md#r6--production-ui). The UI stops
looking like a diagnostics rig and becomes something a friend can be handed a
link to. Three production surfaces — a **landing** page (join by code / start a
stream), a **broadcaster** view (preview-hero, controls float), and a **viewer**
view (the stream *is* the background; fullscreen à la Netflix; a stats overlay
on a hotkey and from a right-click menu). The visual language is
**monochrome and restrained**: a near-black canvas, layered grays, one cool
accent used sparingly, generous negative space — the stream provides the color.

R6 is a **UI-only** effort. The transport, media, and state modules
(`BroadcastPipeline`, `ViewerSession`, the Zustand stores, `ladder.ts`,
`fallback.ts`, the wire format) are UI-agnostic and carry over **untouched** —
the same property every milestone since R3 has preserved. **Zero
relay-server, wire-format, and viewer-protocol changes.** The current
stats-heavy pages are not deleted; they are re-homed under a hidden `#/debug/*`
route and kept maintained as the troubleshooting surface.

## Design taste (decided 2026-07-13)

Locked with the broadcaster before drafting, so the doc plans one design rather
than surveying options:

| Axis | Decision |
|------|----------|
| Visual personality | **Monochrome & restrained** — grayscale canvas, one cool accent used sparingly, Linear/Apple calm. Dark-only. |
| Join-code entry | **Segmented 6-box input** — one box per character, auto-advancing, paste-aware; the code is the hero of the landing page. |
| Broadcaster layout | **Preview-hero, controls float** — the screen preview dominates; live badge, code, and stop float over it; settings tuck behind a gear. |
| Motion | **Subtle & tasteful** — gentle fades, auto-hiding viewer controls, a quiet slow gradient behind the landing box; all gated by `prefers-reduced-motion`. |

## Goals

- **Three production surfaces**, each cohesive with the others:
  - **Landing** (`#/`): a centered glass card — segmented code entry + **Join**,
    with a smaller **Start a stream** affordance below. A friend with a link
    never sees this (they land straight in the viewer); it is the front door
    for people typing a code.
  - **Broadcaster** (`#/broadcast`): pre-start "Start a stream" → screen
    picker → preview fills the view; **LIVE** badge, broadcast **code** +
    copy-link, **Stop**, and a **gear** hiding resolution/framerate/server
    settings float over the preview. R3 ladder + R4 auto/pressure indicators
    render as quiet badges, not full-width colored boxes.
  - **Viewer** (`#/view/<id>`): the decoded stream fills the viewport
    (letterboxed, never cropped). A control bar (**fullscreen**, **leave**)
    auto-hides after inactivity. A **stats overlay** opens on a hotkey **and**
    from a custom **right-click menu**. Connecting / reconnecting / ended /
    error get first-class cinematic states, not raw status text.
- **A small design system**: CSS-custom-property tokens (palette, type scale,
  spacing, radii, motion, glass surfaces) plus a handful of primitives
  (`Button`, `GlassPanel`, `IconButton`, `ContextMenu`) — so the three
  surfaces read as one product.
- **The debug surface survives, frozen**: today's pages move under `#/debug/*`,
  unlinked from the production UI, and stay maintained.
- **Reuse over rebuild** where the seam is clean: `LadderPicker` and
  `ServerSettings` (already store-bound) drop into the broadcaster's gear
  panel; the decoded-frame `drawImage` path and every pipeline/session module
  carry over. The React *page shells* are rebuilt.
- **Accessible and reduced-motion-safe**: keyboard-operable code entry, menus,
  and controls; `focus-visible` rings; motion gated by
  `prefers-reduced-motion`.
- **Zero server / wire / viewer-protocol changes.**

## Non-goals (and why)

- **Live viewer count on the broadcaster.** The relay is a one-way byte
  forwarder by design; the broadcaster has no back-channel for a viewer count,
  and `/statusz` HMAC-obfuscates broadcast IDs (R2) so the broadcaster can't
  even find its own entry. Surfacing a count is a *protocol* addition (a new
  relay→broadcaster signal, sibling to R5's keyframe-request question), not a
  UI change — out of R6's scope. The layout leaves a slot; it renders nothing.
  See Decision 7.
- **Volume / audio controls.** There is no audio in the pipeline yet. Viewer
  controls are fullscreen + leave only; a volume control lands with audio,
  later.
- **Light mode.** The app is dark-first and stays dark-only
  (`color-scheme: dark`). Tokens are structured as custom properties so a
  future light theme is *possible*, but building it is out of scope.
- **Mobile-broadcaster.** `getDisplayMedia` screen-share is a desktop story;
  the broadcaster view targets desktop. The **viewer** and **landing** are
  made responsive down to a phone (a friend watching on their couch), but the
  broadcaster is not optimized for small screens.
- **Router dependency.** Three-ish routes still don't warrant one; the existing
  hand-rolled hash router is extended, not replaced.

## Design language

Dark-only, monochrome, one accent. All values are CSS custom properties in
`global.css` (finalized in J1 — the hexes below are the proposed starting
point, tuned during the polish pass):

```
/* Canvas & surfaces — near-black, layered grays */
--bg:          #0a0b0d;   /* app canvas */
--surface-1:   #121317;   /* raised card */
--surface-2:   #171920;   /* nested / input */
--border:      #262a33;   /* hairline */
--border-soft: #1c2028;

/* Glass — floating chrome over the stream/preview */
--glass:       rgba(18,19,23,0.62);   /* + backdrop-filter: blur(16px) saturate(120%) */
--glass-border:rgba(255,255,255,0.08);

/* Text */
--text:        #e8eaed;
--muted:       #9aa1ad;
--faint:       #626873;

/* The single cool accent — focus rings, the live dot, active states only.
   Proposed: a desaturated azure. Used sparingly; never as a fill for large
   areas. */
--accent:      #5b8def;
--accent-soft: rgba(91,141,239,0.14);

/* Status (kept muted to fit the palette) */
--live:        #e5484d;   /* the LIVE dot only */
--warn:        #d0a215;

/* Type scale (system font stack, unchanged) */
--fs-hero: 2rem;  --fs-lg: 1.25rem;  --fs-md: 0.95rem;  --fs-sm: 0.8rem;  --fs-xs: 0.72rem;

/* Spacing / radius / shadow */
--sp: 0.25rem (× scale);  --r-sm: 6px;  --r-md: 10px;  --r-lg: 16px;
--shadow: 0 8px 40px rgba(0,0,0,0.5);

/* Motion — subtle */
--dur-fast: 150ms;  --dur: 220ms;  --ease: cubic-bezier(0.2, 0, 0, 1);
--control-idle-ms: 3000;   /* viewer control auto-hide */
```

Principles: hairline borders over heavy fills; glass (translucent + blur) for
anything floating over live pixels; the accent appears only on focus, the live
dot, and the active/selected state; motion is short and ease-out, and every
non-functional animation is disabled under `prefers-reduced-motion: reduce`.

## Mockups

Landing (`#/`):

```
                    ╭──────────────────────────────────╮
                    │              gawk                 │
                    │                                   │
                    │          JOIN A STREAM            │
                    │                                   │
                    │     ┌──┬──┬──┬──┬──┬──┐            │
                    │     │A │7 │K │  │  │  │            │
                    │     └──┴──┴──┴──┴──┴──┘            │
                    │       enter the 6-char code       │
                    │                                   │
                    │            [  Join  ]             │
                    │                                   │
                    │      · or ·   Start a stream →     │
                    ╰──────────────────────────────────╯
            (a quiet, slow gradient drifts behind the card)
```

Viewer (`#/view/<id>`) — controls visible, then idle:

```
┌──────────────────────────────────────────────────────────┐
│                                                           │  stream fills the
│                    [ live game frame ]                    │  viewport, letterboxed
│                                                           │  (object-fit: contain)
│  ◦ live                                        ⛶    ⏻     │  ← bar fades after
└──────────────────────────────────────────────────────────┘     ~3s of no pointer
         ⛶ fullscreen (F)      ⏻ leave (→ landing)
```

Viewer — stats overlay (hotkey / right-click) + context menu:

```
┌──────────────────────────────────────────────────────────┐
│ ╭── stats ───────────────╮      ╭──────────────╮          │
│ │ codec    avc1.42E02A   │      │  Stats        │ ← custom │
│ │ dec fps  59.9          │      │  Fullscreen   │   right- │
│ │ dropped  0 / 2 / 0     │      │  Copy link    │   click  │
│ │ queue    1             │      │  Leave        │   menu   │
│ │ latency  3.1 ms        │      ╰──────────────╯          │
│ ╰────────────────────────╯  Ctrl+Alt+Shift+D toggles      │
│                    [ live game frame ]                    │
└──────────────────────────────────────────────────────────┘
```

Broadcaster (`#/broadcast`) — pre-start, then live:

```
   pre-start                          live
╭────────────────────────╮   ┌──────────────────────────────────┐
│    gawk · broadcast     │   │ ● LIVE  A7K2QM ⧉   Auto·720p ⚙ ⏹ │
│                        │   │                                  │
│    ▷  Start a stream    │   │        [ screen preview ]        │
│                        │   │                                  │
│   ⚙ resolution·fps···   │   │                                  │
╰────────────────────────╯   └──────────────────────────────────┘
                              ⚙ panel: [res ▾][fps ▾] server·secret
```

## Status

Chunk prefix **J**, continuing the series (H = R3, I = R4). Each chunk is
independently shippable and green before the next starts. Per CODE-REVIEW.md,
pure logic (code-input parsing, route parsing, auto-hide/fullscreen/hotkey
hooks, status→overlay mapping) is **test-first**; visual results are covered by
the manual pass.

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| J1 | **Design system + routing shell** (`global.css` tokens; `src/ui/` primitives `Button`/`GlassPanel`/`IconButton`/`ContextMenu`; hash-route restructure in `App.tsx`; `features/debug/DebugIndex.tsx`) | Tokens defined per the Design-language spec; primitives render with correct variant classes (render tests). Route parser (pure, unit-tested): `#/`→landing, `#/broadcast`→broadcaster, `#/view/<id>` with a valid 6-char id→viewer, `#/view` or invalid id→redirect `#/`, `#/debug`→index, `#/debug/{broadcast,view,loopback}`→the existing pages (moved by route only, code frozen). No nav chrome on production routes; debug routes get a minimal back-to-index link. `tsc -b` + lint + build green | ✅ done |
| J2 | **Landing page** (`features/landing/LandingPage.tsx` + `.module.css`, `CodeInput.tsx` + `CodeInput.test.tsx`) | `CodeInput` (tests written first): 6 boxes; typing auto-advances; backspace on an empty box steps back; pasting a 6-char code fills all boxes; non-alphabet chars (`BROADCAST_ID_ALPHABET`, uppercased) are rejected; `onComplete(id)` fires only with 6 valid chars. **Join** enabled iff the code is valid → sets `#/view/<id>`. **Start a stream** → `#/broadcast`. Centered, responsive (boxes shrink on narrow widths); keyboard-only join works. lint + build green | ✅ done |
| J3 | **Production viewer** (`features/viewer/ViewerScreen.tsx` + `.module.css`; `lib/useFullscreen.ts`, `lib/useAutoHide.ts` + tests) | Reuses `ViewerSession` **unchanged**; decoded frames paint to a full-viewport canvas (`object-fit: contain`, letterboxed, never cropped) via the existing `drawImage` path. `useAutoHide` (fake-timer test): controls show on mount + on `pointermove`/focus, hide after `--control-idle-ms`. `useFullscreen` (mocked API test): toggle via button and `F`; **leave** → `#/`. Status→overlay mapping (unit-tested) covers connecting / reconnecting (keeps last frame) / ended (terminal card + Home) / error (card + Retry/Home). `#/view` without a valid id redirects to landing. lint + build green | ✅ done |
| J4 | **Viewer stats overlay + context menu** (`features/viewer/StatsOverlay.tsx`, `ContextMenu` usage; `lib/useHotkey.ts` + test; `STATS_HOTKEY` constant) | `useHotkey` (unit-tested): matches `STATS_HOTKEY` (default `Ctrl+Alt+Shift+D`), ignores repeats/held state, no-ops while a text input is focused. Overlay (glass panel) toggles from the hotkey **and** a right-click **Stats** item; shows the live `ViewerStats` fields. Custom context menu over the stream surface `preventDefault`s the native menu; items **Stats / Fullscreen / Copy link / Leave**; closes on outside-click / `Esc`; keyboard-navigable (menu open/close + item dispatch unit-tested). Overlay dismissable via hotkey / `Esc` / menu. lint + build green | ✅ done |
| J5 | **Production broadcaster** (`features/broadcaster/BroadcasterScreen.tsx` + `.module.css`; reuses `LadderPicker`, `ServerSettings`) | Reuses `BroadcastPipeline` **unchanged** and preserves the reclaim→mint fallback logic. Pre-start: centered **Start a stream** + a settings **gear** (res/fps/server/secret settable before sharing, persisted via the existing stores). Live: preview fills; floating **LIVE** badge, **code** chip + **copy-link** (existing `handleCopy` behavior), **gear** panel, **Stop**. R3/R4 indicators (`autoRung`, `autoAtFloor`, `encoderPressure` from `BroadcastStats`, unchanged) render as quiet badges. Viewer-count slot present, renders nothing (Decision 7). lint + build green | ✅ done |
| J6 | **Motion, polish, verification, docs sync** | Transitions (`--dur*`/`--ease`), control fades, the landing gradient — all gated by `prefers-reduced-motion: reduce` → static. `focus-visible` rings on every interactive element; `aria-label`s on icon buttons; glass-chrome contrast checked. Favicon + `<title>` + meta; responsive pass (landing, viewer letterbox, broadcaster). `npm test`, `npm run lint`, `npm run build` green; the manual browser pass below completed; README gotchas + ROADMAP R6 + CLAUDE.md synced | ✅ done (automated gates); manual browser pass pending |

Goal → verified-by:

| Goal | Where | Verified by |
|------|-------|-------------|
| Segmented code entry: type/paste/backspace/reject/complete | `CodeInput` | `CodeInput.test.tsx` |
| Correct route + redirects (incl. id validation) | route parser | route-parser test |
| Controls auto-hide and reveal | `useAutoHide` | fake-timer test |
| Fullscreen toggle (button + `F`), leave → landing | `useFullscreen` | mocked-API test |
| Viewer state overlays (connecting/reconnecting/ended/error) | status→overlay map | unit test |
| Stats overlay opens from hotkey **and** right-click | `useHotkey` + `ContextMenu` | hotkey + menu tests |
| Native context menu suppressed over the stream | `ContextMenu` | menu test + manual |
| Broadcaster reclaim→mint preserved | `BroadcasterScreen` | reuse of existing logic + manual |
| R3/R4 badges reflect `BroadcastStats` | `BroadcasterScreen` | manual (DevTools throttle, per docs/09) |
| Debug pages reachable only at `#/debug/*`, still work | `App.tsx` routing | route test + manual |
| Motion off under `prefers-reduced-motion` | `global.css` / hooks | manual (emulated) |
| Transport/media/wire untouched | whole diff | existing suites stay green |

## Implementation status (2026-07-13)

Chunks J1–J6 are implemented; all automated gates are green (`npx tsc -b`,
`npm test` — 162 tests across 19 files, `npm run lint`, `npm run build`). New
code lives under `src/ui/` (primitives + `Icons`), `src/routing.ts`,
`src/lib/{broadcastId,useAutoHide,useFullscreen,useHotkey}.ts`, and
`src/features/{landing,viewer,broadcaster,debug}/`; the old
`features/stream/*` and `features/loopback/*` pages are untouched and now
reached only via `#/debug/*`. `App.module.css` (the old nav) was removed.
**Zero changes** to `gawk-server`, the wire format, `transport/*` pipelines,
`media/*`, or the Zustand stores — confirmed by the unchanged suites.

As with R4, the *visual* result (monochrome polish, motion, fullscreen, screen
share, real WebTransport/WebCodecs) can only be judged in a real browser — the
manual browser pass below is **pending** and is the acceptance gate for calling
R6 fully done. Nothing here has a headless surface to drive.

**Post-J6 fix — debug viewer hash namespace (2026-07-13).** The frozen debug
`ViewPage` syncs its selected code into `window.location.hash` and auto-joins
from it. It used `#/view/<id>` — which R6 reassigned to the *production* viewer
— so clicking **Watch** in `#/debug/view` rewrote the hash and bounced the user
into the new `ViewerScreen`. Fix: the debug viewer now owns `#/debug/view/<id>`
(regex + write in `ViewPage`, plus `parseRoute` treats `debug/view/*` as
`debug-view`). Lesson for the frozen pages: **any page that writes
`location.hash` is not really route-frozen** — the route move must chase its
self-navigation too. (Also hardened `ViewerScreen` so a StrictMode-discarded
session's late `onEnded` can't null the live session ref.)

**Post-J6 settings refinement (2026-07-13, Decisions 6/6a/11).** The gear panel
was rebuilt to stack fields full-width (the reused debug `ServerSettings` grid
was overflowing the side panel horizontally); server URL + cert hash are now a
localhost-only **Development settings** group; the publish secret moved out of
the panel to a **start-time modal** gated by `config.requirePublishSecret`. New
files: `src/config.ts` (+ `config.test.ts`), `public/config.js`, the
`<script src="/config.js">` in `index.html`, and the gawk-app Helm
`config.requirePublishSecret` value + `templates/configmap.yaml` + a subPath
mount in `deployment.yaml`. `helm template` verified (renders `false` by
default, `true` with `--set`). Still zero `gawk-server` / wire / pipeline
changes.

## Decisions

1. **Routing, and the debug surface.** Root `#/` is the landing page.
   Production broadcaster is `#/broadcast`; production viewer is
   `#/view/<id>`. The existing diagnostic pages move — **by route only, code
   frozen** — under `#/debug/broadcast`, `#/debug/view`, `#/debug/loopback`,
   with a minimal index at `#/debug`. The debug tree is **not linked** from the
   production UI (reachable only by typing the URL) and **does not share
   components** with it (answering the ROADMAP's open question): coupling the
   sleek surfaces to the diagnostic layout would make every debug tweak a
   production risk. `#/view` with no or an invalid id redirects to `#/` — join
   happens on the landing page, which owns code entry. The hand-rolled hash
   router is extended, not replaced by a dependency.

2. **The reuse boundary.** Everything below the React page shell is UI-agnostic
   and carries over verbatim: `BroadcastPipeline`, `ViewerSession`, the
   `transportStore` / `broadcastSettingsStore` / `pipelineStore`, `ladder.ts`,
   `fallback.ts`, `wire.ts`, and the decoded-frame `drawImage` render path.
   `LadderPicker` and `ServerSettings` already bind to the stores, so they drop
   into the broadcaster's gear panel unchanged (restyled via tokens, not
   rewritten). The **page shells** (`BroadcastPage`, `ViewPage`, loopback) are
   *not* edited — they become the debug pages as-is. Production gets a fresh
   component tree under `features/{landing,viewer,broadcaster}` plus shared
   `src/ui/` primitives. `StatsGrid` stays the debug stats layout; the
   production viewer's stats overlay is a new glass component (different visual
   language, same `ViewerStats` data).

3. **A token-based design system, monochrome.** All color/space/motion lives in
   `global.css` custom properties (Design-language section). This supersedes
   the current ad-hoc `--accent` blue for production; the debug pages keep
   working because they reference the same variable names (the palette shifts,
   their layout doesn't). Primitives (`Button` with primary/secondary/ghost/
   danger, `GlassPanel`, `IconButton`, `ContextMenu`) keep the three surfaces
   consistent and are the only place variant styling is defined. Dark-only, but
   authored as properties so a future light theme is a token swap, not a
   rewrite.

4. **Viewer = the stream is the canvas.** The decoded frame fills the viewport
   with `object-fit: contain` (letterboxed on a black field) — game content is
   **never cropped**, unlike a `cover` fit. Fullscreen uses the Fullscreen API
   on the viewer root (button, bottom-right à la Netflix, plus the `F` key);
   `Esc` exits fullscreen via the browser and **leave** returns to the landing
   page. The control bar is visible on entry and on any pointer movement or
   focus, and fades out after `--control-idle-ms` of inactivity
   (`useAutoHide`). All viewer states are driven by the *existing*
   `ViewerSession` callbacks — no new transport behavior; only presentation
   changes (cinematic connecting/reconnecting/ended/error cards vs. today's raw
   status pill). Close code **4000 stays terminal** (ended card, no reconnect),
   exactly as today.

5. **Stats overlay: hotkey *and* right-click, one panel.** The overlay toggles
   from a single named constant `STATS_HOTKEY` (default **`Ctrl+Alt+Shift+D`**,
   honoring the Netflix `Ctrl+Shift+Alt+D` reference the ROADMAP cites — the
   constant is trivially re-bindable) **and** from a **Stats** item in a custom
   right-click menu. The custom `ContextMenu` `preventDefault`s the native menu
   *only over the stream surface* and offers **Stats / Fullscreen / Copy link /
   Leave**; it closes on outside-click or `Esc` and is keyboard-navigable. The
   overlay renders the `ViewerStats` the session already produces (codec,
   decoder fps, dropped incomplete/late, awaiting-keyframe, decoder queue,
   decode latency, datagrams, bad datagrams). **If R5 later lands a
   glass-to-glass / live-edge metric, it slots into this overlay** — R6 does not
   depend on R5 and does not invent the metric.

6. **Broadcaster: preview-hero, settings behind a gear.** Pre-start shows a
   centered **Start a stream** plus a reachable gear. The gear panel is a
   right-side sheet whose fields **stack vertically, full-width** (a side panel
   must never scroll sideways — the flex/grid `min-width:auto` overflow trap):
   a **Stream quality** group (`LadderPicker`) always, and a **Development
   settings** group (server URL, dev cert hash) shown **only on localhost**
   (`isDevEnvironment()` — real users never see relay internals). The panel
   deliberately **does not** reuse the debug `ServerSettings` (its two-column
   grid is what overflowed). Starting calls the unchanged `BroadcastPipeline`
   and keeps the **reclaim→mint** fallback (fresh mint only on
   `phase === 'connect'`). Live, the preview fills and the chrome floats:
   **LIVE** badge, **code** chip + **copy-link**, **gear**, **Stop**. R3/R4
   feedback (`autoRung`, `autoAtFloor`, `encoderPressure`) becomes quiet inline
   badges ("Auto · 720p", an at-floor warning, an explicit-mode pressure
   warning) instead of full-width colored boxes.

6a. **Publish secret is asked at start, gated by a deploy flag — not parked in
   the panel.** A viewer only needs the code; a *broadcaster* needs the relay's
   pre-shared secret (R2) only when the relay runs with `-publish-secret`.
   Whether that is the case is a per-deploy fact the client can't probe (the
   relay is a byte forwarder with no such endpoint, and adding one is out of
   scope), so it is a **frontend runtime flag** `config.requirePublishSecret`
   (Decision 11). When set, pressing **Start a stream** opens a small secret
   modal (pre-filled from the persisted value so returning broadcasters just
   confirm) before the share picker; the entered value is stored in
   `transportStore` exactly as before and never leaves the client except as the
   existing publish query param. When the flag is false (default, and all local
   dev), Start is unchanged. Local testing of a secret-protected relay uses
   `#/debug/broadcast`, which keeps the full `ServerSettings` incl. the secret
   field.

7. **No viewer count (deferred, with a reason).** The layout reserves a slot,
   but it renders nothing. A count needs a relay→broadcaster back-channel that
   does not exist by design (one-way byte forwarder), and `/statusz` HMAC-keys
   broadcast IDs (R2) so the broadcaster can't locate its own row. Adding it is
   a protocol change of the same class as R5's keyframe-request question —
   explicitly outside a UI milestone. Revisit if a back-channel is ever added.

8. **Motion is subtle and always reduced-motion-safe.** Short ease-out
   transitions (`--dur-fast`/`--dur`), the control auto-hide fade, and a slow,
   low-contrast animated gradient behind the landing card. Every non-functional
   animation is disabled under `prefers-reduced-motion: reduce` (the gradient
   goes static, transitions collapse to instant). No heavy page transitions,
   no motion over live video.

9. **Accessibility is in-scope, not a follow-up.** The segmented code input,
   the context menu, and all controls are keyboard-operable; icon-only buttons
   carry `aria-label`s; `focus-visible` rings use the accent; glass-chrome
   contrast is verified against worst-case bright frames in J6. This is cheap
   now and expensive to retrofit.

10. **The controllers/hooks are pure and testable.** `useAutoHide`,
    `useFullscreen`, `useHotkey`, the route parser, and the `CodeInput` parsing
    are written test-first with injected time / mocked browser APIs — the same
    discipline `FpsGate`, `KeyframeCadence`, and `FallbackController` follow.
    Visual polish is covered by the manual pass, not asserted in unit tests.

11. **Deploy-time frontend flags via a Helm-rendered `/config.js`.** The SPA is
    static files behind nginx, so a build-time `VITE_*` env can't carry a
    *deploy*-time choice without rebuilding the image per value. Instead a tiny
    `public/config.js` sets `window.__GAWK_CONFIG__` and is loaded (plain
    `<script>`, before the deferred module bundle) from `index.html`. The
    shipped default is empty; the gawk-app Helm chart renders a ConfigMap from
    its `config.*` values and mounts it over `/usr/share/nginx/html/config.js`
    (subPath), so a `helm upgrade --set config.requirePublishSecret=true`
    changes the flag with no rebuild and **no new server endpoint** (the
    project's stated constraint). `src/config.ts` reads it with typed accessors
    + defaults; only **non-secret, client-safe** flags belong here. This is the
    general runtime-config seam for the frontend, not a one-off.

## Proposed Changes

### `gawk-app`

#### [MODIFY] src/styles/global.css
Add the full token set (Design-language section). Keep the existing variable
*names* the debug pages reference; production reads the new tokens. Add
`prefers-reduced-motion` and `focus-visible` base rules.

#### [NEW] src/ui/ — shared primitives
`Button.tsx` (primary/secondary/ghost/danger), `GlassPanel.tsx`,
`IconButton.tsx`, `ContextMenu.tsx` (+ light render/behavior tests). The only
place variant styling lives.

#### [MODIFY] src/App.tsx (+ a pure `parseRoute` helper + test)
Restructure routing per Decision 1: `#/` landing, `#/broadcast` broadcaster,
`#/view/<id>` viewer (validate id, else redirect `#/`), `#/debug` index,
`#/debug/{broadcast,view,loopback}` the existing pages. Remove the production
nav bar; render it only on `#/debug*`. Extract hash→route parsing into a pure,
unit-tested function.

#### [NEW] src/features/landing/ — `LandingPage.tsx`, `CodeInput.tsx` (+ test), `*.module.css`
Segmented 6-box code entry (Decision-driven behavior), Join + Start-a-stream,
quiet gradient backdrop, responsive.

#### [NEW] src/features/viewer/ — `ViewerScreen.tsx`, `StatsOverlay.tsx`, `*.module.css`
Full-viewport letterboxed canvas reusing `ViewerSession` and the `drawImage`
path; auto-hiding controls; fullscreen; cinematic state cards; stats overlay +
right-click menu. `STATS_HOTKEY` constant lives here.

#### [NEW] src/features/broadcaster/ — `BroadcasterScreen.tsx`, `*.module.css`
Preview-hero broadcaster reusing `BroadcastPipeline` + `LadderPicker`;
reclaim→mint preserved; floating chrome; quiet R3/R4 badges. Gear panel stacks
fields full-width with a localhost-only **Development settings** group (server
URL / cert hash bound straight to `transportStore`; the debug `ServerSettings`
grid is *not* reused — Decision 6). Publish secret is a start-time modal gated
by `config.requirePublishSecret` (Decision 6a).

#### [NEW] src/config.ts (+ config.test.ts), public/config.js, index.html `<script>`
Runtime config seam (Decision 11): `getRuntimeConfig()` / `requiresPublishSecret()`
/ `isDevEnvironment()`. `public/config.js` is the empty default; `index.html`
loads `/config.js` before the bundle.

#### [NEW] src/features/debug/DebugIndex.tsx
Minimal index linking `#/debug/broadcast`, `#/debug/view`, `#/debug/loopback`.
The pages themselves (`features/stream/*`, `features/loopback/*`) are **not
edited** — only reached via the new routes.

#### [NEW] src/lib/ hooks — `useFullscreen.ts`, `useAutoHide.ts`, `useHotkey.ts` (+ tests)
Pure, time/API-injected, test-first.

#### [MODIFY] deploy/ (Helm) — `charts/gawk-app/values.yaml`, `templates/configmap.yaml` (new), `templates/deployment.yaml`
`config.requirePublishSecret` value → ConfigMap `config.js` → subPath mount over
the shipped default (Decision 11). Frontend deploy plumbing only.

### `gawk-server`
No changes. No wire-format changes. R6 is UI-only (see the goals and Decision 7).
The publish-secret *flag* lives in the gawk-app chart; the relay's existing
`-publish-secret` enforcement is untouched.

## Verification Plan

### Automated tests (vitest, test-first where logic exists)
- `CodeInput.test.tsx` — typing/auto-advance, paste-fill, backspace, invalid-char
  rejection, `onComplete` only at 6 valid chars.
- `parseRoute` test — every route + the `#/view` / invalid-id redirects.
- `useAutoHide` / `useFullscreen` / `useHotkey` tests — fake timers / mocked
  Fullscreen + key events (including "ignored while typing").
- Viewer status→overlay mapping test.
- `ContextMenu` test — open/close, outside-click/`Esc`, item dispatch, native
  menu suppression.
- Existing suites (wire, viewer, broadcaster, fallback, ladder, relay) stay
  green — nothing below the UI changed.

### Manual browser verification (the sleek-ness is inherently visual)
On the real setup (Chromium broadcaster + ≥1 viewer, plus a Firefox viewer for
the MSTP-fallback path):

1. **Landing → join**: type a 6-char code (and paste one) → **Join** → lands in
   the viewer; a shared `#/view/<id>` link opens straight into the viewer.
2. **Viewer cinematic**: stream fills the viewport, letterboxed not cropped;
   controls fade after ~3s and return on mouse move; **fullscreen** (button +
   `F`) works and exits; **leave** → landing.
3. **Stats overlay**: `Ctrl+Alt+Shift+D` and right-click → **Stats** both open
   it; numbers update live; `Esc` / re-press closes. Right-click menu also does
   Fullscreen / Copy link / Leave, and the native context menu never appears
   over the stream.
4. **Viewer states**: broadcaster stops → **ended** card (terminal, no
   reconnect); kill+restart the relay → **reconnecting** then recovery;
   bad id / server → **error** card with Retry/Home.
5. **Broadcaster**: **Start a stream** → picker → preview fills; **LIVE** badge,
   **code** + **copy-link** (verify the copied link opens the viewer); gear
   panel changes resolution/framerate mid-stream; **Stop** returns to pre-start.
6. **Settings panel**: open the gear — fields stack, **no horizontal scroll**
   at the panel width; on localhost the **Development settings** group (server
   URL, cert hash) shows; on a deployed origin it is absent.
7. **Publish secret**: with `config.requirePublishSecret=true` (or a local
   `public/config.js` override), **Start a stream** opens the secret modal
   before the picker; a correct secret proceeds, and it persists (next start is
   pre-filled). With the flag false, Start goes straight to the picker.
8. **R3/R4 badges**: under DevTools CPU throttle (per docs/09), the auto rung /
   at-floor / explicit-pressure badges appear as designed.
9. **Debug survives**: `#/debug` lists the three pages; each still works
   (incl. `#/debug/broadcast`'s full `ServerSettings` for local secret tests);
   nothing in the production UI links to them.
10. **Reduced motion + responsive**: emulate `prefers-reduced-motion: reduce`
   (gradient static, transitions instant); check the landing + viewer at a
   phone width.

### Docs to sync on completion (J6)
- `README.md` — status + any new gotcha (e.g. the fullscreen/context-menu
  interaction, the `#/debug` relocation).
- `ROADMAP.md` — R6 row + section status → done.
- `CLAUDE.md` — build-order + directory-structure pointers to `docs/10`.

## Open questions (resolve during implementation, not blockers)
- **Exact accent hex** — the proposed azure is a starting point; finalized
  against real frames in J6 (must stay legible as a focus ring over bright game
  content).
- **`STATS_HOTKEY` default** — `Ctrl+Alt+Shift+D` mirrors Netflix; confirm no
  clash on the broadcaster's browser/OS during the manual pass. It is one
  constant to change if so.
- **Landing gradient treatment** — pure CSS (animated conic/radial) is the
  plan; keep it cheap enough to idle at 0% CPU when tab-blurred.
```
