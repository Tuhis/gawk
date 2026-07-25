# R26 — Quick-start broadcast links (docs/31)

**Status**: designed 2026-07-25, **not started**. Chunks **QL1–QL6** (`QL` =
Quick Link; two-letter prefix per the R21+ convention — `TC`/`NA`/`MF`/`DV`
are claimed). Ships as `gawk-app-vX.Y.Z`; **frontend-only, zero server / wire /
broadcaster-protocol changes** — no new relay code, no new chart value, no new
runtime-config key.

---

## 1. Purpose

Starting a broadcast today costs four interactions from a cold browser:
navigate to the app → **Start a stream** → (first time: terms → secret) →
share picker → pick a surface. Two use cases make that friction worth
removing:

- **Bookmark**: one bookmark on the gaming PC that goes from "cold browser" to
  "the share picker is open, at 720p30" in a single click.
- **Embed**: a "Start a stream" link on another page or internal tool
  (a homelab dashboard, a Discord pin, a wiki page) that lands the visitor
  ready to broadcast rather than on a landing page they must navigate.

R26 adds a **query-parameter grammar on the existing `#/broadcast` route** and
a **primed one-click surface** that consumes it. Everything downstream of the
click — connect, reclaim→mint, capture, encode, the LIVE stage — is reached
through the existing `beginStart` entry point and is byte-identical.

### 1.1 The floor is one click, and it is browser-enforced

**`getDisplayMedia` requires transient user activation.** No link, bookmark,
PWA shortcut, or `autoplay`-style attribute can open the share picker on page
load — a fresh document has neither transient nor sticky activation, and
navigation does not carry activation across documents. The codebase already
records this the hard way: `media/capture.ts` (the R15 finding-1 comment
block) explains that the video-only retry after an audio refusal usually
*cannot re-prompt*, because "the seconds the user spent in the picker have
already spent" the activation.

So R26 does not promise zero clicks and no design in this doc pretends
otherwise. What it removes is **every click that is not the capture gesture**,
and **every decision**:

| Scenario | Today | With an armed link |
|---|---|---|
| Returning device, no publish secret | 2 clicks (Start, then picker) | **1** |
| Returning device, secret required + stored | 3 clicks (Start, confirm secret, picker) | **1** (D4) |
| Fresh device, terms not yet accepted | 3 clicks | **1** (D5 — the Agree click *is* the gesture) |
| Fresh device, secret required, none stored | 4 clicks | **2** |
| Non-default quality wanted | above + open Settings, two pickers | **unchanged count** — the link carries them |

The last row is the one that matters most in practice: a link encodes a
*quality choice* that otherwise costs a settings-panel visit every time the
broadcaster wants something other than their saved default.

---

## 2. Decisions (locked with the owner, 2026-07-25)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **The publish secret is never carryable in a link.** No `?secret=`, in the hash or anywhere else. | Owner choice. A link is a thing that gets bookmarked, synced across devices, pasted into chat, and screenshotted; a publish credential must not inherit that lifecycle. Deployments with `-publish-secret` still work — the secret prompt appears once per device and is stored (`gawk.publishSecret`) exactly as today, after which armed links are one-click (D4). This also keeps R26 free of any security-review surface. |
| D2 | **Link settings are a session-only override, never persisted.** | A bookmarked `res=480` "quick share" link must not silently rewrite the 1080p default the broadcaster uses for gaming. The link drives *this* broadcast; the settings panel reflects the link's values so the UI stays honest; `localStorage` is untouched. A **manual** change made during a link session persists as normal — a deliberate edit is still a preference. |
| D3 | **The armed link renders a dedicated minimal surface**, not the existing prestart card. | One purpose, one button, clickable without reading. Also visually distinct, so a link arriving from someone else's page does not look like the normal app with a button pre-pressed. |
| D4 | **No pre-connect: the relay is dialed on the click, as today.** An armed link that is opened and abandoned costs the relay nothing. | Preserves connect-before-picker ordering (`broadcaster.ts:388`) exactly. Pre-connecting would hold one of the relay's **5** default broadcast slots for the GC grace period (5 min) per abandoned tab — five stray bookmark tabs would lock out the deployment. The cost of this choice is that the async dial sits inside the activation window; §4.4 makes that a measured acceptance criterion rather than a hope. |
| D5 | **Unmet gates are presented on load, not behind the button.** An armed link with unaccepted terms opens the terms modal immediately; its "Agree & continue" click is the capture gesture. | Keeps the *streaming* click count at one in every case. The gate is never bypassed or weakened — R23's rule ("nothing touches the transport until the broadcaster has accepted") holds verbatim; only its position in the click chain moves. |
| D6 | **The link grammar can express nothing the settings panel cannot.** Every parameter maps onto an existing validated vocabulary and an existing setter. | Prevents a second, drifting configuration surface. It also means every value is already tested and already clamped. |
| D7 | **Invalid or unknown parameters are ignored, never fatal.** They are collected and shown as a quiet note. | A link is hand-authored and long-lived; a typo three months later must degrade to "streams at the default" and not to "cannot stream". |
| D8 | **A "Copy quick-start link" builder ships with the feature** (QL5). | A URL grammar with no UI is a feature only its author can use. The builder is what makes the bookmark use case real for a non-author, and it is the discovery surface for the whole milestone. |

---

## 3. Where it plugs into the existing app (grounded in the code)

Four existing seams carry the whole design. Nothing else is touched.

**`src/routing.ts` — `parseRoute`.** Today it strips a leading `#`, a leading
`/`, and trailing `/`, then string-matches the path. A query is not
anticipated: `#/broadcast?start=1` currently falls through to
`{ view: 'redirect', to: HOME }`. QL1 splits the query off before matching,
which also hardens every other route (`#/view/ABC123?utm_source=…` resolves to
the viewer instead of bouncing home — a latent bug fixed in passing).

**`src/state/broadcastSettingsStore.ts`.** Every setter writes `localStorage`
then `set()`. D2 needs a second path that only does the `set()`. The store is
already the single source of truth read by `encoderSettingsFromStore()` and by
`BroadcasterScreen`'s `handleStart` (`useBroadcastSettingsStore.getState()`),
so **an override placed in the store reaches the pipeline with zero pipeline
changes**.

**`src/features/broadcaster/BroadcasterScreen.tsx` — `beginStart`.** The
existing entry point already sequences terms → secret → `handleStart`
(lines ~259–284). The armed surface calls **`beginStart` and nothing else**;
reclaim→mint, `BroadcastStartError.phase` handling, and the LIVE stage are
untouched. This is the load-bearing containment decision: **R26 changes what
the pre-start card looks like and what the settings store holds, and changes
nothing at or after the click.**

**`src/features/terms/acceptance.ts` + `config.requiresPublishSecret()`.** Read
on load by the armed surface to decide which gate (if any) to present
immediately (D5).

**`src/features/broadcaster/captureGuidance.ts` (R24, docs/30).** R24 landed
"Sharing tips" on the pre-start card — `WHOLE_SCREEN_TIP` plus a
capability-branched `AUDIO_TIP` — and they are advice about **the very next
action**: which surface to pick, and ticking "Share audio" *in the picker*.
An armed link is precisely the flow that drops the user into that picker with
the least context, so a minimal surface that silently omits them would make
the fast path the *uninformed* path — worst for the first-time visitor
arriving from an embedded link, who is exactly the person the tips were
written for. **Resolution: minimal ≠ tipless.** The armed surface renders the
R24 tips under R24's own rule — *unless already dismissed* (`shouldShowHint`,
persisted per key in `localStorage`). A returning bookmark owner has dismissed
them and gets the genuinely minimal card D3 asks for; a first-timer gets the
guidance. No new dismissal state, no second copy of the strings — the tips are
imported from `captureGuidance.ts` like every other surface.

---

## 4. Data model & mechanism

### 4.1 Route and query grammar

Parameters live in the **hash**, after the route path:

```
https://gawk.example/#/broadcast?start=1&res=720&fps=30
```

The hash is chosen over a real search string (`/?start=1#/broadcast`) for
three reasons: it keeps the app's single-hash-route model intact; the fragment
is **never sent to the server** and **never appears in a `Referer` header**,
so link contents stay off nginx logs and off any page the visitor clicks
through to; and it composes naturally with the existing `#/view/<id>` shape.

`parseRoute` splits on the first `?`. The route match is unchanged; the query
string rides along on the broadcaster route:

```ts
| { view: 'broadcaster'; query: string }   // '' when absent
```

Other routes discard the query but must not be broken by its presence
(explicitly tested in QL1).

Parsing and validation live in a new pure module `src/lib/startLink.ts` —
separate from `routing.ts`, which stays about view selection:

```ts
export interface StartLinkParams {
  armed: boolean;                        // ?start=1
  overrides: LinkOverrides;              // validated, possibly empty
  ignored: string[];                     // 'res=4k', 'foo' — for the D7 note
}
export function parseStartLink(query: string): StartLinkParams;
```

### 4.2 Parameter vocabulary (D6)

| Param | Accepted values | Maps to | Source of truth |
|---|---|---|---|
| `start` | `1` | arms the surface | — |
| `res` | `auto` \| `native` \| `1080` \| `720` \| `480` | `resolutionSelection` | `RESOLUTION_SELECTIONS` |
| `fps` | `auto` \| `native` \| `60` \| `30` \| `5` | `framerateSelection` | `FRAMERATE_SELECTIONS` |
| `hw` | `auto` \| `hardware` \| `software` | `hwPreference` | `HW_PREFERENCES` |
| `bitrate` | Mbps, e.g. `8` or `2.5` | `bitrateOverride` (bps) | `clampBitrateOverride` → [0.5, 50] Mbps |
| `codec` | e.g. `avc1.42E02A` | `codecOverride` | `DEFAULT_CODEC_PREFERENCES` |

Notes that keep this honest:

- The vocabularies are **imported, not restated**. A future rung added to
  `media/ladder.ts` is accepted by links the same day, and a rung removed
  stops being accepted — no second list to forget.
- `bitrate` is in **Mbps**, unlike the store's bps, because links are
  hand-written. `clampBitrateOverride` already bounds the result, so an absurd
  value clamps rather than being rejected.
- `start` without any override is the plain "start immediately with my saved
  defaults" link — use case 1's simplest form.
- Overrides **without** `start` (`#/broadcast?res=720`) are honored as a
  preset on the normal prestart card. Useful, and it keeps `start` meaning
  exactly one thing: *arm the one-click surface*.

### 4.3 Session-only override (D2)

One new store action, no other change:

```ts
// Applies link-borne values for this page session only. Deliberately does
// NOT write localStorage: a link drives this broadcast, it does not rewrite
// the broadcaster's saved defaults (docs/31 D2).
applyLinkOverrides: (o: LinkOverrides) => void;
```

Consequences, all intended: the settings panel shows the link's values (the
pickers read the store); `encoderSettingsFromStore()` feeds them to the
pipeline unchanged; a reload of a bare `#/broadcast` reverts to saved
defaults; and a manual picker change during the session goes through the
normal persisting setter.

### 4.4 Gate pre-resolution and the activation budget

**On load**, an armed link resolves gates in R23's order and stops at the
first unmet one:

1. Terms not accepted for the current version → present the terms modal now
   (D5). "Agree & continue" → `proceedStart()`.
2. `requiresPublishSecret()` and **no stored secret** → present the secret
   prompt now. Submit → `handleStart()`.
3. `requiresPublishSecret()` and a **stored** secret → **skip the prompt**
   (D4/D1). Today's flow always asks so a returning broadcaster can "just
   confirm"; on an armed link that confirmation is exactly the click the link
   exists to remove. A *wrong* stored secret now fails at connect instead of
   at a prompt — recoverable, and the error card's "Try again" path already
   handles it.
4. Otherwise → render the primed button.

**The activation budget is a first-class risk, not a footnote.** Chrome's
transient activation window is ~5 s, and between the click and
`getDisplayMedia` there are **three** async stages, not two — the first sits
in `createBroadcastSession` (`workerBroadcastSession.ts:349-379`), before
`pipeline.start()` is ever called (`broadcaster.ts:388-416`):

```
click → createBroadcastSession()
          ├ new Worker(broadcaster.worker.ts)
          ├ waitForBoot(worker, BOOT_TIMEOUT_MS)   … hard 2 s worst case
          └ probeTrackTransfer(worker)             … R11 capability gate
      → pipeline.start()
          ├ connectTransport()                     … WebTransport dial (network)
          └ refreshMatrix()                        … R13 isConfigSupported probes
      → startMedia() → getDisplayMedia()           ← needs the activation
```

This risk exists today and R26 does not add to it — but R26 makes it *visible*,
because a one-click surface is exactly where a lost activation reads as "the
link is broken".

**Which stage dominates is unknown until measured**, and the doc deliberately
does not pre-commit to a fix. What can be said about each lever now:

- **Worker boot** has the only *hard* worst case in the chain —
  `BOOT_TIMEOUT_MS` is 2 s, i.e. 40 % of the budget can be spent before a
  packet is sent. It is also the **safest thing to move to page load**: unlike
  pre-connecting (D4) it holds no relay slot, and unlike probing it allocates
  no encoder contexts. If any stage is pre-warmed, start here.
- **The dial** is the stage that cannot move — pre-connecting is exactly what
  D4 rejected, and for a good reason that has not changed.
- **The probes** are the *least* attractive lever despite being the obvious
  one. `MAX_CONCURRENT_PROBES` is 4 precisely because "on Chrome every pending
  call holds a real encoder instance (software probes at 4K allocate full
  encoder contexts) — the broadcaster surface requests ~170 combos at load,
  and unbounded parallelism OOM-crashed the tab (field bug, 2026-07-15)". So
  moving probes to page load re-enters the exact scenario that produced that
  crash; it is bounded now, but the resource cost is real and the codebase's
  standing preference (`useCodecMatrices`) is "not probing at all is still
  better". If probes are pre-warmed anyway, it must be **on the armed path
  only** — never on every `#/broadcast` visit — and it is safe from a
  *correctness* standpoint, since the prober memoizes by combo key, so a
  settings change or a refined source-dims key simply re-probes. Note also
  that the main-thread `EncoderSupportProber` singleton in `useSupportMatrix`
  **cannot** serve the worker pipeline's prober (separate realm, separate
  memo), so this means starting the pipeline's probe earlier, not sharing a
  cache — which in turn means booting the worker early, i.e. the first lever.

The mitigation that ships regardless of the measurement:

- **Recover from it**: a capture-phase `NotAllowedError` on the armed surface
  gets a specific message ("The browser needed a fresh click to open the
  screen picker") and a Try again button, rather than the generic "Couldn't
  start". The pipeline has already torn down (`start()`'s capture-phase catch),
  and `broadcastId` + the resume token are still held, so Try again reclaims
  rather than orphaning viewers. This also covers the case the budget can
  never fully close: a broadcaster who clicks and then looks away.

---

## 5. Chunk breakdown & acceptance criteria

### QL1 — Route query parsing + the parameter grammar (pure)

`parseRoute` splits the query; `src/lib/startLink.ts` parses and validates it.

- Table-driven tests for every parameter: valid, invalid, absent, duplicated.
- Every route that parses today still parses identically with **no** query.
- `#/view/ABC123?x=1` resolves to `{ view: 'viewer', broadcastId: 'ABC123' }`
  (today it redirects home — a fixed latent bug).
- Unknown keys and invalid values land in `ignored[]` and never throw (D7).
- Vocabularies are imported from `media/ladder.ts` / `media/probe.ts` /
  `media/types.ts`; a test asserts an unlisted rung is rejected, so a
  hand-copied list can't drift in later (D6).

### QL2 — Session-only settings override

`applyLinkOverrides` in `broadcastSettingsStore`.

- After apply, `encoderSettingsFromStore()` and the store reflect the values.
- **`localStorage` is asserted untouched** — the test that gives D2 teeth.
- A subsequent manual setter call *does* persist.
- Empty overrides are a no-op.

### QL3 — The primed one-click surface

A new pre-start view inside `BroadcasterScreen`, reached only when
`armed && status === 'idle' && !onStage`.

- Renders brand, one primary button, and a one-line summary of what will
  happen (only non-default values named, e.g. "720p · 30 fps").
- Renders the R24 sharing tips when `shouldShowHint` says so, imported from
  `captureGuidance.ts` — asserted both ways (shown when undismissed, absent
  once dismissed), so the fast path can never become the uninformed one.
- Clicking it calls **`beginStart`** — asserted, so the terms/secret/reclaim
  chain can never be re-implemented alongside it.
- The D7 note renders when `ignored` is non-empty and never blocks the button.
- **A bare `#/broadcast` renders byte-identically to today** — asserted.

### QL4 — Gate pre-resolution

§4.4's on-load resolution and the armed-link secret skip.

- The click-count table in §1.1 is asserted case by case.
- **The terms gate is never bypassed**: an armed link with unaccepted terms
  cannot reach `handleStart` without an acceptance — the R23 invariant, tested
  from the armed path specifically.
- Cancelling a pre-resolved modal falls back to the primed surface (not a
  dead end, not an auto-retry).
- A capture-phase `NotAllowedError` renders the activation-specific recovery.

### QL5 — "Copy quick-start link" builder (D8)

A button in the broadcaster settings panel, beside the existing Terms link —
the settings panel because that is where the *intent* is formed: you have just
picked 720p/30, and the button right there says "bookmark this exact setup".
Label it **"Copy quick-start link"**, with the existing `handleCopy` toast
pattern (`copied` state, 1800 ms). The panel is reachable from the LIVE stage
too, where the topbar's separate **Copy join link** icon copies `#/view/<id>`;
the two are different artifacts (one starts *your* broadcast, one joins it), so
the labels must stay explicit and neither may be abbreviated to "Copy link".

- Serializes the **current** selections into `#/broadcast?start=1&…`.
- **A value is emitted when it differs from the app's built-in default**
  (`'auto'` on both ladder axes, `'auto'` acceleration, no bitrate/codec
  override) — **not** when it differs from the broadcaster's *stored*
  preference. The distinction is load-bearing and the reason this bullet is
  spelled out: a link built against stored values would emit nothing on a
  machine whose owner had already saved 720p, and would then resolve to
  *whatever the recipient happens to have stored* — so the same link would mean
  different things on different machines, breaking the embed use case
  (§1) precisely when it is shared, which is the only time it matters.
- Consequently `#/broadcast?start=1` (everything at app defaults) means **"no
  opinion on quality"** and correctly resolves to the local user's own stored
  preferences. A deliberate choice pins itself; an absence of choice stays
  absent. An embedder wanting determinism pins the values explicitly, which is
  exactly what the builder produces once anything is selected.
- Round-trip test: `parseStartLink(build(state)).overrides` ≡ the
  differs-from-app-default part of `state`, asserted with a store whose
  *stored* values are non-default, so reading (b) above fails the test.
- **A test asserts the builder never emits `secret`** (D1) — the guard against
  a well-meaning future addition.

### QL6 — Docs, embed guidance, verification

- README gotcha: the one-click floor and *why* (§1.1), so it is never
  re-litigated as a bug.
- Embed guidance (§7) in the README.
- The §8 verification pass, including the click→picker measurement.

---

## 6. Non-goals

- **Zero-click autostart.** Browser-enforced (§1.1). An operator on their own
  PC can additionally launch Chrome with
  `--auto-select-desktop-capture-source="Entire screen"` (the sibling of the
  tab-capture flag R20's harness uses) to remove the picker *dialog* — the
  click remains. Documented as an operator trick, never a product feature and
  never a default.
- **A stable, bookmarkable broadcast ID** ("my link always gives code
  `ABC123`"). Blocked on two independent grounds: R17 requires a relay-minted
  **resume token** for *all* `/publish/{id}` claims, so a client cannot choose
  its own ID; and carrying such a token in a link is a publish credential,
  which D1 rules out for the same reasons as the secret. Persistent
  per-broadcaster channels were already deferred by R1 as needing an identity
  story the ephemeral model avoids — this stays that milestone's job.
- **The publish secret in a link** (D1).
- **Viewer-side link parameters** (delivery mode, playout, muted-on-join). The
  same `parseRoute` query split makes them a small follow-up; R26 deliberately
  does not design them, because the viewer's join link is a *shared* artifact
  whose parameters would travel to other people's devices — a different
  question that deserves its own thinking.
- **Auto-stop, scheduled, or unattended broadcasts.** A link starts a
  broadcast a human is present for.
- **Native broadcaster (R14) parity.** It already takes CLI flags. Aligning
  its flag vocabulary with §4.2 would be a nice consistency pass, not work
  R26 owns.

---

## 7. Embedding guidance (use case 2)

The supported shape is a **plain link**, optionally `target="_blank"`:

```html
<a href="https://gawk.example/#/broadcast?start=1&res=720&fps=30">Start a stream</a>
```

**Iframes are not the recommended shape**, and the doc says so rather than
staying silent, because it is the first thing an embedder will try:

- `getDisplayMedia` in a cross-origin iframe requires
  `allow="display-capture"` on the `<iframe>` element; without it, capture
  rejects with `NotAllowedError` and the surface looks broken for a reason
  that is invisible from inside.
- Third-party storage partitioning means the embedded app gets a **separate**
  `localStorage`: terms acceptance, the publish secret, and quality defaults
  are not shared with the same user's top-level gawk. A returning broadcaster
  is treated as a fresh device every time.
- Asking someone to share their screen inside a frame owned by another page is
  a poor trust story regardless of whether it works.

QL3 can cheaply detect `window.self !== window.top` and mention the
`allow="display-capture"` requirement in the capture-failure message — the
only iframe-specific code in the milestone.

---

## 8. Verification plan

1. Cold bookmark, returning device, no secret: one click → picker → LIVE.
2. Same link on a fresh profile: terms modal appears **on load**; Agree →
   picker (still one click). Terms acceptance is recorded.
3. Secret-required deployment: first visit prompts once; second visit from the
   same bookmark is one click.
4. `#/broadcast?start=1&res=480&fps=5`: the LIVE stage's "sending" readout and
   the overlay show 480p/5 — and after stopping, a bare `#/broadcast` shows the
   **saved** defaults, not 480p/5 (D2's observable proof).
5. `#/broadcast?start=1&res=4k&foo=bar`: streams at defaults, note lists both
   ignored params.
6. Click→`getDisplayMedia` measured under 2 s (§4.4); record the number in the
   implementation-status section.
7. Wrong stored secret on an armed link: fails at connect with a readable
   error and a working Try again.
8. Bare `#/broadcast` and every `#/debug/*` surface unchanged.
9. Builder round-trip: copy a link from the settings panel, open it in a fresh
   profile, confirm the same selections apply.

## 9. Deferred / open

- **Pre-warming a click-path stage** — only if §8 step 6 misses its budget,
  and in the order §4.4 gives (worker boot first, probes last and armed-path
  only, the dial never). Recorded here so the levers and their costs are known
  before they are needed, rather than reached for under pressure.
- **Viewer link parameters** — see §6.
- **Telling the embedder the join code** (postMessage back to an opener, a
  `?next=` redirect carrying the code). Genuinely useful for the "internal
  tool" case and deliberately out of v1: it is a cross-origin messaging design
  with its own trust questions, and the code is on screen and copyable the
  moment the broadcast starts.
- **R20 harness simplification.** Tier 1's browser-broadcaster step could
  arm the surface with a link instead of clicking through. Optional — the
  harness's value is that it drives the *production* flow, so the click-through
  is arguably the point.
