# R22 — iOS Native Fullscreen via MSE / ManagedMediaSource

Design doc for [ROADMAP R22](../ROADMAP.md#r22--ios-native-fullscreen-via-mse)
(designed 2026-07-23; **MF1–MF4 implemented 2026-07-25 + MF5's
observability/docs half — automated gates green incl. the new Chrome
`MediaSource` playback check; MF5's on-device verification pass is
pending** — see "Implementation status & deviations"). Supersedes the
native-fullscreen tier
of [R16](21-ios-video-fullscreen.md), whose `VideoTrackGenerator` MediaStream
tee was rejected after three on-device passes showed a **black** native player
(docs/21 "U4 findings"). This item makes the iPhone fullscreen button actually
show video — by feeding a hidden `<video>` from a
**`ManagedMediaSource`** (an fMP4 muxer over the already-encoded stream)
instead of a MediaStream, and calling the same `webkitEnterFullscreen()` R16
already wires up.

R16 evaluated MSE as **survey option C and rejected it up front** ("an fMP4
muxer + SourceBuffer buffering fights the sub-500 ms target; a large new
component for a fullscreen button") because the MediaStream tee looked cheaper.
The tee turned out to be **the** thing that doesn't work on iOS WebKit — the
native player cannot present a locally generated `MediaStreamTrack` (docs/21
third-pass verdict). A throwaway spike on a real iPhone (2026-07-23) then
proved the reverse for MSE:

> A `ManagedMediaSource`-backed `<video>` (fMP4 appended via `SourceBuffer`)
> **presents real frames** under `webkitEnterFullscreen()` on iPhone. Both the
> MMS button and a plain-`<video src>` control showed video; neither was black.

So the calculus that rejected option C is inverted by evidence: MSE is now the
*only known-working* native-fullscreen path on iPhone, and its one real
drawback — buffered latency vs. the sub-500 ms target — is confined to the
native-fullscreen view by scoping MSE **parallel** to the untouched WebCodecs
inline path (Decision 1), not as a replacement for it.

Two constraints carry over verbatim from R16 (they were always the right
constraints; only the mechanism was wrong):

1. **On every device with the Element Fullscreen API, nothing changes.**
   Byte-identical: no `<video>` in the DOM, no `ManagedMediaSource`, no muxer,
   no new worker messages. The whole feature stays gated on the *absence* of
   `Element.requestFullscreen` — the iPhone signature. (Sole overlay-only
   exception, unchanged from R16 Decision 9: the Feature Gates section reports
   `NativeVideoFullscreen` on every viewer.)
2. **The WebCodecs/canvas inline path survives untouched.** Paced presentation,
   adaptive offset, and interpolation keep painting the canvas exactly as
   today on iPhone as everywhere. MSE is a *second*, fullscreen-only surface;
   it never replaces the render pipeline (that was the rejected R16 survey
   option B, and MSE-primary-on-iOS is rejected again here for the same
   reason — see Rejected).

## Goal

An iPhone viewer taps fullscreen and gets real, chrome-free native fullscreen
showing the live stream, rotating to landscape like any video app; exiting
returns to the inline viewer seamlessly. Where the codec/capability probes
fail, the button still does something visible (CSS pseudo-fullscreen, which
also keeps interpolation) — the silent no-op stays dead on every device.
Desktop Chrome/Firefox and iPad behavior is untouched. Zero server, wire, and
broadcaster changes.

## Background: why MSE, why now

R16 established every platform fact this item rests on; the short version:

- **iPhone has no Element Fullscreen API.** `HTMLVideoElement.webkitEnterFullscreen()`
  is the only native fullscreen; it needs a **user gesture** and a `<video>`
  at `readyState ≥ HAVE_METADATA`, does not fire `fullscreenchange`
  (`webkitbeginfullscreen`/`webkitendfullscreen` instead), and a
  `display: none` video breaks it. `playsinline` keeps playback inline. All of
  this is already implemented in `useFullscreen.ts` (R16 Decision 2).
- **The viewer paints to an OffscreenCanvas in a worker** (R8 S6), so there is
  no `<video>` for that API to act on — R16 added a hidden one; R22 keeps it
  and changes only what feeds it.

What is new, and what makes option C viable now:

- **`ManagedMediaSource` (MMS) is the MSE variant on iPhone.** MSE only reached
  iPhone in **iOS 17.1** (late 2023), via `ManagedMediaSource` — a
  power/network-aware variant the system paces with `startstreaming` /
  `endstreaming` events and manages buffer eviction for. Plain
  `window.MediaSource` may be `undefined` on iPhone; `window.ManagedMediaSource`
  is the one to feature-detect (fall back to `MediaSource` where only it
  exists, e.g. desktop test harnesses). The API surface —
  `addSourceBuffer` / `appendBuffer` / `SourceBuffer` — is otherwise the same.
- **The WebTransport floor guarantees it.** WebTransport arrived on iOS in
  **Safari 26.4** (docs/21), far above the iOS 17.1 MMS floor — so every iPhone
  that can watch a gawk stream at all has MMS. There is no "can watch but lacks
  MMS" tier, exactly as there was no "lacks `VideoTrackGenerator`" tier.
- **`webkitEnterFullscreen` + MSE is the sanctioned iPhone path.** MMS exists
  on iPhone specifically so adaptive players (YouTube-style) can drive the
  native player, fullscreen included. Unlike a MediaStream source (which the
  native player refused — black), an MSE-backed `<video>` flows through
  WebKit's standard buffered-media decode/present pipeline — the pipeline the
  fullscreen player is built for. **Spike-confirmed on real hardware
  2026-07-23** (see header).
- **We already hold everything to mux.** The viewer worker has the encoded
  H.264 chunks *before* decode (`EncodedVideoChunk` inputs), the keyframe flag,
  and the `DecoderConfig`; `viewer.ts` already sniffs Annex-B vs AVCC and
  parses the avcC/SPS-PPS (`normalizeAvccExtradata`). An fMP4 muxer is a pure
  worker-side transform over data that is already in hand — **zero wire /
  server / broadcaster change** (the R16 scope guarantee, kept).

## The core insight: fork *upstream* of the decoder

R16's tee wrapped the **presented canvas frames** — *downstream* of the
decoder — and wrote them to a `VideoTrackGenerator`. That is post-decode,
locally generated media, and the iOS native player renders it black.

MSE takes **encoded bitstream**. So R22 forks the stream *upstream* of the
decoder — at the reorder buffer's release point, the same in-order,
deduplicated, freeze-on-gap-applied `ReleasedFrame` stream (`reorder-buffer.ts`)
that becomes `EncodedVideoChunk`s for the `VideoDecoder` in `viewer.ts`. The
muxer turns that stream into fMP4 and hands it to the native player, which does
its own decode. **This upstream move is the whole reason MSE renders and the
tee did not** — it is not a tuning difference, it is a different class of
media source.

The cost of taking the encoded stream instead of the presented one: the native
player shows the **real encoded cadence**, not R12's post-decode interpolated
60 fps (interpolated frames never exist as encoded bytes). That is retained for
all normal viewing and given up only inside the native player — see Decision 9.

## Decisions

1. **Parallel tee, not MSE-primary (the architecture; user-confirmed
   2026-07-23).** The WebCodecs/canvas inline path stays byte-identical on all
   platforms. On gated devices only, an MSE-fed hidden `<video>` is armed for
   the native-fullscreen moment. This preserves the sub-500 ms live-edge, R12
   paced/interpolated playback, and the R5 latency metrics for all inline (and
   CSS-pseudo-fullscreen) viewing on iPhone; MSE's buffered latency is confined
   to the native player, a deliberate native mode the user opted into by
   tapping fullscreen. **MSE-primary-on-iOS is rejected**: it would surrender
   all of the above everywhere on iPhone for a one-decode-path simplification
   (see Rejected).

2. **Gate + tiers unchanged; only tier-2's *source* changes.** Reuse R16's
   device gate (`elementFullscreenAvailable()` — absence of
   `Element.requestFullscreen`) and the three-tier `useFullscreen` with its
   `presentationVideo` seam and per-tier state tracking. Tier 2 stays
   `video.webkitEnterFullscreen()` on a ready `<video>`; what changes is that
   the video's media comes from `ManagedMediaSource.appendBuffer` rather than a
   `VideoTrackGenerator` MediaStream `srcObject`. `useFullscreen.ts` needs
   almost no change (the in-gesture `play()` + readyState gate already fit MSE;
   the "paused MediaStream is black" comment updates). Non-gated devices:
   no code path activates.

3. **Fork encoded frames at the reorder-buffer release; mux in the worker.**
   A new pure muxer consumes the `ReleasedFrame` stream and the `DecoderConfig`
   and emits fMP4 segments; it runs in the viewer worker (where the encoded
   frames already live) and posts segments to the main thread as **transferable
   `ArrayBuffer`s**, which the main thread appends to the MMS `SourceBuffer`.
   This mirrors R16's worker-produces / main-thread-consumes split (there:
   generator in worker, track transferred to main) — only the fork point moves
   upstream and the payload is bytes, not a track. The muxer lives at the
   worker-shell level beside the render sink, so it survives pipeline
   attempts/reconnects (R16 Decision 4's lifecycle, reused).

4. **The fMP4 muxer handles both wire formats and assumes no B-frames.**
   - *Init segment* (`ftyp` + `moov` with one AVC `trak`): the `avcC` comes
     from the `DecoderConfig` extradata when present (AVCC path — browser
     broadcaster); when extradata is empty (Annex-B path — the native Linux
     broadcaster, docs/19, in-band SPS/PPS at every IDR) the muxer extracts
     SPS/PPS from the keyframe's NALs and synthesizes the `avcC` itself. Visual
     dimensions come from the **SPS**, honoring docs/01 ("trust the bitstream,
     not metadata").
   - *Media segments* (`moof` + `mdat`, one per frame or per GOP): Annex-B
     start codes are rewritten to length-prefixed AVCC NAL units for the
     `mdat`; AVCC input is already length-prefixed. Because the protocol
     guarantees **no B-frames** (decode order == presentation order — a
     long-standing gawk invariant, docs/19), per-sample composition offsets are
     zero (`CTS == DTS`) and `baseMediaDecodeTime` advances monotonically off
     the frame timestamps. The µs timestamps map to a fixed movie timescale.
   - The muxer is DOM-free and golden-vector tested (the wire-test culture): a
     committed encoded slice produces byte-stable fMP4 whose box structure is
     asserted, and — the load-bearing automated proof — **plays in a Chrome
     `MediaSource` `<video>`** (Decision 10).

5. **Arm at `watching`; keep the video loaded-and-paused near live until the
   in-gesture play.** `webkitEnterFullscreen` must run inside the user gesture
   on a video that already has media, and the arm chain (create MMS → add
   SourceBuffer → append init + first GOP → metadata) is async — so pre-arm, as
   R16 Decision 5 concluded. On gated + capable devices, at `watching` the
   worker starts muxing and the main thread creates the MMS and keeps the
   hidden `<video>` **loaded but paused at the live edge** (ready, not
   presenting at full rate), so continuous dual-decode is avoided while the
   fullscreen tap always finds ready media. The in-gesture tier-2 handler seeks
   to the live edge, `play()`s, and `webkitEnterFullscreen()`s. **Pre-registered
   thermal escape hatches** (decided by MF5's on-device measurement, not up
   front): (a) defer the arm to the first control-revealing tap (every
   fullscreen tap on touch is normally preceded by one); (b) suspend the inline
   WebCodecs decode while native fullscreen is active (the canvas is invisible
   under the system player; exit resyncs to the next keyframe, ≤ one 500 ms
   GOP). Both are optimizations, not v1 scope.

6. **Live-edge management leans on MMS's managed buffering.** Appends are paced
   by MMS `startstreaming` / `endstreaming` (the system also evicts old
   buffered ranges, which keeps a long session bounded). The video is kept near
   live by seeking to `buffered.end` on play and a small catch-up if it drifts.
   Mid-stream config changes (R4/R13 resolution steps, codec pin) emit a fresh
   init segment (or `SourceBuffer.changeType` on a codec change); a broadcaster
   restart / frameID wrap resets the muxer and re-inits, driven by the reorder
   buffer's existing restart/backwards-keyframe signals. Native fullscreen
   therefore sits at MSE-buffer depth behind live — accepted, and measured
   inline-vs-fullscreen in MF5.

7. **Delete the R16 tee / generator / MediaStream path.** R22 removes
   `TeeRenderSink`, the `VideoTrackGenerator` arming, the `presentationTee`
   init flag, the `presentationTrack` transfer, and the MediaStream `srcObject`
   wiring — the exact cleanup R16's U4 verdict left pending in BUGS.md. What
   survives and is reused: the hidden `<video>` element and its hiding rules
   (R16 Decision 6), the gate, the `useFullscreen` tiers, and the
   `NativeVideoFullscreen` feature gate. The plumbing behind the video element
   is swapped from "presented-frame tee" to "encoded-frame muxer + MMS".

8. **Feature Gates / overlay reuse.** The `NativeVideoFullscreen` gate stays;
   `active` now means "MMS armed + capability probe passed." The R16 tee
   diagnostics (`presentationSurface.teedFrames`/`teeErrors`,
   `elementContentPeak`, the "Content sample" row) are replaced by MSE ones:
   `SourceBuffer` buffered depth, appended-segment count, append errors, video
   `readyState`/`paused`, and the inline-vs-fullscreen live-edge delta. The
   iPhone-reachable **Native Fullscreen** overlay section (visible rows on gated
   devices, since the Feature Gates tooltip is useless on a phone) carries over.

9. **Interpolation is retained inline, given up only in the native player
   (answer to the 2026-07-23 question; "good to have, not must have").**
   R12 interpolation is a post-decode, canvas-render-time synthesis; those
   frames are never encoded, so the native MSE player shows the true encoded
   cadence. Inline viewing and CSS pseudo-fullscreen keep interpolation on
   iPhone as everywhere — so the only view without synthesized mid-frames is the
   native system player, where seeing the real stream is fine. **Optional, not
   scoped**: a user preference to prefer CSS pseudo-fullscreen (interpolated,
   full-viewport) over the native player; default is native (it is the
   requested feature). If the broadcaster sends true 60 fps, the native player
   shows real 60 fps — interpolation only ever mattered when encoded cadence <
   display Hz.

10. **CI coverage in Chrome `MediaSource`; native presentation manual.** The
    muxer's correctness — valid fMP4 that decodes and presents — is verifiable
    in **headless Chrome**, which has `MediaSource` and plays fMP4/H.264, even
    though `webkitEnterFullscreen`'s native *presentation* is iPhone-only. The
    R20 tier-1 harness gains a **standalone** step that drives the production
    muxer and presenter *modules* — committed fixture in, a Chrome `<video>`
    out — and asserts frames present (`requestVideoFrameCallback` fires,
    `currentTime` advances). It is a component proof that happens to run in a
    real browser, **not a run of the viewer surface**: the viewer under Chrome is
    ungated, so it never builds a muxer at all. The iPhone-specific native-player
    rendering stays manual (MF5) — the spike already settled that — and so does
    everything the boundary below names. This is R20's "browser E2E where
    possible, platform-specific manual" split.

    **Where that coverage stops** (recorded after MF5 pass 1, because a green
    Chrome check reads as more than it is): the check imports the muxer and
    presenter directly and **never goes through the device gate** — the gate is
    the *absence* of `Element.requestFullscreen`, which Chrome has, so in the
    production viewer under Chrome `gated` is false and no R22 code runs at all
    (Decision 1's byte-identity guarantee; the tier-1 viewer scenario therefore
    proves nothing about this path, by design). The gate is not stubbed here, it
    is simply off the path — and elided with it are arm-at-`watching`, the
    worker/postMessage crossing, the audio probe firing off the stats tick,
    `useFullscreen`'s tier selection and the audio handoff, all of which are
    covered by jsdom tests against the connection seam instead. Two further gaps
    are structural rather than deferrable: **Chrome has no
    `ManagedMediaSource`** (verified), so MMS `streaming` parking, MMS's own
    eviction, and the `srcObject`-with-MediaSource wiring are unexercised (Chrome
    takes the object-URL fallback) — which is why open finding 5 below cannot be
    caught in CI; and **WebKit's pause-and-fire-`ended` on an MSE underrun is
    definitionally what Chrome does not do**, which is exactly why finding 1
    shipped with every check green.

11. **Scope boundaries.** Viewer-client only: zero server/wire/broadcaster
    changes; `#/debug/*` untouched; the main-thread pipeline fallback gets tier
    3 only (the muxer lives in the worker like R16's generator, and iOS is
    confirmed on the worker path — a muxer bridge for a path iOS never takes is
    complexity for nobody). **H.264 only in v1**: `isTypeSupported` for VP8/VP9
    fMP4 on iOS is unreliable and the muxing differs, so a VP-codec broadcaster
    (rare — a Firefox broadcaster) viewed on iPhone probes false and falls back
    to CSS pseudo-fullscreen. The Chromium and native-Linux broadcasters both
    send H.264 — the covered path. Known, inherited limitation: native
    fullscreen shows the **system** player UI, so gawk's overlay/menu/stats are
    unreachable there (available inline); the system pause button pauses a live
    stream and resuming seeks back to live.

## End-to-end path (gated devices only)

```
viewer worker (existing R8/R10/R12 pipeline, unchanged upstream):
  transport → reassemble → ReorderBuffer.release (in-order, freeze-on-gap)
    ├─ EncodedVideoChunk → VideoDecoder → PacedPresentationSink → OffscreenCanvas
    │     (inline surface — byte-identical, keeps pacing + interpolation)
    └─ armed: ReleasedFrame + DecoderConfig → fMP4 muxer   [new, worker-side]
          → fMP4 init/media segments ──transfer──▶ main thread

main thread (ViewerScreen, gated devices while watching):
  ManagedMediaSource ← appendBuffer(segment)   (paced by startstreaming/endstreaming)
    └─ hidden <video playsinline muted> ← srcObject = mms  (loaded, paused near live)
         fullscreen tap → tier 2: seek-to-live → play() → video.webkitEnterFullscreen()
                          (not ready / throws → tier 3: CSS pseudo-fullscreen)
         state: webkitbeginfullscreen / webkitendfullscreen → isFullscreen
```

Non-gated devices: none of the above exists; tier 1 (`requestFullscreen`) is
the entire feature, exactly as today.

## Direction survey

| Option | Sketch | Verdict |
|--------|--------|---------|
| **Parallel MSE tee (fork encoded → MMS → hidden `<video>`)** | Inline WebCodecs/canvas untouched; a worker-side fMP4 muxer feeds an MMS `<video>` armed only for native fullscreen | **Chosen** — the only source type that renders under `webkitEnterFullscreen` on iOS (spike), inline latency/smoothing preserved, non-iPhone byte-identical, deletes the dead R16 tee |
| MSE-primary on iOS | The MMS `<video>` is the whole viewer surface on iPhone; drop WebCodecs/canvas there | Rejected — surrenders paced playout, interpolation, and live-edge/TimeSync control *everywhere* on iPhone; buffered latency fights sub-500 ms; a large iOS-vs-rest divergence for a one-decode-path win |
| R16 `VideoTrackGenerator` MediaStream tee | Tee presented canvas frames into a generator track → `<video>` | Rejected by evidence — black across three on-device passes (docs/21); the native player can't present a locally generated MediaStream |
| CSS pseudo-fullscreen only | The current shipping fallback | Kept as the fallback tier (and the interpolation-preserving option), not the goal — browser chrome stays, no system-video affordances |

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| **MF1** | **fMP4 muxer (pure, worker-side, test-first)** — init segment (`ftyp`/`moov`/`avcC`) from `DecoderConfig`; SPS/PPS + dimensions from the bitstream on the empty-extradata Annex-B path; media segments (`moof`/`mdat`) with Annex-B→AVCC length-prefixing; `CTS == DTS` (no B-frames); monotonic `baseMediaDecodeTime`; fresh init segment on config change | DOM-free module; golden-vector byte-stable fMP4 from the committed fixture (box structure asserted); **AVCC and Annex-B inputs both produce valid init + media segments**; empty-extradata Annex-B synthesizes the `avcC` from in-band SPS/PPS; a codec/resolution change emits a new init segment; **the muxer output plays in a Chrome `MediaSource` `<video>` — frames present, `currentTime` advances** (the CI proof the bytes are real, iPhone-independent) | ✅ 2026-07-25 (`transport/fmp4-muxer.ts` + committed fixture `transport/h264-fixture.ts`; Chrome playback via `e2e/run.mjs --muxer-check`) |
| **MF2** | **MMS capability probe + gate wiring** — feature-detect `ManagedMediaSource`/`MediaSource` + `isTypeSupported(negotiated H.264 mime)` + a trial `addSourceBuffer`/tiny-append; surface to the main thread; map to the R16 `NativeVideoFullscreen` gate; H.264-only boundary | Probe correct on gated/non-gated and H.264-vs-VP; **non-gated byte-identical** (no MMS, no `<video>`, no new worker messages — explicit test); probe-fail (incl. VP codec) ⇒ tier-3 pseudo, no arm command sent; gate `active`/`detail` reflect the MSE state | ✅ 2026-07-25 (`features/viewer/msePresentation.ts` probe; deviation 2 below on the trial-append) |
| **MF3** | **Encoded-fork + MMS surface + native tier + R16 cleanup** — fork `ReorderBuffer` release to the worker muxer; post segments (transferable) to main; MMS `<video>` fed via `appendBuffer` on `startstreaming`/`endstreaming`; `useFullscreen` tier-2 seek-to-live + `play()` + `webkitEnterFullscreen`; arm-at-`watching` (paused-near-live); **delete `TeeRenderSink`/generator/MediaStream path** | Gated + capable: tap → `webkitEnterFullscreen` synchronously in-gesture on a ready MMS video (stubbed-video test); not-ready → pseudo without a dead tap; non-gated DOM has no video/MMS; teardown releases MMS/SourceBuffer/video (StrictMode-safe per R16's deferred-dispose finding); the R16 tee code is gone and the BUGS.md black-video entry is closed | ✅ 2026-07-25 |
| **MF4** | **Live-edge + config-change hardening** — MMS managed buffering (streaming events, bounded buffer), seek-to-live on play; mid-stream resolution/codec re-init; broadcaster-restart / frameID-wrap recovery via the reorder restart signals; (optional here) suspend inline decode during native fullscreen | Fullscreen live-edge delta within a small measured bound of inline; MMS buffer bounded over a long session; a mid-stream resolution/codec change re-inits without wedging the fullscreen video; a broadcaster restart recovers; seek-to-live keeps native fullscreen near live | ✅ 2026-07-25 (except the live-edge-delta *measurement*, which is on-device — MF5; suspend-inline-decode not taken, as scoped) |
| **MF5** | **Observability + on-device verification + docs sync** — MSE overlay rows (buffered depth, segments, append errors, `readyState`/`paused`, inline-vs-fullscreen live-edge) on gated devices; on-device passes; README/ROADMAP/CLAUDE sync; replace the R16 BUGS.md entry | Manual plan executed + findings recorded; **verdicts**: native fullscreen shows real frames (spike-confirmed — re-confirm in-app), latency acceptable, no thermal/battery red flags over ~20 min (this drives the Decision-5/MF4 suspend-canvas call), reconnect-while-armed works, config-change-while-fullscreen recovers; `NativeVideoFullscreen ✓` on iPhone; the R16 black-video BUGS.md entry becomes "resolved via MSE (R22)" | 🔶 overlay rows + docs sync + BUGS.md done 2026-07-25; **the on-device verification pass is pending** (not CI-reachable) |

Ordering: MF1 (pure muxer, CI-provable in Chrome) and MF2 (probe/gate) are
independently valuable and low-risk; MF3 is the behavioral landing that also
removes the dead R16 tier; MF4 hardens latency; MF5 verifies and closes. The
spike already de-risked the one load-bearing platform unknown, so **no chunk is
gated on an unverified iOS fact** — a difference from R16, whose U-chunks built
the whole native path before U4 could test it.

## Implementation status & deviations (2026-07-25)

MF1–MF4 implemented; MF5's observability/docs half done, and its first on-device
pass is **done** — see "On-device findings" below: native fullscreen shows real
video (the design's premise holds), with two liveness defects found and fixed
(infinite duration; audio muxed into the presentation) and three more diagnosed
and left open. A second on-device pass is needed for the fixes.
Automated gates green: gawk-app tsc/vitest/oxlint/build (855 tests
incl. the new muxer, presenter, fullscreen-tier, worker-tap and
controller-message-shape suites), plus
the full tier-1 e2e (fixture publisher → relay → production viewer, live +
resilient + deep-buffer passes) and the new `run.mjs --muxer-check` Chrome
`MediaSource` playback proof — both run locally and wired into the `e2e` CI
job (the muxer check runs first as its own step; failure artifacts land in
`out-muxer-check/`).

Where things live: the muxer is `gawk-app/src/transport/fmp4-muxer.ts` (pure,
DOM-free; golden vectors in its test against the committed
`transport/h264-fixture.ts` — the first 18 AUs of the same 320x240 stream
`gawk-broadcast/internal/fixture` embeds); the main-thread MMS surface is
`features/viewer/msePresentation.ts` (probe + `MsePresenter`); the encoded
fork rides the pipeline callbacks as `onReleasedFrame`
(`viewer.ts` → `viewer-session.ts` → `viewer-worker-core.ts`'s
`WorkerHost.frameTap`, generation-guarded); the worker shell owns the muxer
and posts `muxSegment` events with transferred buffers; the in-page CI driver
is `src/e2e/muxer-check-entry.ts`.

Deviations from the letter of the design, recorded so nobody "fixes" them
back:

1. **The muxer decides the wire format at keyframes and holds it sticky
   between them** — a per-frame Annex-B sniff (the viewer.ts precedent the
   design pointed at) is structurally unsafe here: an AVCC 4-byte length
   prefix for any 256–511-byte NAL is `00 00 01 xx`, byte-identical to a
   start code, and fixture frame 16 is exactly that (the AVCC/Annex-B
   equivalence test caught it producing a mangled sample). Keyframes are
   unambiguous — avcC-bearing config ⇒ AVCC, leading start code + in-band
   SPS/PPS ⇒ Annex-B — so the decision is made there and deltas inherit it.
2. **MF2's "trial `addSourceBuffer`/tiny-append" is the arm itself, not a
   separate probe step**: `addSourceBuffer` is only legal on an open,
   element-attached MediaSource, and the arm happens at `watching` — well
   before any fullscreen tap — so a real failure there degrades the gate to
   pseudo exactly as a pre-probe would have (`MsePresenter.failed` →
   `NativeVideoFullscreen ✗ / MSE surface failed → pseudo`). The synchronous
   probe half (ctor presence + H.264-only + `isTypeSupported`) runs on the
   first codec event.
3. **Broadcaster-restart recovery is timestamp-driven inside the muxer, not
   wired to the reorder buffer's restart callback**: the release stream the
   muxer taps already reflects the reorder buffer's restart resync, and the
   muxer re-anchors its own monotonic output timeline whenever input
   timestamps jump backwards (or absurdly forward, > 5 s) — so a restart
   plays through as a seamless continuation with `baseMediaDecodeTime` never
   moving backwards, no second signal path needed.
4. **The output timeline is muxer-owned and strictly monotonic** (input
   timestamps are the broadcaster's clock — huge, and reset by restarts);
   `timescale` is 1 MHz so wire µs map with no rounding, and the one-sample
   fragment layout makes `moof` a constant 100 B with `data_offset` 108
   (pinned by tests, so a box edit can't silently break the offset).
5. **Samples are rewritten to 4-byte-length AVCC NALs with AUD/SPS/PPS
   stripped** (parameter sets live in the avcC; in-band sets are an `avc3`
   affordance and Safari is strict about `avc1`); SEI and slices pass
   through byte-identical — asserted by the golden vectors and by ffmpeg
   decoding the concatenated output clean during development.
6. **`presentationSurfaceStats` reshaped, not extended**: the R16 tee fields
   (`teedFrames`/`teeErrors`/`elementContentPeak` and the "Content sample"
   row) died with the tee; the section now reports the muxer counters
   (worker), append counters + buffered span/behind-edge (main thread), and
   the element readyState/paused/size/rVFC rows that carry over.
7. **The hidden `<video>` no longer autoplays** — Decision 5's
   loaded-but-paused state is literal now: the element renders (gated +
   armed) without `autoPlay`, `useFullscreen` tier 2 does
   seek-to-live → `play()` → `webkitEnterFullscreen()` in-gesture, and both
   exit paths (system UI event and the toggle) pause it again so the second
   decode never outlives the native player.
8. **`MsePresenter` survives element remounts by caching the newest init
   segment**: a re-attach builds a fresh MediaSource, re-primes it from the
   cache and drops queued media up to the next keyframe (their references
   live in the old SourceBuffer). The same cache re-primes when queue
   overflow drops an init before it could append.
9. **Attachment tries `srcObject` first and falls back to an object URL** —
   WebKit's MMS-recommended wiring vs. Chromium (which rejects a MediaSource
   as `srcObject`); the CI muxer check exercises the object-URL path against
   real Chrome.
10. **A codec probe re-runs on every codec event** (not once): a broadcaster
    restart that lands on a VP codec flips the gate to
    `✗ codec … is not H.264 → pseudo` mid-view instead of leaving an armed
    surface whose muxer has silently stopped producing — and the hidden
    `<video>` unmounts with it (review finding), so the next tap actually
    falls to pseudo rather than native-presenting the stale, frozen
    pre-restart content off a still-ready element.

## On-device findings (MF5 pass 1, 2026-07-26)

The first real-iPhone test of the shipped MF1–MF4 path. **The headline result is
positive and supersedes docs/21's verdict: native fullscreen shows real video.**
Two defects came with it, both in the *liveness* of the presentation rather than
in the media, and both fixed below. The owner's report:

> Video now actually shows up in full screen. For a while.. The native player
> doesn't show "live" badge anymore. The video (in fullscreen) plays sometimes
> for a second, sometimes for 10, but always pauses after that. When I click the
> native "play" icon, it'll continue for another moment.

### Finding 1 — a live MSE presentation must declare `duration = Infinity` (fixed)

`MsePresenter` never set `MediaSource.duration`. Left unset, MSE's coded-frame
processing raises duration to the newest appended end timestamp, so the element
reports a **finite, growing** duration. That costs two distinct things:

* **The LIVE badge.** WebKit's native player draws a live UI when
  `duration === Infinity`; with a finite duration it draws an ordinary scrub bar.
  Every live MSE player (hls.js, dash.js, shaka) sets this explicitly for exactly
  this reason — it is not a detail we can leave to the default.
* **The pauses.** With a finite duration, "the playhead reached the buffered end"
  is indistinguishable from "the media resource ended". WebKit resolves that
  ambiguity by **pausing and firing `ended`** where Chromium stalls and resumes
  when more data arrives — which is also why CI (Chrome) could not have caught
  it. A manual tap then resumes, because by then more media has been appended and
  `currentTime < duration` again: precisely the reported "continues for another
  moment", on a loop.

Fixed in `declareLive()`, called from the `sourceopen` handler — the one moment
the setter is legal (readyState `open`, no SourceBuffer updating; a fresh source
has none). Infinity is never less than the highest end timestamp, so appends
cannot lower it and one write per MediaSource suffices; a re-attach re-declares
on the new source. A refusal is non-fatal (`failed` stays false — a finite
duration degrades the player's UI and stall behavior, it does not stop it
presenting) and surfaces as the overlay's **Live duration** row plus
`presentationSurface.liveDuration` in Copy diagnostics. CI asserts both the
presenter's verdict and `video.duration === Infinity` in real Chrome.

### Finding 2 — audio in native fullscreen (implemented, on-device pending)

The muxed presentation was video-only, so entering native fullscreen lost audio
entirely: the inline AudioWorklet sink keeps playing, but the native player
covers the page and the two are independently clocked. Owner decision: **mux the
R15 Opus lane into the presentation as a second track.**

Design, as implemented:

* **Two SourceBuffers on one MediaSource** (`video/mp4; codecs="avc1…"` +
  `audio/mp4; codecs="opus"`), the standard MSE shape — not one interleaved file.
  It keeps the working video path untouched when audio is absent, refused, or
  dies.
* **Opus-in-MP4 is probed, never assumed** (`probeMseAudio`). WebKit 17 added
  "one or two channel Opus audio in WebM and MPEG-4 containers", but nothing
  documents it *through* ManagedMediaSource, and Apple's own HLS mandates AAC. A
  refusal keeps the native player video-only with the inline sink unchanged, and
  reports the reason in the overlay's **Audio track** row. If the iPhone refuses
  it, the follow-up is transcoding to AAC in the viewer worker (the decoded
  `AudioData` is already in hand for the worklet) — the muxer, presenter, tap and
  timeline plumbing are all reusable.
* **One shared timeline.** The muxer republishes the video path's input→output
  offset on every frame, and audio maps through it. Both media carry timestamps
  on the same broadcaster `performance.now()` clock (docs/20's load-bearing sync
  decision), so relative A/V skew is zero by construction. Audio before the first
  video frame has no anchor and is dropped.
* **The audio track's timescale is its sample rate**, so every duration is an
  exact sample count (960 for a 20 ms frame) and the track cannot accumulate
  rounding drift against video.
* **Sample durations come from one packet of lookahead.** This is the
  load-bearing rule: `HTMLMediaElement.buffered` is the **intersection** of the
  tracks' ranges, so a hole in audio is a hole in playback and stalls the video
  the native player is showing. A sample's duration is therefore the real
  interval to its successor — which also means a lost datagram is absorbed by
  *stretching* the preceding sample over the gap (the range stays continuous and
  the audio renderer pads). Past `AUDIO_MAX_STRETCH_MS` (1 s) the loss is an
  outage rather than jitter: the sample is declared honestly, the hole is counted
  (`audioHoles`), and on a link that bad the video datagrams are gone too.
* **The audio SourceBuffer is created lazily, on the first audio *sample***, not
  when the config arrives. An audio track with no samples empties the buffered
  intersection — it would stall the video and could keep the element from ever
  reaching the readiness `webkitEnterFullscreen` requires.
* **Audio faults never touch video**: a refused `addSourceBuffer` (the plausible
  iOS outcome) drops the audio track for good and leaves `failed` false.
* **The handoff is exclusive.** Entering tier-2 fullscreen suppresses the inline
  sink (a new `audio.setSuppressed`, an override that never touches the persisted
  mute preference) and the hidden `<video>` is unmuted — declaratively, since
  both inputs are known before the tap, and honoring the viewer's own mute
  toggle; the viewer's volume is mirrored onto the element. Exit restores the
  inline sink. Without a muxed track nothing is suppressed, or fullscreen would
  be silent.
* **The stream's audio format now comes from the config message**, not the decode
  lane: they agree whenever decoding works, and where it doesn't (no
  `AudioDecoder` in scope) only the config knows the format at all — which is
  exactly the case this feature covers, an iPhone that cannot decode Opus itself
  still handing it to the native player.

**CI proof.** `run.mjs --muxer-check` now runs an audio leg: the page encodes
real Opus with the browser's own `AudioEncoder` (which forced the page onto a
`localhost` origin — `about:blank` is not a secure context and WebCodecs is
`[SecureContext]`), muxes it interleaved with the fixture video, and asserts the
`Opus`/`dOps` init segment is accepted, the track is created, and no holes
appear. The pre-existing "frames present + currentTime advances" assertions do
the heaviest lifting here for free: with both tracks appended the element only
plays where their ranges intersect, so a malformed audio timeline fails the video
verdict. Chrome reports `isTypeSupported('audio/mp4; codecs="opus"')` true; where
a runtime says false the leg is skipped **loudly**, never silently.

### Still open from the same investigation (diagnosed, not fixed)

Recorded so the next pass starts here rather than re-deriving it, and tracked as
three entries in [`BUGS.md`](../BUGS.md) (all three are iPhone-native-fullscreen
only — the inline canvas path has no equivalent failure mode). Findings 1 and
2 may well be enough on their own — the point of the fixes is that an underrun
becomes a recoverable stall instead of a dead player — but these three are real
and independent. **They were deliberately not fixed in the same change**: 1 and 2
are what the owner scoped, and shipping the rest unverified would mean changing
the live-edge budget and adding a seek-issuing watchdog in the same pass that
first proved native fullscreen works at all — landing them separately keeps pass
2's verdict attributable.

3. **The live-edge cushion is ~100 ms and cannot grow.** `LIVE_EDGE_REJOIN_S`
   (0.1) in *both* `useFullscreen.seekToLiveEdge` and `MsePresenter.maybeCatchUp`
   — a duplicated constant, itself worth unifying. The subtlety is that the muxer
   is fed from `decodeReleased`, i.e. **after** the reorder buffer's paced
   release, so appends arrive at playback rate: the SourceBuffer can never build
   ahead while the element plays. Whatever the entry seek leaves is the entire
   budget for the session, and 100 ms is below the arrival jitter this project has
   already measured in the field (72–158 ms, docs/20 finding 6). `maybeCatchUp`
   then re-creates the condition every time lag exceeds 2 s. A shared
   `TARGET_LAG_S` of ~0.4–0.6 s is the fix; with audio now muxed it costs
   fullscreen latency but no lip-sync, since both tracks come off the same
   timeline.
4. **Nothing watches for a stalled element.** WebKit's pause-on-underrun (finding
   1) and any hole in either track leave the native player stopped with no
   recovery path — no listener for `waiting`/`stalled`/`ended`/`pause` exists.
   Production players all ship this (hls.js's gap-controller nudges, seeks over
   holes, and re-plays). A tier-2-only watchdog that seeks into the newest
   buffered range and re-`play()`s is the general answer to both.
5. **MMS parking plus the bounded queue can manufacture holes while armed.**
   `pump()` parks on `streaming === false`; while armed the element is paused, so
   MMS's buffered-ahead only grows and it stops asking for data, while the queue
   fills and front-drops past `MAX_QUEUED_SEGMENTS`. Any arming period longer than
   the queue depth (~2–4 s) therefore leaves a hole between the last appended
   segment and where appends resume. Either keep the armed playhead following the
   buffered edge so MMS never parks, or treat resume-after-park as a
   discontinuity and seek into the newest range.

## Verification plan (manual, MF5)

On a real iPhone (Safari, plus Chrome-on-iOS for the same-WebKit check),
against the homelab deployment:

1. **Native path**: join a live broadcast → tap fullscreen → native video
   fullscreen with system UI; rotate to landscape; exit via system UI → inline
   viewer resumes seamlessly (the canvas never stopped); `isFullscreen` state
   correct throughout (button icon, menu label).
2. **Latency**: Copy-diagnostics live-edge drift inline vs. in fullscreen —
   record the added buffer depth; confirm it is acceptable for the native mode.
3. **Config change while fullscreen**: trigger an R4/R13 resolution step (or a
   codec pin) mid-view → the fullscreen video re-inits cleanly, no wedge.
4. **Reconnect while armed**: kill the relay pod mid-view → auto-reconnect →
   fullscreen still works without leaving/rejoining (one MMS across attempts).
5. **Probe-failure / VP tier**: a VP-codec broadcast (or a dev-forced probe
   failure) → the tap produces pseudo-fullscreen, no MMS, no video element;
   Feature Gates reads `NativeVideoFullscreen ✗`.
6. **Regression, non-gated**: desktop Chrome + Firefox — fullscreen unchanged,
   DOM has no video element, worker traffic unchanged (assert via logging);
   iPad — tier 1, R22 inert.
7. **Cost sanity**: ~20 min fullscreen session — battery/thermal; if
   concerning, apply the Decision-5 deferred-arm and/or suspend-inline-decode
   optimizations and re-measure.
8. Record findings + verdicts here; sync README/ROADMAP/CLAUDE; replace the
   R16 BUGS.md entry.

## Non-goals

- **Custom controls/overlay inside native fullscreen** — impossible under
  `webkitEnterFullscreen`; the system UI is the UI (inherited from R16).
- **Interpolated frames inside the native player** — structurally impossible
  (post-decode synthesis can't be encoded); retained inline instead
  (Decision 9).
- **VP8/VP9 native fullscreen on iOS** — H.264-only in v1 (Decision 11); VP
  broadcasts fall back to pseudo on iPhone.
- **MSE as the inline render path on iOS** (MSE-primary) — rejected
  (Decision 1 / Rejected).
- **An iOS-reachable stats-overlay opener** — the same real diagnosability gap
  R16 noted; a separate small item, not scoped here.
- **`screen.orientation.lock()`** — unavailable on iOS Safari.
- **Server/wire/broadcaster changes** — none; the encoded stream we mux is the
  one already on the wire.

## Rejected

- **MSE-primary on iOS** — drops paced playout, interpolation, and
  live-edge/TimeSync control everywhere on iPhone, runs at buffered latency,
  and forks the iOS viewer from every other platform, all to save the parallel
  path's second decoder. The parallel tee's only extra cost is a worker-side
  muxer and (while armed) a native decoder — paid only on iPhone.
- **The R16 `VideoTrackGenerator` MediaStream tee** — proven black on iOS
  across three on-device passes (docs/21). R22 deletes it (Decision 7).
- **Lazy arm on the fullscreen tap** — the async MMS arm chain (create → add
  SourceBuffer → append → metadata) risks leaving the user gesture and
  producing a dead tap; pre-arm at `watching` (Decision 5), with deferred arm
  as a recorded thermal optimization.
- **Re-encoding interpolated canvas frames to feed the native player at 60 fps**
  — re-encoding H.264 on a phone for a fullscreen button is absurd; the native
  player shows real cadence (Decision 9).
- **WebRTC** for the presentation surface — set aside project-wide, and
  unnecessary: MMS is guaranteed on every gated device that can connect
  (Safari 26.4 ⊃ iOS 17.1).
