# R16 — iOS Native Fullscreen: `<video>` Presentation Surface

Design doc for [ROADMAP R16](../ROADMAP.md#r16--ios-native-fullscreen)
(designed 2026-07-16; U1–U3 implemented 2026-07-16, automated gates green;
**U4 verdict 2026-07-19: the native path does not work on iPhone** — native
fullscreen enters but shows a **black video** across three on-device passes,
and the decoded-frame clone tee (no canvas readback) did not cure it. Per the
pre-registered U4 criteria the native tier is **rejected**: pseudo-fullscreen
(CSS) is the shipping path — see “U4 findings”, BUGS.md). Makes the viewer's fullscreen button
actually work on iPhone — today it is a **silent no-op** — by teeing the
already-presented canvas output into a `MediaStreamTrack` (via the
worker-only `VideoTrackGenerator` API) that feeds a hidden, pre-armed
`<video>` element, whose `webkitEnterFullscreen()` is the *only* native
fullscreen that exists on iPhone.

Two constraints anchor the design (user decisions, 2026-07-15/16):

1. **On every device that has the Element Fullscreen API, nothing changes.**
   Byte-identical behavior: no video element in the DOM, no new worker
   messages, no tee, the render sink construction exactly as today. The
   whole feature is gated on the *absence* of `Element.requestFullscreen` —
   effectively an iPhone signature (iPad has had it since iPadOS 16.4).
   (Sole deliberate exception, overlay-only: the new Feature Gates stats
   section reports the gate's state on every viewer — Decision 9.)
2. **The R12 smoothing stack survives.** Paced presentation, adaptive
   offset, and frame interpolation keep painting the canvas exactly as
   today; the `<video>` element is a *presentation layer* fed from the
   canvas *after* those sinks have done their work — not a replacement
   render path. (The earlier direct-`VideoTrackGenerator`-sink shape, which
   sacrificed R12 on iOS, is rejected — see Rejected.)

## Goal

An iPhone viewer taps the fullscreen button (or menu item) and gets real,
chrome-free native fullscreen with the smoothed (paced/interpolated) stream,
rotating to landscape like any video app; exiting returns to the inline
viewer seamlessly. On devices where the capability probes fail, the button
still does something visible (CSS pseudo-fullscreen) — a silent no-op stops
being a reachable state on *any* device. Desktop Chrome/Firefox and iPad
behavior is untouched.

## Background: why the button does nothing on iPhone

`useFullscreen.ts` calls `ref.current?.requestFullscreen?.()`. On iPhone
Safari `Element.requestFullscreen` **does not exist** — WebKit ships the
Fullscreen API for arbitrary elements on iPadOS only — so the optional
chaining swallows the whole thing. Since every iOS browser (Chrome and
Firefox included) is WebKit underneath, this is every iPhone, not just
Safari. And the viewer paints to a `<canvas>` (an OffscreenCanvas
transferred into the viewer worker, R8 S6), so there is no `<video>` to
call iPhone's one native-fullscreen API on.

Platform facts the design rests on (researched 2026-07-15):

- **iPhone**: no Element Fullscreen (tracked publicly for years; iPad-only
  since 16.4). `HTMLVideoElement.webkitEnterFullscreen()` is the only
  native fullscreen; it requires a **user gesture** and a video with loaded
  media (readyState ≥ metadata), and it does not fire `fullscreenchange` —
  state travels on `webkitbeginfullscreen`/`webkitendfullscreen`. A
  `display: none` video breaks it (hide with size/position instead).
  `playsinline` is required to keep normal playback inline.
- **WebTransport arrived on iOS in Safari 26.4 (March 2026)** — so every
  iPhone that can watch a gawk stream at all runs ≥ 26.4.
- **`VideoTrackGenerator` shipped in Safari 18** (Sept 2024), **worker-only
  by spec** (WebKit championed that split), alongside transferable
  `MediaStreamTrack`. Combined with the 26.4 floor: the API is
  **guaranteed present on every WebTransport-capable iPhone** — there is no
  "can watch but lacks the generator" tier.
- **Confirmed on a real device (2026-07-15)**: iOS Safari runs the gawk
  viewer with both transport *and* rendering in workers — the worker
  pipeline boots, which is exactly where the worker-only generator must
  live. The main-thread-fallback contingency is moot on iOS.
- **Unverified, load-bearing**: `new VideoFrame(offscreenCanvas,
  {timestamp})` inside a worker on iOS WebKit — the tee's core operation.
  U1's probe settles it before anything is built on top (see the
  pre-registered fallback below).

## Direction survey (settled in the 2026-07-15/16 analysis)

| Option | Sketch | Verdict |
|--------|--------|---------|
| **A — canvas tee → `VideoTrackGenerator` → hidden `<video>`** | Sinks paint the OffscreenCanvas as today; after each present, wrap the canvas in a `VideoFrame` and write it to a generator; track transferred once to main; `webkitEnterFullscreen()` on tap | **Chosen** — preserves all of R12 (interpolated frames cross too), no sink internals change, worker-only API lands where the pipeline already lives |
| B — direct `VideoTrackGenerator` render sink | Replace the WebGL/paced sinks with a generator sink; the element presents raw decoded frames | Rejected: silently drops paced playback + interpolation on the one platform that can't opt back in; only wins if iOS were on the 2D sink anyway |
| C — MSE / ManagedMediaSource | Mux AVCC → fMP4, feed `<video src>` | Rejected: an fMP4 muxer + SourceBuffer buffering fights the sub-500 ms target; a large new component for a fullscreen button |
| D — `canvas.captureStream()` | Capture the on-screen canvas element | Rejected: `HTMLCanvasElement`-only and the element is a placeholder after `transferControlToOffscreen()`; main-thread variant double-draws |
| E — CSS pseudo-fullscreen only | `position: fixed; inset: 0` on the viewer root | Kept, but as the **fallback tier**, not the goal: browser chrome stays visible, no orientation/system-video affordances |

## Decisions

1. **Gate: the absence of Element Fullscreen, checked once on the main
   thread.** `typeof document.documentElement.requestFullscreen !==
   'function'` ⇒ the device is "element-fullscreen-less" (iPhone, plus any
   hypothetical browser in the same boat — the tiering handles it either
   way). On devices where the check passes (desktop, Android, iPad), **no
   R16 code path activates**: no tee wrapper, no worker command, no video
   element, `useFullscreen` tier 1 is exactly today's call. This is the
   "nothing changes on non-iOS" guarantee, and U1 pins it with tests.

2. **Tiered fullscreen in `useFullscreen`** (per-tap, not just per-device):
   (1) `element.requestFullscreen()` where it exists — unchanged;
   (2) `video.webkitEnterFullscreen()` when the presentation video is armed
   and ready (readyState ≥ metadata), wrapped in try — an
   `InvalidStateError` falls through;
   (3) CSS pseudo-fullscreen (`position: fixed; inset: 0; height: 100dvh`
   on the viewer root, reusing the same `isFullscreen` state so the
   button/icon/menu stay correct).
   State tracking: `fullscreenchange` (tier 1),
   `webkitbeginfullscreen`/`webkitendfullscreen` on the video (tier 2),
   local state (tier 3). The hook returns which tier is active for the
   overlay.

3. **The tee is a `RenderSink` decorator around the context sink, inside
   the paced sink** — `PacedPresentationSink(TeeRenderSink(contextSink))`.
   It delegates `draw`/`kind`/`drawnFrames`, and (crucially) passes through
   the `upload`/`present` interpolation surface so
   `PacedPresentationSink.supportsInterpolation` still sees it. After every
   paint that reaches the screen — plain `draw()`, `present(1)`, *and*
   `present(0.5)` synthesized mid-frames — it captures the canvas:
   `new VideoFrame(canvas, { timestamp })` → generator writer. Only
   **presented** frames cross (coalesced/superseded frames were never
   painted), so the track carries the exact smoothed output, interpolated
   60 fps included. Timestamps: real frames reuse the source frame's
   timestamp; synthesized presents get the midpoint of the two real
   timestamps the decorator saw around them (the element renders live
   `srcObject` frames on arrival, so these only need to be sane and
   monotonic, not load-bearing).

4. **Idle until armed, armed for the rest of the session.** The tee wrapper
   is only *constructed* when the worker `init` carries a new optional
   `presentationTee: true` flag (sent only on gated devices — Decision 1),
   and even then it is pass-through-only (zero per-frame work, no capture)
   until an `arm` command activates it. Arming creates the
   `VideoTrackGenerator` **in the worker**, posts the track back
   (transferred) as a new `presentationTrack` event, and starts capturing.
   The generator and tee live at the host level (beside `renderSink`), so
   they survive pipeline attempts/reconnects — the track keeps flowing
   across a reconnect without re-arming. Once armed, it stays armed:
   pausing the tee would let the video's readyState question reopen under
   the user's next tap.

5. **Pre-armed at `watching`, not lazily on tap.** `webkitEnterFullscreen`
   must run inside a user gesture on a video that already has media — a
   lazy arm (postMessage → generator → transfer → srcObject → metadata) is
   an async chain that risks outliving the gesture. So on gated devices the
   screen arms the tee as soon as the connection reports `watching` and
   keeps the hidden video playing. The cost — one GPU `VideoFrame` copy per
   presented frame plus a hidden video element — is paid **only on
   iPhone**. If U4's on-device pass shows a battery/thermal problem,
   deferring the arm to the first control-revealing tap is the recorded
   optimization (every fullscreen tap on a touch device is normally
   preceded by one), accepting tier-3 for a hasty first tap.

6. **The hidden video element**: `<video playsinline muted autoplay>`,
   visually hidden by size/position (e.g. 1 px, off-viewport, or
   `visibility: hidden`-equivalent that keeps it renderable) — **never
   `display: none`**, which breaks `webkitEnterFullscreen`. `srcObject =
   new MediaStream([track])` when the track event arrives; `play()` called
   defensively. It renders only on gated devices while armed. While in
   native fullscreen the canvas keeps painting underneath (it *is* the tee
   source), so exiting fullscreen is seamless — the inline surface never
   stopped.

7. **Capability probe before any protocol change** (U1): on gated devices
   the worker reports, boot-handshake style, whether
   `typeof VideoTrackGenerator !== 'undefined'` *and* a trial
   `new VideoFrame(new OffscreenCanvas(2, 2), { timestamp: 0 })` succeeds.
   Probe failure ⇒ tier 3 (pseudo-fullscreen) and no arm command is ever
   sent. **Pre-registered fallback** (R12 pattern — a documented rejection
   is a valid completion): if the probe fails *on real iPhone hardware*,
   U2/U3 are dropped, pseudo-fullscreen ships as the iPhone answer, and
   this doc records the finding.

8. **Scope boundaries.** Viewer-client only: zero server/wire/broadcaster
   changes; the frozen `#/debug/*` pages are untouched; the main-thread
   pipeline fallback keeps today's behavior and gets tier 3 only
   (`VideoTrackGenerator` is worker-only, iOS is confirmed on the worker
   path, and a generator-host worker bridge for a path iOS doesn't take is
   complexity for nobody — see Rejected). Known, accepted limitation:
   native video fullscreen shows the **system** player UI — gawk's overlay,
   context menu, and stats are unreachable while fullscreen (they remain
   available inline; this is inherent to `webkitEnterFullscreen`, not a
   choice). The system pause button pauses a live `srcObject`, and resuming
   returns to the live edge by construction.

9. **Observability: a new "Feature Gates" overlay section** (user
   decision 2026-07-16). The stats overlay gains a **Feature Gates**
   section listing gate-controlled features by name with their active
   state — the generic home for "which conditional features are live on
   this client". Gate names are **UpperCamelCase** (user requirement),
   enforced as a TS string-literal union (`FeatureGateName`) so names
   can't drift per call site. `ViewerStats` gains
   `featureGates: { name: FeatureGateName; active: boolean;
   detail?: string }[]`; the shared overlay renders the section **only
   when the surface reports at least one gate** — the broadcaster overlay
   reports none today and is untouched.

   R16 contributes the first (and for now only) gate:
   **`NativeVideoFullscreen`** — `active` when the device gate is on, the
   capability probe passed, and the tee is armed (i.e. the native path
   would actually be used on the next tap); `detail` carries the resolved
   state (`element fullscreen available` on non-gated devices,
   `probe failed → pseudo`, `arming`, `armed`). Raw tee diagnostics —
   `presentation: { tier: 'element' | 'video' | 'pseudo' | null,
   armed: boolean, teedFrames: number, teeErrors: number }` — stay in
   `ViewerStats` for Copy diagnostics; the gate row is derived from them.
   (Follow-up, 2026-07-16: the row's *value* is just ✓/✗ — the full
   `✗ — detail` string overflowed the overlay grid; `detail` renders as a
   hover tooltip on the value and always travels in Copy diagnostics.)

   Scope note: the section appears on **every** viewer (a desktop viewer
   shows `NativeVideoFullscreen ✗ — element fullscreen available`, which
   is exactly the remote-diagnosis win). This is the one deliberate
   R16-visible change on non-gated devices, and it is overlay-only — the
   Decision 1 guarantee (no tee, no worker messages, no video element,
   unchanged fullscreen path) is about behavior, not the stats readout.
   Future conditional features (paced/adaptive playout, interpolation,
   worker placement, R15 audio) are natural later entries — noted, not
   scoped. (Reaching the overlay on iOS is itself awkward — no keyboard,
   no `contextmenu` on long-press — a known gap noted for a future item,
   not scoped here.)

10. **R15 synergy noted, not scoped**: when system audio lands, the same
    element could carry the audio track on iOS for element-native A/V sync.
    R15's AudioWorklet design stands; revisit there if iOS audio behaves
    poorly.

## End-to-end path (gated devices only)

```
viewer worker (existing R8/R10/R12 pipeline, unchanged upstream):
  decode → reorder release → PacedPresentationSink
    → TeeRenderSink(contextSink)               [new decorator, idle until armed]
        paints OffscreenCanvas exactly as today (WebGL2/WebGL/2D)
        └─ armed: new VideoFrame(canvas, {timestamp})
             → VideoTrackGenerator.writable    [worker-only API]
                  .track ──transfer once──▶ main thread

main thread (ViewerScreen, gated devices while watching):
  hidden <video playsinline muted autoplay> ← srcObject = MediaStream([track])
  fullscreen tap → tier 2: video.webkitEnterFullscreen()   [sync, in-gesture]
                   (not ready / throws → tier 3: CSS pseudo-fullscreen)
  state: webkitbeginfullscreen / webkitendfullscreen → isFullscreen
```

Non-gated devices: none of the above exists; tier 1 (`requestFullscreen`)
is the entire feature, as today.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| U1 | **Gate + tiered fullscreen + pseudo-fullscreen + probe + Feature Gates section** — gate util (element-fullscreen absence); `useFullscreen` rework to the three tiers with per-tier state tracking; pseudo-fullscreen CSS on the viewer root; worker capability probe (`VideoTrackGenerator` + trial `VideoFrame`-from-canvas) surfaced to the main thread; the overlay's new **Feature Gates** section (`FeatureGateName` union, `ViewerStats.featureGates` + `presentation`) with its first entry `NativeVideoFullscreen` | Non-gated devices: `requestFullscreen` called exactly as today, **no video element in the DOM, no new worker messages** (explicit tests; the Feature Gates row reading `✗ — element fullscreen available` is the sole visible delta, per Decision 9); gated devices: the button always visibly does something (pseudo-fullscreen works with no worker support at all); jsdom tests cover tier selection + state tracking incl. `webkitbeginfullscreen`/`webkitendfullscreen`; Feature Gates section renders only when ≥1 gate is reported (broadcaster overlay unchanged — test), gate active/detail correct for each tier/probe outcome, names type-checked UpperCamelCase; Copy diagnostics includes `featureGates` + `presentation` | ✅ implemented 2026-07-16 (note: the raw tee diagnostics landed as `ViewerStats.presentationSurface` — `presentation` was already taken by the R12 pacing-placement field) |
| U2 | **Worker tee** — `TeeRenderSink` decorator (idle/armed, interpolation `upload`/`present` passthrough, synthesized-frame midpoint timestamps, `teedFrames`/`teeErrors` counters); generator creation + track transfer on `arm`; `init.presentationTee` flag; host-level lifecycle across reconnects | Unit tests with a fake generator/writer: idle tee delegates byte-identically (paced-sink + interpolation tests re-run wrapped, unchanged results); armed tee writes **only presented** frames (coalesced/superseded never cross); `present(0.5)` frames carry midpoint timestamps; write failure counts `teeErrors`, never throws into the paint path; sink construction without the flag is exactly today's (assertion on the non-gated path); arm across a reconnect keeps one generator/track | ✅ implemented 2026-07-16 (`transport/tee-render-sink.ts`; the tee + generator live at the worker-shell level beside the sink, arm is idempotent there and in the controller) |
| U3 | **Main-thread video surface + native fullscreen** — hidden pre-armed `<video>` on gated devices at `watching` (Decision 6 hiding rules); `srcObject`/`play()` wiring on the `presentationTrack` event; arm command dispatch; tier-2 `webkitEnterFullscreen` with try/fall-through; StrictMode-safe teardown (track/element released with the controller) | Gated + capable: tap → `webkitEnterFullscreen` called synchronously in the gesture handler (test via stubbed video); not-ready video falls through to pseudo without a dead tap; non-gated DOM snapshot has no video element; teardown closes the writer and detaches `srcObject` (no leaked track); menu item + button + `f` hotkey all route through the same tiered toggle | ✅ implemented 2026-07-16 (track teardown rides the controller's *deferred* dispose — a plain effect-cleanup `track.stop()` would kill it for good across a StrictMode remount) |
| U4 | **On-device verification + findings + docs sync** — real-iPhone pass (below); record the `VideoFrame`-from-canvas verdict (Decision 7's pre-registered fallback fires here if needed); latency/battery sanity; README gotcha list + ROADMAP/CLAUDE status sync | The manual verification plan executed and findings recorded in this doc; explicit verdicts on: tee works on iOS (or documented rejection → pseudo ships), smoothing visibly preserved in fullscreen, added latency acceptable (compare live-edge drift inline vs fullscreen), no thermal/battery red flags in a ~20 min session; gotchas (iPhone fullscreen facts, `display:none` trap) added to README | 🚧 in progress — pass 1 2026-07-16: enters native fullscreen but **black**; defenses shipped (tee-local PTS, preserveDrawingBuffer, gesture play(), element-side overlay diagnostics). Pass 2 same day: still black with frames flowing end-to-end → canvas readback ruled the operative cause; **decoded-frame clone tee** shipped (pre-registered step; preserveDrawingBuffer removed with the readback) + Content sample row — see “U4 findings”. **Pass 3 (2026-07-19): still black with the clone tee → native tier rejected per the pre-registered criteria (the `webkitEnterFullscreen` player can't present a locally generated MediaStream on iOS WebKit); pseudo-fullscreen (CSS) is the shipping path.** ⚠️ verdict recorded; code cleanup (delete tier-2 tee/generator/video path) is the remaining follow-up, tracked in BUGS.md |

Ordering: U1 alone already kills the silent no-op (pseudo-fullscreen tier)
and is independently shippable; U2 → U3 build the native path; U4 gates
calling R16 done. U1–U3 landed together 2026-07-16 (lint + typecheck +
491 tests green); since the capability probe runs at runtime before any
arm, a probe failure on real hardware degrades to pseudo-fullscreen on its
own — U4 records the verdict either way.

## Verification plan (manual, U4)

On a real iPhone (Safari, plus Chrome-on-iOS for the same-WebKit check),
against the homelab deployment:

1. **Native path**: join a live broadcast → tap fullscreen → native video
   fullscreen with system UI; rotate to landscape; stream is the smoothed
   output (with a 30 fps rung + interpolation on, fullscreen should look
   60 fps-smooth, not 30); exit via system UI → inline viewer resumes
   seamlessly; `isFullscreen` state correct throughout (button icon, menu
   label).
2. **Latency**: Copy-diagnostics (or remote inspector) live-edge drift and
   capture→render latency inline vs. in fullscreen — the tee + element
   should add at most a few frames; record numbers.
3. **Reconnect while armed**: kill the relay pod mid-view → auto-reconnect
   → fullscreen still works without leaving/rejoining (one generator/track
   across attempts, Decision 4).
4. **Probe-failure tier**: with the probe artificially failed (dev build),
   the tap produces pseudo-fullscreen, no arm command, no video element;
   Feature Gates reads `NativeVideoFullscreen ✗` (tooltip / Copy
   diagnostics: `probe failed → pseudo`).
5. **Regression, non-gated**: desktop Chrome + Firefox — fullscreen
   behavior unchanged, DOM has no video element, worker message traffic
   unchanged (assert via logging); the overlay's Feature Gates section
   shows `NativeVideoFullscreen ✗` with `element fullscreen available` as
   the value's tooltip (the sole visible delta) and the broadcaster
   overlay has no such section; iPad — tier 1 element fullscreen, R16
   inert. On the iPhone happy path, Feature Gates reads
   `NativeVideoFullscreen ✓` (tooltip: `armed`).
6. **Cost sanity**: ~20 min fullscreen session on the iPhone — battery/
   thermal observation; if concerning, record and consider the Decision 5
   deferred-arm optimization.
7. Record findings + verdicts in this doc; sync README/ROADMAP/CLAUDE and
   remove the BUGS.md entry.

## U4 findings

### First on-device pass (2026-07-16): native fullscreen enters, but black

**Symptom**: the fullscreen tap enters real native fullscreen (system player
UI), but the video shows nothing — black. What the symptom already proves:
tier 2 fired (a tier-3 fall-through would show the working canvas
pseudo-fullscreen, not black), so `webkitEnterFullscreen` ran against a
video at `readyState ≥ HAVE_METADATA` — the probe passed, the arm
succeeded, the track reached the element, and *something* (at least
metadata) flowed. The blackness enters after that point.

Decision 7 pre-registered `VideoFrame`-from-canvas as the one unverified
load-bearing fact, with probe-failure → pseudo as the fallback. The actual
failure mode is subtler than the pre-registered one: the probe *passes*
(construction works) but the presented result is black. Three candidate
causes survive analysis, each with real WebKit history, and they are not
distinguishable from the symptom alone:

1. **Black readback** — `new VideoFrame(WebGL-backed OffscreenCanvas)`
   reading a discarded or GPU-process-mediated drawing buffer returns
   black. WebKit class-mates: [237424](https://bugs.webkit.org/show_bug.cgi?id=237424)
   (video→canvas black under GPU-process canvas rendering),
   [181663](https://bugs.webkit.org/show_bug.cgi?id=181663) (canvas
   captureStream → `<video>` blank on iOS for four years, fixed in 15.4).
   Note the U1 probe canvas is **2D**; the production canvas is **WebGL** —
   exactly the gap a construction probe can't see.
2. **PTS scheduling** — the tee stamped generator frames with the
   *source frames'* timestamps, i.e. the **broadcaster's**
   `performance.now()` µs: huge foreign values, backwards jumps on
   broadcaster restarts. If WebKit schedules a locally generated track's
   samples by PTS against the element's media clock (rather than
   display-immediately), such frames never present — black, while
   dimensions/metadata still arrive.
3. **Paused element** — a paused MediaStream `<video>` is black in the
   native player by definition, and `readyState` still reaches
   HAVE_METADATA. The arm-time `play()` is an out-of-gesture muted
   autoplay; iOS **Low Power Mode** (among others) rejects even those, and
   the rejection was silently swallowed.

**Defenses shipped 2026-07-16** (all three, plus instrumentation — they are
small, orthogonal, and confined to the gated path; non-gated devices stay
byte-identical, re-verified by test + headless-Chrome pass):

- **Tee-local PTS** (`tee-render-sink.ts`): generator frames are stamped
  from the tee's own clock — zero-based at first capture, strictly
  monotonic (+1 µs tie-break on clock stalls), restart-proof. Capture time
  *is* the paced presentation time, so it is also the truthful PTS for this
  stream. Source timestamps never reach the generator anymore; the old
  midpoint bookkeeping went with them.
- **`preserveDrawingBuffer: true`** for the tee'd GL context only
  (`createContextSink` now takes GL-option overrides; every other caller
  passes none and gets byte-identical options — pinned by test).
- **Gesture-context `play()`** (`useFullscreen.ts` tier 2): a paused video
  is played *inside the user gesture* right before `webkitEnterFullscreen`
  — gesture-context play succeeds where muted autoplay is blocked. The
  arm-time effect also force-sets `video.muted = true` imperatively
  (React's `muted` prop famously may not reach the DOM property) and now
  logs a `play()` rejection instead of swallowing it.
- **Element-side diagnostics**: `PresentationSurfaceStats` gained
  `elementReadyState`, `elementPaused`, `elementWidth/Height`, and
  `elementFrames` (a `requestVideoFrameCallback` counter — frames the
  element actually *presented*); a new overlay **Native Fullscreen**
  section renders these as visible rows on gated devices only (the Feature
  Gates detail is a hover tooltip — useless on a phone). All of it rides
  Copy diagnostics.

### Second on-device pass (2026-07-16): still black; frames flow end-to-end

Overlay verdict: `Tee armed · N frames` climbing, element `playing`,
`Element frames` climbing, element size non-zero — and fullscreen still
black. That eliminated causes 2 and 3 (frames present with sane local PTS,
element not paused) and pointed at the *content* of the teed frames:
**`VideoFrame`-from-WebGL-canvas readback is black on this WebKit even with
`preserveDrawingBuffer: true`**. (Caveat kept honest below: with the
element invisible inline, rVFC climbing could not yet fully separate
"presents black frames" from "native player renders black" — the third-pass
Content sample row closes that gap.)

**The pre-registered next step was executed the same day** — the tee no
longer touches the canvas at all:

- **Decoded-frame clone tee** (`tee-render-sink.ts` rework): the tee writes
  `new VideoFrame(decodedFrame, { timestamp: localTs })` clones of the
  frames it presents — `draw()` clones before the inner sink consumes the
  frame; on the interpolation surface, `upload()` takes the clone and
  `present(1)` (the frame's own slot) writes it, a superseding upload
  closing the held clone unseen. This is the canonical
  WebCodecs→MediaStream path (no canvas readback anywhere, cheaper per
  frame — relevant to the item-6 thermal check). Cost, accepted: synthesized
  `present(0.5)` blends exist only on the canvas and don't cross —
  fullscreen shows real frames at the paced cadence (constraint 2 traded
  slightly for a path WebKit demonstrably exercises).
- The **probe** now trials clone-with-timestamp from a VideoFrame (the new
  load-bearing operation); the `preserveDrawingBuffer` override was removed
  along with the readback it defended.
- **Content sample** overlay row: the main thread periodically draws the
  presentation `<video>` into a 4×4 canvas and reports the max RGB channel
  (`elementContentPeak`) — the missing discriminator between black frame
  content and a black native player.

### How to read the third on-device pass

Inline, before going fullscreen (stats button; right-click doesn't exist on
iPhone):

| Observation | Verdict |
|---|---|
| Fullscreen works, video visible | Done — close U4 (record latency/thermal per items 2/6) |
| `Content sample` peak high (with a bright inline stream) but fullscreen still black | Frame content is fine — the native fullscreen player itself can't present locally generated MediaStreams on this WebKit. Decision 7's fallback fires for good: remove tier 2, ship pseudo |
| `Content sample` peak ~0 while the inline canvas is bright | Clone content is *also* black — decoded-frame clones don't survive the generator either; same verdict: pseudo ships (and record it as a WebKit bug worth filing upstream) |
| `Tee … err` climbing or probe now fails | Clone-with-timestamp unsupported on this WebKit — probe failure already degrades to pseudo on its own |

### Third on-device pass (2026-07-19): still black — native tier rejected

The third pass ran the decoded-frame clone tee on a real iPhone: native
fullscreen **still shows a black video**. With the clone rework the tee no
longer touches the WebGL canvas at all (the pass-2 suspect), so the remaining
explanation is the one the pre-registered table's high-sample-still-black row
names: **the native `webkitEnterFullscreen` player cannot present a locally
generated `MediaStreamTrack` (`VideoTrackGenerator` output) on this iOS
WebKit** — decoded-frame clones fed through the generator do not render in the
system player, independent of where the frames come from.

**Verdict (pre-registered, Decision 7 fallback fires for good): the native
tier is not viable on iPhone — remove tier 2, ship pseudo-fullscreen (CSS).**
The gate/probe already fall back to pseudo when the tee is unavailable; the
follow-up is a code cleanup that deletes the tee/generator/hidden-`<video>`
path so the black native player is never reached (tracked in BUGS.md until it
lands). Non-gated devices remain byte-identical throughout. Worth an upstream
WebKit bug report (MediaStream `<video>` → `webkitEnterFullscreen` renders
black).

## Non-goals

- **Custom controls/overlay inside native fullscreen** — impossible under
  `webkitEnterFullscreen`; the system UI is the UI. Inline overlay/menu are
  unchanged.
- **An iOS-reachable stats overlay opener** (no keyboard, no `contextmenu`
  on long-press) — a real diagnosability gap on exactly this platform, but
  a separate small item; noted, not scoped.
- **`screen.orientation.lock()`** — not available on iOS Safari; rotation
  is the user's.
- **Audio through the element** — R15's business (Decision 10).
- **Main-thread-pipeline tee** — tier 3 covers that path (Decision 8).
- **Offering the video surface on capable-but-non-gated devices** — the
  canvas + element fullscreen path is strictly better where it exists.

## Rejected

- **Direct `VideoTrackGenerator` render sink** (survey option B) — loses
  R12 pacing + interpolation on iOS for no compensating win once the tee
  exists; the tee's only extra cost is the per-presented-frame copy.
- **MSE/ManagedMediaSource and `canvas.captureStream()`** — survey options
  C/D (muxer + buffering vs. the latency budget; placeholder canvases
  can't be captured after `transferControlToOffscreen()`).
- **Lazy arm on the fullscreen tap** — the async arm chain risks leaving
  the user gesture, producing exactly the dead-tap this item exists to
  kill; pre-arm at `watching` instead (Decision 5, with the
  first-interaction arm recorded as a fallback optimization).
- **A generator-host worker bridge for the main-thread pipeline** — builds
  a frame-transfer hop for a fallback path that iOS (the only gated
  platform) is confirmed not to take.
- **WebRTC** for the presentation track — set aside project-wide; and
  wholly unnecessary: `VideoTrackGenerator` exists on every gated device
  that can connect at all (Safari 26.4 ⊃ 18).
