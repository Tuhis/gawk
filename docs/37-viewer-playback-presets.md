# R32 — Viewer playback presets & settings UX

**Status**: designed + **UX1–UX6 implemented 2026-07-29**; automated gates
green (`gawk-app` 1129 tests / oxlint / `tsc -b` build) and verified live in
Chrome against the production fleet on broadcast `5UP4XW`. Deviations in §13.
**Scope**: `gawk-app` viewer surface only — **zero server, wire, relay,
broadcaster and media-pipeline changes**. No new persisted key, no new stat,
no change to any delivery/parity/striping *mechanism*. R32 moves controls and
adds words; every value it writes is a value the viewer can already hold
today.

---

## 1. The problem

Every milestone from R12 onward added its viewer control to one flat
right-click menu. Nobody removed anything. The worst realistic case today —
live-edge delivery, a stream carrying audio, interpolation available,
non-dev build — is **17 rows**:

| | Row | Kind |
|---|---|---|
| 1–2 | Stats, Fullscreen | action |
| 3 | Paced playback ✓ | tuning (R12) |
| 4–6 | Live edge ✓ / Resilient mode / Deep buffer | tuning (R19/R21) |
| 7–9 | Loss protection: full ✓ / light / off | tuning (R29) |
| 10–12 | Striping: auto ✓ / on / off | tuning (R30) |
| 13 | Frame interpolation (experimental) ✓ | tuning (R12 T4) |
| 14–17 | Mute, Copy link, Terms of use, Leave | action |

**Eleven of seventeen rows are tuning knobs, and every one of them already
ships with the correct default for the average viewer** — live edge, adaptive
pacing, fleet-max parity, auto striping, interpolation on. A first-time viewer
who opens the menu to mute the stream is shown eleven decisions they should
never make in order to find the one they should.

Four specific defects, all read out of the current code rather than inferred:

**1.1 — The menu can run off the bottom of the screen with no way to scroll.**
`ui/ContextMenu.module.css` sets `max-width` but no `max-height` and no
`overflow`, and the placement clamp floors `top` at `PAD` (`ContextMenu.tsx:66`,
`Math.max(PAD, ny)`). At `--fs-sm` (0.8 rem) with the `pointer: coarse`
`0.7rem` row padding, 17 rows come to roughly 740 px of menu. On a phone in
landscape — which is how someone watches a game stream — *Copy link*, *Terms
of use* and *Leave* are below the viewport and unreachable. (Arithmetic from
the CSS; the on-device confirmation is UX1.1's job, and the fix stands whether
or not the rest of R32 ships.)

**1.2 — Irrelevant options vanish instead of graying.** Parity and striping
are filtered out of the array entirely under Resilient/Deep
(`ViewerScreen.tsx:715`, `:742`). The menu changes *length* when the delivery
mode changes, so a viewer who saw "Loss protection" once cannot find it again
and is told nothing about why.

**1.3 — Frame interpolation appears mid-session.** It is gated on
`stats?.interpolation != null` (`:770`), and stats arrive a second or two
after connect. The menu grows a row while the user is looking at it.

**1.4 — The primitive cannot express any of the above.** `MenuItem` is
`{ label, onSelect }` (`ContextMenu.tsx:4-7`) — no `disabled`, no groups, no
submenu, no radio semantics. Checkmarks are string suffixes
(`'Paced playback ✓'`), so there is no `aria-checked`, a screen reader hears a
*changing accessible name* where the state changed, and `ViewerScreen.test.tsx`
(1019 lines) matches on those exact strings.

## 2. Goal

A viewer who has never used gawk before can, without opening anything:

- see what playback mode they are in, and
- change it to a better one for their connection in **two taps**,

while a viewer who wants Striping set to `on` can still get there, and the
owner can still talk a friend through reaching it over the phone.

Concretely: **the tuning surface an average viewer meets drops from 11 rows to
one control**, and the eleven rows do not disappear — they move somewhere they
can be labelled, explained and grayed with a reason.

## 3. The axis, and what is *not* on it

The current menu presents four independent controls. Three of them are not
independent, and one of them is not on the same axis at all. Getting this right
is what makes the preset model honest rather than decorative:

| Control | Axis | Cost of "more" |
|---|---|---|
| Delivery mode (R19/R21) | **latency** | live → ~0.5 s → seconds behind |
| Paced playback (R12) | **latency** | +50–350 ms (adaptive clamp, 150 ms seed) |
| Loss protection (R29) | robustness | ~11 %/~22 % more data. **No latency cost** |
| Striping (R30) | robustness | N extra QUIC connections. **No latency cost** |

**Parity and striping are not on the latency axis.** An earlier sketch of this
item mapped them into the presets ("Lowest latency" = parity off + striping
off) — that is wrong and is recorded here as rejected, because turning them
off does not lower latency by a millisecond. It lowers overhead and raises
loss sensitivity. Their defaults (`auto`/fleet-max) are the right answer for
essentially every viewer; the only reasons to change them are a metered
connection or a middlebox that dislikes N connections, both of which are
genuinely advanced concerns.

So: **presets govern delivery + pacing. Parity, striping and interpolation are
Advanced, stay at their defaults, and are exactly what "Custom" tracks.**

## 4. Owner decisions (2026-07-29)

1. **Presets are promoted to the control bar**, not buried in the menu. A
   viewer must be able to see and change playback mode without opening
   anything.
2. **"Custom" is shown only once the user has actually gone off-preset** — it
   is never offered as something to pick from a clean install.
3. **A real settings panel**, reusing the broadcaster's existing pattern
   (scrim + slide-in `GlassPanel` with `.group`/`.groupTitle` sections,
   `BroadcasterScreen.tsx:372-440`), rather than a nested/submenu context menu.
4. **Options irrelevant to the selected delivery mode are grayed with a
   reason**, never removed (fixes 1.2).
5. **The right-click / "⋮" menu becomes actions-only.**

## 5. Locked decisions

1. **No new persisted key. The preset is derived, never stored.** The five
   existing keys stay exactly as they are — `gawk:playout-mode`,
   `gawk:viewer-delivery`, `gawk:parity-level`, `gawk:stripe-mode`,
   `gawk:interpolation` — and the preset is a *view* computed from them. This
   buys three things: no migration for existing viewers, no possibility of the
   stored preset drifting out of sync with the values it claims to describe,
   and a legacy configuration (e.g. an R19-era viewer on `resilient` with
   pacing off) that keeps working and simply reads **Custom**. A stored preset
   would be a second source of truth for state that already has one, which
   CODE-REVIEW's "one definition, one home" forbids.

2. **A preset is a complete configuration, and picking one resets the advanced
   knobs to their defaults.** All four presets share the same advanced defaults
   (`parity: auto`, `striping: auto`, `interpolation: on`), so "picking a
   preset" reduces to one sentence: *set delivery and pacing, put everything
   else back to normal.* The alternative — orthogonal, sticky advanced values —
   was rejected because it makes the pill label lie: it would read "Balanced"
   while striping was forced off. The cost is real and accepted: a viewer who
   deliberately set striping `off` for a metered link loses it when they switch
   preset. It is bounded (one control, one tap to restore), visible (the pill
   moves back to a preset name), and it is the standard preset semantic.

3. **Custom is a state you land in, not an option you choose.** It appears in
   the popover — checked, and inert on select — exactly when the effective
   tuple (delivery, pacing, parity, striping, interpolation) matches no
   preset. On a clean install it never renders. (A stored `playoutMode:
   'fixed'` used to resolve here too; R32 removed that mode outright — see
   deviation 7 — so it now migrates to `'adaptive'` on load instead.)

4. **Not-applicable is a *disabled row with its reason on the row*, never a
   tooltip and never a removal.** Touch has no hover, and R19's own PRODUCT-2
   finding was that a control reachable only by right-click is unreachable on
   the phones it was built for. A grayed row with no explanation is worse than
   an absent one, so the reason is required text, not decoration.

5. **The panel renders inside the viewer root subtree, never portalled to
   `document.body`.** In CSS pseudo-fullscreen (the iPhone shipping tier —
   docs/21 U4) the fullscreen element *is* `rootRef`, so anything portalled
   out is invisible. The same holds for desktop element fullscreen. This is a
   correctness constraint, not a preference.

6. **The panel does not pause, reconnect, or cover the video.** A viewer
   changes playback mode *in order to judge playback*, so the stream stays
   visible and playing beside the panel. Opening the panel suppresses the
   control-bar auto-hide, exactly as an open menu already does
   (`ViewerScreen.tsx:655-656`).

7. **Reconnects are disclosed, because they are visible.** Delivery mode and
   parity level are in the session effect's dependency array
   (`useViewerConnection.ts:717-721`) — changing either tears the session down
   and re-dials. Pacing, striping and interpolation are separate live-crossing
   effects (`:609-624`) and never reconnect. Any control whose change costs a
   reconnect says so on the row; the others must not, or the disclosure means
   nothing.

8. **Copy lives in one module.** `features/viewer/playbackPresets.ts` owns the
   preset table, the labels, the sub-labels and the not-applicable reasons; the
   panel and the popover *import* those strings. Same rule as docs/30 decision
   8 — no surface inlines a second copy of a label.

9. **Defaults are not re-litigated.** R12's adaptive-pacing-and-interpolation
   default (2026-07-15) and R30 finding 6's striping-on-by-default
   (2026-07-29) both stand; `Balanced` is defined to *be* today's default
   state, so a viewer who installs R32 and touches nothing gets byte-identical
   playback behaviour. R32 changes where controls live, not what they do.

## 6. Design

### 6.1 The preset model (pure)

New `features/viewer/playbackPresets.ts` — no React, fully unit-testable:

```ts
export type PresetId = 'lowest' | 'balanced' | 'smoother' | 'stable';

export interface PlaybackConfig {
  delivery: ViewerDeliveryMode;   // 'live' | 'resilient' | 'deep'
  playout: PlayoutMode;           // 'off' | 'adaptive'
  parity: ParityChoice;           // 'auto' | 1 | 0
  striping: StripeMode;           // 'auto' | 'on' | 'off'
  interpolation: boolean;
}
```

| id | Label | Sub-label | delivery | playout | reconnects from `balanced`? |
|---|---|---|---|---|---|
| `lowest` | Lowest latency | least delay — can judder | `live` | `off` | no |
| `balanced` | **Balanced** | smooth, ~0.2 s behind — default | `live` | `adaptive` | — |
| `smoother` | Smoother | for mobile networks, ~0.5 s behind | `resilient` | `adaptive` | yes |
| `stable` | Most stable | rides out dropouts, seconds behind | `deep` | `adaptive` | yes |

Advanced defaults, shared by all four: `parity: 'auto'`, `striping: 'auto'`,
`interpolation: true`.

The module exports exactly four things beyond the table:

- `resolvePreset(config): PresetId | null` — the preset whose full five-field
  tuple equals `config`, else `null` (⇒ Custom).
- `presetConfig(id): PlaybackConfig` — the complete configuration to apply.
- `advancedChanges(config): number` — how many advanced fields differ from
  their defaults, for the disclosure's "· N changed" marker and to decide
  whether **Reset advanced** is enabled.
- `notApplicable(field, delivery): string | null` — the gray-out rule *and*
  its reason, one function so the panel cannot gray something without saying
  why (decision 4).

`notApplicable` encodes exactly two rules, both already true of the pipeline:

| Field | Not applicable when | Reason shown |
|---|---|---|
| `parity` | `delivery !== 'live'` | "Not used in this mode — it already recovers lost packets by resending them." |
| `striping` | `delivery !== 'live'` | "Not used in this mode — reliable delivery already handles bursts." |
| `interpolation` | effective playout is not `adaptive` | "Needs paced playback — available on Balanced and smoother." |

The interpolation rule reads the **effective** mode, not the stored one — R19
resilient delivery implies adaptive pacing inside `playout.ts`, which is
review finding LIFECYCLE-2's whole point (docs/24 finding 16) and must not be
re-broken by moving the control. When R27 (docs/32) lands and interpolation
becomes reachable in live-edge mode, this rule becomes capability-based and
the third row of the table disappears; nothing else in R32 moves.

### 6.2 UX1 — the `ContextMenu` primitive grows up

`MenuItem` gains three optional fields and the menu gains a scroll clamp:

```ts
export interface MenuItem {
  label: string;
  onSelect: () => void;
  checked?: boolean;     // renders as menuitemradio/menuitemcheckbox + aria-checked
  disabled?: boolean;    // not focusable, not selectable, dimmed
  reason?: string;       // second line, shown when disabled (decision 4)
}
```

- **Checkmarks stop being string suffixes.** `checked` renders the mark and
  sets `role="menuitemradio"` + `aria-checked`, so the accessible name stops
  changing when the state does.
- **Disabled rows are skipped by Up/Down** and ignored by `choose()`; the
  existing `active` index arithmetic (`ContextMenu.tsx:107-115`) must skip
  them rather than land on them.
- **`max-height: calc(100vh - 16px)` + `overflow-y: auto`** on `.menu`, and
  the placement measurement must read the *clamped* height so a menu taller
  than the viewport is positioned at `PAD` and scrolls (fixes 1.1). This is a
  bug fix, so per CODE-REVIEW it is **test-first**: a failing test that renders
  enough items to exceed a stubbed viewport and asserts the last item is
  reachable, before the CSS changes.

UX1 is independently valuable and lands first; the overflow fix should not
wait on the rest of R32.

### 6.3 UX3 — the settings panel

New `features/viewer/ViewerSettingsPanel.tsx`, styles ported from
`broadcaster.module.css:150-249` into `viewer.module.css`. Right-side panel
over the video, scrim behind it, `Done` in the header — the broadcaster's
shape, so there is one settings idiom in the product.

```
┌ Playback ──────────────────── Done ┐
│                                    │
│  PLAYBACK                          │
│   ○ Lowest latency                 │
│       least delay — can judder     │
│   ◉ Balanced                       │
│       smooth, ~0.2 s behind        │
│   ○ Smoother                       │
│       mobile networks, ~0.5 s      │
│       · switching reconnects       │
│   ○ Most stable                    │
│       rides out dropouts           │
│       · switching reconnects       │
│                                    │
│  ADVANCED  ⌄            · 1 changed│
│   Loss protection    [ Full   ▾ ]  │
│     repairs lost packets, ~22 %    │
│     more data · changing reconnects│
│   Striping           [ Off    ▾ ]  │
│     splits bursts when loss is     │
│     detected                       │
│   ☑ Frame interpolation            │
│     (experimental)                 │
│                                    │
│                  [ Reset advanced ]│
└────────────────────────────────────┘
```

- **Advanced is a collapsed disclosure**, controlled (not native `<details>`)
  to match the broadcaster's animated reveal (`BroadcasterScreen.tsx:105`). It
  carries a `· N changed` marker when `advancedChanges() > 0`, which is the
  only thing that tells a viewer at a glance why their pill says Custom.
- **Reset advanced** is disabled at zero changes and restores all three
  advanced fields to defaults. It does not touch delivery or pacing.
- **Gray-out** comes from `notApplicable()`: the control is `disabled`, dimmed,
  and its reason replaces the cost line. The value it *would* have is still
  shown, so nothing is hidden — only inert.

### 6.4 UX4 — the control-bar pill, and the menu reduced

The bar gains one text control at the head of `.actions`, before the audio
controls:

```
● live  👁 3 watching              [ Balanced ▾ ]  🔊 ▂▄▆   ⋮   📊   ⛶   ⏻
```

Tapping it opens the preset popover — **the same `ContextMenu` component**,
anchored `bottom-right` exactly like the existing "⋮" button
(`ViewerScreen.tsx:1012-1018`, including the `anchorRef` outside-click rule
that stops the button re-opening what its own pointerdown closed):

```
┌──────────────────────────────────┐
│ ○ Lowest latency                 │
│ ◉ Balanced                       │
│ ○ Smoother      · reconnects     │
│ ○ Most stable   · reconnects     │
│ ──────────────────────────────── │
│   More settings…                 │
└──────────────────────────────────┘
```

**A pill-plus-popover, not a segmented control.** A four-way segmented control
at `--fs-sm` is ~280 px, which does not fit a phone control bar that already
carries a status pill, a watching badge, a volume slider and four icon
buttons; and it would be a second bespoke widget to build, test and describe.
The pill is one control, identical on mouse and touch, its label *is* the
state readout, and it reuses the menu primitive UX1 already had to grow.
Accessible name is `Playback quality: Balanced` (the visible label drops the
prefix for width).

When the state is off-preset the pill reads **Custom** and the popover shows a
fifth checked, non-interactive Custom row above the separator (decision 3).

The menu (right-click **and** "⋮" — they stay the same menu) becomes:

```
Stats · Fullscreen · Mute† · Playback settings… · Copy link · Terms of use · Leave
                                                      († audio streams only)
```

Seven rows at worst, all actions, plus the dev-only `Relay server (dev)…`
entry which keeps its `isDevEnvironment()` gate.

### 6.5 UX5 — the cross-cutting rules

Three behaviours that span both surfaces and therefore get their own chunk and
their own criteria table, rather than being smeared across UX3 and UX4:

1. **Not-applicable** — every advanced control renders in exactly one of three
   states: enabled, or disabled-with-reason, never absent. The delivery mode
   is the only input.
2. **Reconnect honesty** — the four rows whose change re-dials (Smoother, Most
   stable, any parity change, and returning to a `live` preset from a carrier
   one) carry `· switching reconnects`; nothing else does.
3. **Interpolation stops appearing mid-session.** It renders from first paint,
   disabled with a reason until the pipeline reports it available — which
   fixes 1.3 and, unlike today's `stats?.interpolation != null` gate, tells the
   user something.

## 7. Chunks

| Chunk | What | Depends on |
|---|---|---|
| **UX1** | `ContextMenu`: `checked`/`disabled`/`reason`, disabled-skipping keyboard nav, scroll clamp (**bug fix — test-first**) | — |
| **UX2** | `playbackPresets.ts`: table, `resolvePreset`, `presetConfig`, `advancedChanges`, `notApplicable` (pure) | — |
| **UX3** | `ViewerSettingsPanel.tsx` + ported panel CSS; Playback radio group, Advanced disclosure, Reset advanced | UX2 |
| **UX4** | Control-bar preset pill + popover; menu reduced to actions-only | UX1, UX2, UX3 |
| **UX5** | Not-applicable, reconnect honesty, interpolation from first paint | UX3, UX4 |
| **UX6** | ROADMAP/README/CLAUDE.md sync; browser + on-device verification pass | all |

UX1 and UX2 are independent and can land in either order; UX1 alone fixes the
off-screen-menu bug and is worth shipping on its own if R32 stalls.

## 8. Acceptance criteria

### UX1 — the menu primitive

| # | Criterion | Verified by |
|---|---|---|
| UX1.1 | A menu taller than the viewport is placed at `PAD` and its **last item is reachable** by scrolling | `ContextMenu.test.tsx` — written first, watched fail against today's CSS |
| UX1.2 | `checked` renders `role="menuitemradio"` + `aria-checked="true"`, and the accessible **name does not change** when checked flips | test asserts name equality across a checked/unchecked render of the same label |
| UX1.3 | A `disabled` item is not selectable by click or Enter, and Up/Down **skips** it | test: three items, middle disabled; arrow twice, assert focus lands on the third |
| UX1.4 | A `disabled` item renders its `reason` as visible text | test |
| UX1.5 | An all-enabled menu with no `checked`/`disabled` renders **identically to today** | existing `ContextMenu.test.tsx` stays green unmodified |

### UX2 — the preset model

| # | Criterion | Verified by |
|---|---|---|
| UX2.1 | `resolvePreset` returns `'balanced'` for today's default state, and each preset's own tuple round-trips through `presetConfig` → `resolvePreset` | `playbackPresets.test.ts`, all four |
| UX2.2 | Any single advanced field off its default ⇒ `resolvePreset` returns `null` | test: three one-field mutations of `balanced` |
| UX2.3 | A legacy off-preset combination (`resilient` + playout `off`) resolves to `null`, not to a nearest preset | test |
| UX2.4 | A stored `playout: 'fixed'` migrates to `'adaptive'` on load, in every build (the mode is gone — deviation 7) | `ViewerScreen.test.tsx` |
| UX2.5 | `advancedChanges` counts exactly the deviating advanced fields and **never** counts delivery or playout | test across the 8 advanced combinations |
| UX2.6 | `notApplicable('parity'\|'striping', d)` returns a non-empty reason for `'resilient'`/`'deep'` and `null` for `'live'` | test, all six pairs |
| UX2.7 | `notApplicable('interpolation', …)` is driven by the **effective** playout mode, so a resilient viewer with stored `'off'` gets `null` (LIFECYCLE-2 must not regress) | test mirroring `docs/24` finding 16's case |

### UX3 — the panel

| # | Criterion | Verified by |
|---|---|---|
| UX3.1 | Selecting a preset writes **all five** underlying keys, incl. resetting advanced to defaults (decision 2) | `ViewerScreen.test.tsx` asserts each `localStorage` key after a preset change from a deviating state |
| UX3.2 | Advanced is **collapsed** on open (`aria-expanded="false"`) and shows `· N changed` iff `advancedChanges() > 0` | test |
| UX3.3 | **Reset advanced** restores the three advanced keys and leaves delivery + playout untouched | test |
| UX3.4 | The panel renders **inside** the viewer root element, not a body portal (decision 5) | test asserts DOM ancestry from `rootRef`'s node |
| UX3.5 | Opening the panel suppresses control auto-hide and never unmounts the canvas | test + manual |
| UX3.6 | Every existing persistence behaviour survives the move: each control writes the same key, same values, as before R32 | the existing persistence assertions in `ViewerScreen.test.tsx`, re-pointed at the new surface but **not weakened** |

### UX4 — the pill and the reduced menu

| # | Criterion | Verified by |
|---|---|---|
| UX4.1 | The pill's label equals the resolved preset's label, and **`Custom` on a clean default install never renders** | `ViewerScreen.test.tsx` |
| UX4.2 | After changing one advanced value, the pill reads `Custom` and the popover shows a checked, **inert** Custom row | test: click it, assert no key changed |
| UX4.3 | The popover is the same `ContextMenu` component, anchored `bottom-right`, and the pill dismisses what it opened | test (the `anchorRef` regression from docs/24 finding 9) |
| UX4.4 | The menu contains **no tuning rows** — no delivery, parity, striping, pacing or interpolation entry | test asserts absence by regex over rendered rows |
| UX4.5 | Menu row count ≤ 8 in the worst case (audio + dev build) | test |
| UX4.6 | The pill auto-hides with the control bar and is reachable in CSS pseudo-fullscreen | manual (iPhone) |

### UX5 — cross-cutting

| # | Criterion | Verified by |
|---|---|---|
| UX5.1 | Under `resilient`/`deep`, parity and striping are **present, disabled, and carry their reason** — never removed (fixes 1.2) | `ViewerScreen.test.tsx`, both modes |
| UX5.2 | Under `live`, both are enabled and carry no reason | test |
| UX5.3 | Interpolation renders from **first paint** with `stats == null`, disabled with a reason, and enables when the pipeline reports availability (fixes 1.3) | test: render without stats, assert present+disabled; then supply stats, assert enabled |
| UX5.4 | Exactly the reconnecting controls carry `· reconnects`: the two carrier presets and parity — pacing, striping and interpolation do **not** | test asserts the annotation set, pinned against `useViewerConnection`'s dep array |
| UX5.5 | A preset change that alters only pacing (`lowest` ↔ `balanced`) does **not** re-dial | test asserts the injected session factory was called once across the change |

### UX6 — docs

| # | Criterion | Verified by |
|---|---|---|
| UX6.1 | ROADMAP R32 row → status + doc link; CLAUDE.md item added; README gotcha list updated if UX1 adds one | grep/manual |

## 9. Verification plan

| # | Step | How |
|---|---|---|
| 1 | Automated gates | `npm test && npm run lint && npm run build` in `gawk-app` (the build is the only real typecheck — CODE-REVIEW) |
| 2 | Default install shows `Balanced`, menu has 7 rows, no tuning row | `gawk-app:verify` (headless Chrome) |
| 3 | Two-tap preset change works and `lowest ↔ balanced` does not reconnect | `gawk-app:verify`, watching the status dot |
| 4 | Switching to `Smoother` reconnects once and the pill follows | `gawk-app:verify` |
| 5 | Under `Smoother`, parity + striping are visibly gray **with reasons** | `gawk-app:verify` |
| 6 | Advanced change ⇒ pill reads `Custom`; Reset advanced ⇒ back to a preset name | `gawk-app:verify` |
| 7 | **iPhone, portrait and landscape**: pill tappable, popover fully on-screen, panel scrolls, all rows reachable in CSS pseudo-fullscreen | manual on device — the surface R32 exists for, and not CI-reachable |
| 8 | The 17-row menu's overflow is gone (UX1) even with every row forced visible | manual, dev build |

Step 7 is the one that matters most and the one no gate can run: docs/24
finding 9 (PRODUCT-2) is precisely the case of a viewer control that passed
every automated check and was unreachable on a phone.

## 10. Pre-registered fallback

If, on the device pass, the panel proves worse than what it replaced — the
owner's own troubleshooting flow ("switch striping on") measurably slower than
today's menu, or the preset abstraction unable to represent a state a real
viewer reaches — R32 falls back to the **two-tier menu**: keep UX1 and UX2,
drop the panel and the pill, and nest the tuning rows under
`Playback ▸` / `Advanced ▸` drill-downs in the existing menu. That fallback
keeps the two defects worth fixing regardless (1.1 overflow, 1.2 vanishing
rows) and costs only UX3/UX4. Recording the fallback now is what makes the
device pass able to fail.

## 11. Named risks

- **Picking a preset discards advanced choices** (decision 2). Accepted and
  bounded, but it *will* surprise someone once. Mitigated by the `· N changed`
  marker making the deviation visible before they switch.
- **Phone support gains a step.** "Open the menu, set Striping to on" becomes
  "open the menu, Playback settings, Advanced, Striping". One extra step, on a
  surface that is far easier to describe over a phone than a 17-row scrolling
  menu. Accepted; the alternative (dev-gating the advanced knobs) was rejected
  precisely because the owner's remote-troubleshooting flow is load-bearing in
  this project's history.
- **One more control in a bar designed for restraint** (R6 J4). Mitigated: the
  pill auto-hides with everything else and is text, not chrome.
- **Test churn.** `ViewerScreen.test.tsx` is 1019 lines and matches on exact
  label strings including `'✓'` suffixes. The rule for the rewrite is that
  every *behavioural* assertion (which key is written, with what value, and
  whether the session re-dialled) survives; only the selector changes. A
  criterion (UX3.6) exists to stop the churn quietly weakening coverage.

## 12. Non-goals

- **Any mechanism change.** Zero server, wire, relay, broadcaster and
  media-pipeline changes. R32 writes only values the viewer can already hold.
- **Re-litigating defaults.** R12's adaptive+interpolation default and R30
  finding 6's striping-on-by-default stand (decision 9).
- **Auto-detect / the suggest-banner.** R19 Decision 11's "your connection
  looks lossy — switch to Smoother?" banner stays deferred. R32 is its
  prerequisite, not its replacement: once presets exist, the banner has one
  thing to suggest instead of four knobs to set. It is the natural follow-up
  and should be a separate item.
- **The frozen `#/debug/*` surfaces.** Untouched, as always.
- **The broadcaster surface.** Its settings panel is the pattern being
  borrowed, not changed.
- **A stats-overlay redesign.** The overlay stays exactly as it is; it remains
  the ground truth and the Copy-diagnostics path.

## 13. Implementation status & deviations (2026-07-29)

UX1–UX6 implemented. Gates: `npm test` (1129 passed / 75 files), `npx oxlint`
(clean), `npm run build` (`tsc -b` — the only real frontend typecheck, per
CODE-REVIEW). Live-verified in Chrome against the production fleet
(`https://api.gawk.ioio.fi:4433`) on broadcast `5UP4XW`, driving the real
viewer: default pill reads **Balanced**, the popover renders four presets with
the mark on Balanced and no Custom row, the panel opens with Advanced
collapsed, a striping change flips the pill to **Custom** with `· 1 changed`,
**Reset advanced** returns it to Balanced, switching to **Smoother**
renegotiates delivery (`gawk:viewer-delivery` → `resilient`, stream keeps
playing) and grays Loss protection + Striping *with their reasons*.

Files: `ui/ContextMenu.tsx` + `.module.css` (UX1),
`features/viewer/playbackPresets.ts` (UX2),
`features/viewer/ViewerSettingsPanel.tsx` + panel/pill styles appended to
`features/viewer/viewer.module.css` (UX3/UX4), and `ViewerScreen.tsx` wiring.

### Deviations from the design above

1. **The reconnect annotation is dynamic, not a fixed pair.** §6.1's table and
   UX5.4 named "the two carrier presets" as the rows carrying
   `· switching reconnects`, which is only true when read from the default
   (live-edge) state. As implemented, a preset row is annotated when *its*
   delivery differs from the *current* delivery — so from Smoother it is the
   other three that reconnect and Smoother itself that does not. This is
   strictly more accurate against `useViewerConnection`'s dependency array,
   which is what decision 7 says the annotation must track. The criterion was
   re-pointed accordingly.

2. **`MenuItem` gained a fourth field, `note`.** §6.2 specified `checked`,
   `disabled` and `reason`; a *disabled* row's reason and an *enabled* row's
   cost line ("smooth, ~0.2 s behind — default") are different things and both
   are needed, so `reason` stayed disabled-only and `note` was added for the
   enabled case. One render path picks between them.

3. **The accessible name is wired with `aria-labelledby`/`aria-describedby`.**
   Not specified either way. Rendering the second line as plain content made
   every row's accessible name a sentence — `getByRole('menuitemradio', {name:
   'Balanced'})` could not match, which is the test-visible symptom of a real
   navigation problem. Name is now the label alone, the second line is the
   description. The check mark is a CSS `::before` driven by
   `[aria-checked='true']`, so no `'✓'` ever enters `textContent` (UX1.2).

4. **Interpolation availability is a tri-state, not a boolean.** UX5.3 said
   "disabled with a reason until the pipeline reports it available". Passing
   `interpolationAvailable: boolean | null` keeps *unknown* apart from *no*:
   before the first stats sample the panel says "Available once the stream is
   running", and only a reported `interpolation: null` says the pipeline can't
   do it. Collapsing them would tell a healthy viewer, for the first second of
   every session, that its browser cannot do something it can.

5. **The two menus close each other explicitly.** ContextMenu's
   outside-`pointerdown` listener already handles pointer input, but a
   *keyboard* activation fires `click` with no `pointerdown` at all — so Enter
   on the pill left both the preset popover and the "⋮" menu on screen. Each
   opener now nulls the other's state. (Found while driving the live browser
   programmatically, which dispatches bare `click`s — the same shape as
   keyboard activation.)

6. **Menu ceiling: 7 in production, 9 in a dev build.** §6.4 said "seven rows
   at worst … plus the dev-only entry"; there are two dev-only entries (the
   retired fixed-playout diagnostic and the relay override). UX4.5's test now
   pins the production ceiling explicitly by forcing `isDevEnvironment()`
   false, since that is the number that matters.

7. **The `'fixed'` playout mode was removed outright** (owner decision,
   2026-07-29), superseding docs/17 Decision 10's "retire it from the menu but
   keep the mode". As first implemented, R32 left its dev-only "Smooth playback
   (fixed 150 ms)" entry in the "⋮" menu on the reasoning that a diagnostic is
   not a preset — but that put a *pacing* row in a menu R32's own headline rule
   makes actions-only, enforced by a test that only ran with
   `isDevEnvironment()` false. So the rule held for real viewers and quietly
   not for dev builds, and the row rendered indented among flat action rows
   because it carried the radio check gutter. The mode's stated value was as a
   measurement-free control separating a pacing bug from a bug in the jitter
   estimator driving it (PLAYOUT-1, docs/24 finding 8) — real, but not reached
   for since, and not worth an exception to the one rule that keeps this menu
   from growing back. `PlayoutMode` is now `'off' | 'adaptive'`; a stored
   `'fixed'` migrates to `'adaptive'` in every build; `PLAYOUT_OFFSET_MS`
   stays as adaptive's warmup seed. UX4.4 now asserts no pacing row in *any*
   build.

8. **The test cleanup helpers now clear all five preference keys.** They
   previously cleared three, and one test compensated by removing
   `gawk:viewer-delivery` by hand. Harmless while each control was read
   independently; with the pill *derived* from all five, a value left behind by
   an earlier test changes what the next one renders. The leakage predates R32
   — the fix is order-independence, not new coverage.

### Not verified, and why

- **UX4.6's on-device pass (iPhone, portrait + landscape).** Not run — the
  device is the owner's. The desktop-Chrome run covers the flows but not the
  coarse-pointer targets, the CSS pseudo-fullscreen containment (decision 5),
  or the panel's scroll behaviour on a short viewport.
- **UX1.1 in a real browser.** The clamp is asserted in jsdom against a stubbed
  1400 px menu and a 768 px viewport (written first, watched fail), and the
  live run confirms the wiring is active — `max-height: 897px` = `innerHeight −
  16`, `overflow-y: auto`. What was *not* reproduced in Chrome is the original
  overflow itself: the production menu is now 7 rows and no longer tall enough
  to overflow any realistic viewport, which is the fix working, not a gap in
  it. The attempt to shrink the window for a direct check failed — the
  extension's resize did not change the page viewport (`innerWidth` stayed
  1600) — so this stays a unit-test-backed claim.
