# R24 — Broadcaster capture & audio guidance

**Status**: designed 2026-07-24; **CG1–CG5 implemented 2026-07-24, automated
gates green** (gawk-app tsc/vitest/oxlint/build). Frontend-only — **zero
server / wire / broadcaster-protocol / media-pipeline change**. Ships *words*
(and a small amount of dismissible reactive UI state), not pipeline changes,
exactly as the ROADMAP R24 sketch requires.

## Goal

The production broadcaster surface should tell a first-time streamer *what to
pick in the share picker and why* — **whole screen vs. a single window**, and
**how to actually get audio through** — in a few short lines they can read
once, without ever slowing down an experienced broadcaster who already knows.

Today both are tribal knowledge. The pre-start card says only "Share a screen
or window; you'll get a code to hand out," so:

- a broadcaster picks a **window**, alt-tabs into their fullscreen game, and
  streams a frozen rectangle (or nothing — an exclusive-fullscreen game is
  frequently not capturable as a window at all); and
- a broadcaster expects **sound** and gets silence, because system audio needs
  the picker's "Share audio" checkbox ticked (Chromium) — or because they are
  on a browser that cannot do audio at all (Firefox), which nothing tells them.

These are the two most common **self-inflicted** broadcast failures. Both are
already documented — in `CLAUDE.md`, in
[docs/20 field finding 1](20-system-audio.md) — nowhere the person starting a
broadcast will ever look. The facts are settled; what is missing is that they
reach the surface where the decision is made.

## Why now

It is the cheapest possible follow-up to R15 (system audio). The audio path's
dominant failure mode is **not a bug we can fix in code** — it is a picker
choice made without information, or a browser that structurally lacks the
feature. R15 already graduated to always-on (docs/20 "Graduation",
2026-07-23): the production broadcaster requests system audio on *every* start
and `capture.ts` degrades gracefully to video-only. So the machinery is done;
the only thing left is telling the human which lever to pull, and being honest
when there is no lever to pull (Firefox).

**Reconciliation with the ROADMAP sketch.** The R24 idea was captured
2026-07-23, the same day R15 graduated by *removing* the "Enable audio
(experimental)" toggle. The sketch's "two-step: enable audio in advanced
settings **and** tick the picker box" is therefore stale in its first half:
there is no gawk-side audio toggle any more. The design below reflects the
shipped reality — **gawk always requests audio, so the only step the user owns
is the picker checkbox** — which makes the advice *simpler*, not more complex.

## Cross-browser reality (the load-bearing fact)

Audio is **Chromium-only in practice today**. The broadcaster audio lane needs
both `AudioEncoder` (WebCodecs) and `MediaStreamTrackProcessor`; Firefox
exposes neither, and its `getDisplayMedia` has no system-audio source. The
codebase already has the exact predicate — `audioLaneSupported()` in
`media/audio-lane.ts`. R24 keys **every** browser-aware decision on that
predicate (never on the `audioState` string — see the note below), and
surfaces the distinction *before* the mistake and *at the right words*:

| Browser class | Can do audio? | What the guidance says |
|---|---|---|
| Chromium (Chrome/Edge/Brave/…), Windows | yes, incl. system audio | "tick **Share audio** in the picker" |
| Chromium, macOS / Linux screen or window share | no system-audio source | "share a **browser tab** to carry its sound" |
| Chromium, any OS, **tab** share | yes (internal mirroring, no OS loopback) | "share a tab — its audio comes through everywhere" |
| Firefox / other non-MSTP browser | **no — video only** | "audio isn't supported here; use a Chromium browser for sound" |

The distinction is drawn by **feature detection, never UA sniffing**
(`audioLaneSupported()`), consistent with the project's capture principle
(trust capabilities, not metadata) and with every other cross-browser branch
in the app. The macOS/Linux-vs-Windows split *within* Chromium cannot be
feature-detected up front (`getDisplayMedia` only fails at grant time), so it
is stated as guidance ("system audio works on Windows; elsewhere share a tab")
and confirmed **reactively** after a refusal via the already-existing
`audioState: 'unavailable'` signal — we never *claim* a platform capability we
haven't observed.

**A note on `audioState` vs. `audioLaneSupported()` (corrected after review).**
It is tempting to think "Firefox → `audioState: 'unsupported'`", but that is
**not** what the pipeline produces. `startAudioLane` (broadcaster.ts) checks
`!track` *before* the `audioLaneSupported()` check, and Firefox's
`getDisplayMedia` yields no audio track — so a Firefox broadcaster lands on
`'no-track'` (or `'unavailable'`), never `'unsupported'`. The `'unsupported'`
state requires an audio *track present* while `AudioEncoder`/MSTP are absent, a
combination no real broadcaster browser produces. This is exactly why R24 gates
its browser-aware copy and note-suppression on `audioLaneSupported()` (a
main-thread capability check) and **not** on the `audioState` string: the
string cannot distinguish "Firefox, can't do audio" from "Chromium, box
unticked" — both are `'no-track'`. CG4 additionally teaches the stats overlay
this same truth, so a Firefox broadcaster's overlay reads an honest "Not
supported here" instead of the misleading "No audio shared".

## Locked decisions

1. **Words + a little dismissible reactive state; nothing else.** Zero server,
   wire, broadcaster-protocol, or media-pipeline change. The two reactive
   signals R24 needs already exist and are read where they already live:
   `stats.audioState` (already in `BroadcastStats`, already shown in the stats
   overlay) and the capture surface (`displaySurface`), read UI-side from the
   preview `sourceStream` the screen already holds. No new stat, no plumbing.

2. **Never adds a step or a click to starting a broadcast.** The "Start a
   stream" button stays exactly one click → (terms gate, once) → picker. The
   terms acknowledgment (R23) remains the *only* pre-connect gate. All R24
   guidance is **passive**: pre-start tips are collapsed card content the user
   may open; reactive notes appear only once a broadcast is already live. No
   modal, no wizard, no interstitial, no confirmation. This is a hard
   requirement from the item's framing ("must not complicate starting the
   stream any further").

3. **Out of the way of experienced users.** The pre-start "Sharing tips" is a
   **collapsed-by-default** disclosure — a single quiet line an experienced
   broadcaster's eye skips and a first-timer can open. It does not auto-expand,
   does not persist an "opened" flag, and imposes nothing. Each reactive note
   is **dismissible and its dismissal is remembered** (localStorage, `gawk:*`
   convention), so a broadcaster who is *deliberately* sharing a window /
   streaming without audio dismisses it once and is never nagged again. This is
   a **conscious trade** (see decision 4): persisting the dismissal serves the
   stated "don't get in the way of experienced users" priority at the cost of
   not re-warning a *forgetful* repeat mistake. That cost is bounded — the
   pre-start tips and the always-on stats-overlay Audio section remain — and
   the user's priority is explicit, so persistence wins over per-broadcast
   re-nagging (which would pester exactly the deliberate window-sharer the
   priority protects).

4. **Reactive over proactive where the two compete — for the *first* mistake.**
   The highest-value guidance fires *after* the mistake, on the live surface,
   because that is when it is specific and actionable ("you're sharing a
   window", "you didn't tick Share audio"). The pre-start tips are the cheap,
   ignorable, before layer; the reactive notes catch the **first-time** mistake
   — up until the broadcaster explicitly dismisses one, at which point we defer
   to their demonstrated knowledge (decision 3). The notes are therefore an
   *onboarding* aid, not a durable guard rail; the stats overlay's Audio
   section (R15 N6) stays the always-on ground truth for anyone who wants it.

5. **`displaySurface` is an advisory categorical hint only.** It is read to
   choose *words*, never to configure the encoder or compute layout — so the
   docs/01 "trust the `VideoFrame`, not `getSettings()`" rule (which is about
   frame dimensions feeding the pipeline) is not in tension. `displaySurface`
   has no `VideoFrame` equivalent, and `undefined` (some browsers don't
   populate it) is treated as "unknown → no hint", which is always safe.

6. **Firefox is never nagged about audio it cannot have.** A browser where
   `audioLaneSupported()` is false gets the honest pre-start line ("video
   only; use Chromium for sound") and — via CG4 — an honest "Not supported
   here" in the stats overlay's Audio State row (derived from
   `audioLaneSupported()`, not from the raw `audioState`, which would read the
   misleading "No audio shared"). But it gets **no runtime audio-missing
   note**, because there is no action to offer. Runtime audio-missing notes
   fire *only* where audio is actually achievable (`audioLaneSupported()` true
   and the outcome was `'no-track'` or `'unavailable'`).

8. **One definition, one home (CODE-REVIEW).** `captureGuidance.ts` imports the
   `audioState` union as `BroadcastStats['audioState']` rather than
   re-declaring its six members, and re-exports `audioLaneSupported` rather
   than re-deriving the capability. All guidance copy lives as named constants
   in `captureGuidance.ts`; CG2's disclosure, CG3's notes, and CG4's echoes
   *import* those strings — no surface inlines a second copy.

7. **Frozen-canvas / static-window detection is out.** Detecting "this window
   share stopped updating" reliably means frame-diffing, and a false positive
   ("your stream looks frozen") on a legitimately static screen is worse than
   silence. R24 instead nudges *toward the whole screen* whenever a **window**
   is shared (the categorical, false-positive-free signal), and leaves
   frozen-frame detection as a possible future item.

## Non-goals

- Platform/OS detection heuristics that promise more than we can verify
  (beyond `displaySurface`, which is a browser-reported category, and the
  reactive `'unavailable'` observation).
- Frame-diff "your stream looks frozen" detection.
- Automating or pre-selecting the picker (browsers deliberately forbid it).
- Per-OS screenshot walkthroughs to maintain.
- Re-litigating R15's audio architecture, or the always-on default. R24 ships
  *words*, not pipeline changes.
- The native broadcaster (`gawk-broadcast`) — it has its own share/audio story
  (R14/R25) and no browser picker.

## Design

Everything lands in `gawk-app`, in and around
`features/broadcaster/BroadcasterScreen.tsx`, plus one new pure helper module
and its test. The frozen `#/debug/*` broadcast surface is untouched (it keeps
`DEFAULT_CAPTURE_CONFIG`, audio absent — no guidance applies).

### CG1 — the guidance model (pure, tested)

A new `features/broadcaster/captureGuidance.ts` centralizes the *decisions*, so
the copy and the branch logic are unit-testable without React:

- `audioGuidanceForBrowser()` → `'chromium' | 'unsupported'`, computed from
  `audioLaneSupported()` (the single source of truth; re-exported, not
  re-derived).
- `audioReactiveNote(audioState, audioSupported)` → a `{ text }` or `null`.
  Returns a note only when `audioSupported` and `audioState ∈ {'no-track',
  'unavailable'}`; `null` for `'active' | 'off' | 'error' | 'unsupported'` and
  for any state when audio is unsupported (decision 6).
- `captureSurfaceNote(displaySurface)` → a note only for `'window'`; `null` for
  `'monitor' | 'browser' | undefined` (decision 5).
- The dismissal-key constants (`gawk:hint-audio-missing`,
  `gawk:hint-window-share`) and tiny `isHintDismissed` / `dismissHint`
  helpers, following `terms/acceptance.ts`'s try/catch-guarded localStorage
  idiom (private-mode safe: a storage failure just means the hint may re-show,
  never a throw on the broadcast path).

Copy lives here as constants so it is reviewed in one place.

### CG2 — pre-start "Sharing tips" (collapsed disclosure)

On the pre-start card, below the Start button and above the footer, a single
quiet control: **"Sharing tips"** (a `▸`/`▾` disclosure). Collapsed by
default. Opening it reveals two short points inline (no modal):

- **Whole screen vs. window.** "Share your **whole screen** for fullscreen
  games — a single window can freeze when you alt-tab or the game goes
  fullscreen. Pick a window only to show one app and keep the rest private."
- **Audio** — browser-aware:
  - Chromium: "Audio is captured automatically — just tick **Share audio**
    (or **Share tab audio**) in the picker. System audio works on Windows; on
    macOS or Linux, share a **browser tab** to carry its sound."
  - Firefox/other: "Audio isn't supported in this browser — you'll stream
    video only. Use a Chromium-based browser (Chrome, Edge) for sound."

Placement is deliberate: it lives **inside the `idle` branch**, next to the
Start button (the `connecting`/`stopping`/`error` branches render their own
minimal content and are not the moment to teach). The picker has not opened
yet, so the advice is still actionable (decision 4 / ROADMAP placement note).
The disclosure adds one skippable line to the card and **nothing** to the Start
path.

### CG3 — reactive live notes (the safety net)

While broadcasting (the live stage), a small stack of dismissible info notes
sits just under the topbar (reusing the existing `note`/`badge` styling, not a
modal). At most two, each shown only when relevant and not previously
dismissed:

- **Audio-missing** — when `audioLaneSupported()` and `stats.audioState ∈
  {'no-track', 'unavailable'}`:
  - `'no-track'`: "Streaming without audio — 'Share audio' wasn't ticked in the
    picker. Stop and start again with the box checked to include sound."
  - `'unavailable'`: "Audio couldn't be captured on this system. Share a
    **browser tab** to include its sound, or continue video-only."
- **Window-share** — when `displaySurface === 'window'`: "You're sharing a
  single window. If your game runs fullscreen it may look frozen to viewers —
  share the whole screen to be safe."

Each has an `×`; dismissing writes the localStorage key so it never nags that
broadcaster again (decision 3). Neither auto-hides — they describe a
correctable condition and should stay until acted on or dismissed.

`displaySurface` is read UI-side from the preview stream `BroadcasterScreen`
already holds, on both the main-thread and worker paths (the worker path's
`onSourceStream` delivers the main-thread `getDisplayMedia` stream). The read
is fully **optional-chained** —
`sourceStream?.getVideoTracks?.()[0]?.getSettings?.().displaySurface` — so a
teardown-race with no track, or a test fake without `getVideoTracks`, yields
`undefined` (→ no hint), never a render-time throw (review finding 6). No stat,
no message, no wire change.

### CG4 — compact echoes (settings + stats overlay honesty)

Two small read-only echoes of the browser-aware status:

- **Settings panel** — a small **Audio** note under the Advanced group ("Audio
  is captured automatically; tick 'Share audio' in the picker" on Chromium /
  "Audio isn't supported in this browser — video only" on Firefox). This is the
  ROADMAP's "echoed compactly in the advanced settings' audio row", adapted to
  the post-graduation reality (there is no audio *setting*, so it is an
  informational line, not a control).
- **Stats overlay** — `BroadcasterStatsOverlay` gains an `audioSupported` prop
  (passed from `BroadcasterScreen`, computed once via `audioLaneSupported()`).
  When it is false, the Audio "State" row reads **"Not supported here"**
  regardless of the raw `audioState` — fixing the misleading "No audio shared"
  a Firefox broadcaster sees today (review finding 1). When it is true the row
  is unchanged. This is the smallest honest fix and keeps the overlay
  consistent with the guidance layer.

### CG5 — docs / README sync

Update the ROADMAP R24 row to done, add the consolidated gotcha (the
audio-is-Chromium-only fact and the picker-checkbox step) to the README gotcha
list per the CLAUDE.md keep-in-sync rule, and record the implementation status
here.

## Acceptance criteria

### CG1 — guidance model

| # | Criterion | Verified by |
|---|---|---|
| CG1.1 | `audioGuidanceForBrowser()` returns `'chromium'` iff `audioLaneSupported()`, else `'unsupported'` | `captureGuidance.test.ts` (both, MSTP/AudioEncoder faked present/absent) |
| CG1.2 | `audioReactiveNote` returns a note only for `('no-track'\|'unavailable')` **and** `audioSupported`; `null` otherwise | test: full 6-state × supported/unsupported matrix |
| CG1.3 | `'no-track'` and `'unavailable'` yield **different** copy (the two are not interchangeable) | test asserts the two texts differ and each mentions its cue ("Share audio" vs "browser tab") |
| CG1.4 | `captureSurfaceNote` returns a note only for `'window'`; `null` for `'monitor'`, `'browser'`, `undefined` | test: all four inputs |
| CG1.5 | dismissal helpers are private-mode safe (a throwing localStorage never propagates), and the two keys are distinct | test with a `localStorage` stub that throws; assert key constants differ |

### CG2 — pre-start tips

| # | Criterion | Verified by |
|---|---|---|
| CG2.1 | The disclosure is **collapsed by default**; the card renders no tip body until opened | `BroadcasterScreen.test.tsx` |
| CG2.2 | Opening shows the whole-screen line and the **browser-correct** audio line (Chromium vs unsupported) | test with `audioLaneSupported` faked both ways |
| CG2.3 | Toggling the tips disclosure fires **no** transport/capture call (the Start path is untouched) | test: render, click the disclosure, assert the injected session factory was never called |
| CG2.4 | No new blocking element on the start path (no modal/dialog added by R24) | code review + manual |

### CG3 — reactive notes

| # | Criterion | Verified by |
|---|---|---|
| CG3.1 | On Chromium with `audioState:'no-track'`, the audio note renders; dismissing it hides it and sets the localStorage key | `BroadcasterScreen.test.tsx` |
| CG3.2 | On Firefox (`audioLaneSupported` false), **no** audio note renders for any `audioState` | test |
| CG3.3 | With `displaySurface:'window'`, the window note renders; `'monitor'`/`'browser'` render none | test (mock a `sourceStream` track whose `getSettings` returns each surface) |
| CG3.4 | A previously-dismissed note (key present in localStorage) does not re-render | test seeds localStorage, asserts absence |
| CG3.5 | **Happy path renders zero notes**: `audioState:'active'` + `displaySurface:'monitor'` shows neither note | `BroadcasterScreen.test.tsx` (the key UX-constraint assertion) |
| CG3.6 | The two dismissals are **independent**: dismissing the audio note leaves the window note visible, and vice-versa | test dismisses one, asserts the other still present |
| CG3.7 | Notes are not modal and do not block the video/topbar controls | manual |

### CG4 — settings echo

| # | Criterion | Verified by |
|---|---|---|
| CG4.1 | Settings panel shows the browser-correct audio info line | `BroadcasterScreen.test.tsx` (panel open) |
| CG4.2 | With `audioSupported=false`, the stats overlay Audio State row reads "Not supported here" even when `audioState` is `'no-track'`/`'unavailable'` | `BroadcasterStatsOverlay.test.tsx` |

### CG5 — docs

| # | Criterion | Verified by |
|---|---|---|
| CG5.1 | ROADMAP R24 row → done + doc link; README gotcha added | grep/manual |

## Adversarial design review (2026-07-24)

Reviewed against `CODE-REVIEW.md` and the two stated UX constraints ("don't get
in the way of experienced users", "don't complicate starting the stream").

1. **"Does anything here add a step to Start?"** — No. CG2 is collapsed card
   content (a disclosure the user may ignore); CG3/CG4 render only *after*
   start or *inside* the settings panel. The only pre-connect gates remain the
   terms modal (R23) and the secret prompt (R2), both untouched.
   *Resolution:* acceptance CG2.3/CG2.4 pin this with a test and a review line.

2. **"Nag risk for experienced users."** The original sketch's auto-expanded
   tips would show every first visit; a permanent window-share nag would fire
   on every deliberate window share. *Resolution:* pre-start tips collapsed by
   default (decision 3); reactive notes dismissible with **remembered**
   dismissal (localStorage). Worst case for a veteran: one `×` click per
   note, once, ever.

3. **"Can a note fire on the happy path?"** An audio-active broadcast
   (`audioState:'active'`) or a whole-screen share (`'monitor'`) must show
   nothing. *Resolution:* CG1.2/CG1.3 enumerate the full state space; the note
   functions return `null` on every happy-path input, tested exhaustively.

4. **"Is `audioLaneSupported()` the right proxy on the worker path?"** The
   audio lane runs in the worker, but the predicate is checked main-thread.
   Chromium exposes `AudioEncoder`+`MSTP` in *both* scopes and Firefox in
   *neither*, so the main-thread check is faithful; a worker-only-WebCodecs
   browser is not known to exist. *Resolution:* accepted with the caveat
   recorded here; the reactive layer (which reads the *actual* `audioState`
   the pipeline computed) corrects any mismatch after the fact.

5. **"`getSettings()` is banned by docs/01."** That rule governs frame
   dimensions feeding the encoder. `displaySurface` is a categorical UI hint
   with no `VideoFrame` equivalent and never touches the pipeline.
   *Resolution:* decision 5 draws the line explicitly; `undefined` is handled
   as "no hint".

6. **"Stale-closure / effect-timing bugs" (CODE-REVIEW React rule).** The
   guidance reads live values (`stats.audioState`, `sourceStream`) during
   render, not inside an acting effect, and performs no dial/join/navigate — so
   the R1 stale-closure class does not apply. Dismissal is an explicit click
   handler. *Resolution:* no acting effects added.

7. **"One authoritative signal."** Audio state has exactly one producer —
   the pipeline's `stats.audioState`. R24 does not compute a second audio
   truth; it only *renders* that one (and, for the surface hint, the
   orthogonal `displaySurface`). *Resolution:* no competing signal introduced.

8. **"Copy that lies."** The macOS/Linux "system audio doesn't exist" fact
   can't be known before grant time, so the pre-start line says "works on
   Windows; elsewhere share a tab" (advice, not a claim) and the *reactive*
   `'unavailable'` note only appears once the browser has actually refused.
   *Resolution:* we never assert a capability we haven't observed (decision 6,
   cross-browser table footnote).

**Independent second-pass review (subagent, against the written doc + the
codebase).** It confirmed the load-bearing mechanics — `audioState` reaches the
UI on both paths, `displaySurface` is readable on both paths,
`audioLaneSupported()` is an honest proxy, and the `'no-track'`/`'unavailable'`
copy split matches `capture.ts` semantics — and raised seven findings, all
folded in above:

- **F1 (medium-high, fixed):** the doc wrongly claimed Firefox surfaces
  `audioState: 'unsupported'`. It does not (`!track` wins first → `'no-track'`).
  Corrected in "Cross-browser reality" and decision 6; CG4 now makes the
  overlay honest via `audioLaneSupported()` rather than the raw state.
- **F2 (medium, resolved as a conscious trade):** persisted dismissal makes the
  reactive layer a one-shot, not a durable guard rail. Decisions 3–4 reworded
  to own this — the notes are an onboarding aid; persistence is chosen because
  the user's explicit priority is not nagging experienced users.
- **F3 (medium, fixed):** test-mapping holes closed — `'unavailable'` copy
  (CG1.3), dismissal-key independence (CG3.6), a rendered happy-path zero-notes
  assertion (CG3.5), and CG2.3 made a hard assertion.
- **F4 (low-med, noted):** Firefox reaching a live video-only broadcast is
  pre-existing R15 always-on behaviour; the doc no longer *asserts* it —
  manual case 4 confirms it.
- **F5 (low, fixed):** one-definition added as decision 8 (import the
  `audioState` union; copy as shared constants).
- **F6 (low, fixed):** the `displaySurface` read is optional-chained (CG3).
- **F7 (low, fixed):** pre-start tips placed in the `idle` branch (CG2).

Verdict carried in: sound to implement, with F1/F2/F3 fixed first — done.

## Verification plan

**Automated (gates, every criterion above with a test):**

```sh
cd gawk-app && npm test && npm run lint && npm run build
```

`npm run build` (`tsc -b`) is the real typecheck (CODE-REVIEW note). New tests:
`captureGuidance.test.ts` (CG1) and additions to `BroadcasterScreen.test.tsx`
(CG2–CG4).

**Manual (browser, post-merge):**

1. Chromium, whole-screen share, "Share audio" ticked → audio active, **no**
   reactive notes; Sharing tips collapsed by default, opens to the Chromium
   audio copy.
2. Chromium, window share, box unticked → window-share note **and** audio
   `no-track` note both appear; dismiss each, reload, start again → neither
   returns.
3. Chromium on macOS/Linux, screen share with box ticked → capture succeeds
   video-only (the R15 retry), audio `unavailable` note appears with the
   "share a tab" wording.
4. Firefox → Sharing tips shows the "video only / use Chromium" copy; a live
   broadcast shows **no** audio note; settings echo matches.

## Implementation status (2026-07-24)

CG1–CG5 implemented. `captureGuidance.ts` + test added; `BroadcasterScreen`
grew the collapsed pre-start disclosure, the reactive note stack, and the
settings audio line; ROADMAP + README synced. Automated gates green. Manual
browser verification (the four cases above) pending — it needs Chromium on
Windows *and* on a no-system-audio OS, plus Firefox, so it is owner-run.
Deviations from this doc, if any, are recorded here on implementation.
