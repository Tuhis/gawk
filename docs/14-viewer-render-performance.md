# R10 — Viewer Render Performance: Design + Implementation Plan

Design doc for [ROADMAP R10](../ROADMAP.md#r10--viewer-render-performance).

Firefox viewers show two coupled symptoms that Chrome viewers don't:
a significant **"Dropped (incomplete)"** rate (reassembler-level datagram
loss) and **decoded fps persistently below received fps** (the decoder falls
behind and the resync-to-keyframe policy dumps work). This doc diagnoses
both, records the fix ideas, and implements P1–P3: the render fixes (P1/P2)
land behind the worker-side **`RenderSink`** seam introduced by R8 S6, the
transport split (P3) behind a new **`ViewerTransport`** seam in
`ViewerPipeline`. P1–P4 are zero server/wire/broadcaster changes; the
[field findings](#field-findings-2026-07-14-diagnostics-session) below later
added one server-side fix (zombie-subscriber eviction, with new close code
4001) and a viewer reorder-wait retune. Wire *format* unchanged throughout.

## Background: the diagnosis

On the worker path (which Firefox takes — it has worker `WebTransport` +
`VideoDecoder`), **one worker thread runs everything**: the datagram read
loop, reassembly, the reorder buffer, decoder feeding, *and* rendering.
`ViewerPipeline.handleDecoded` calls `RenderSink.draw()` synchronously from
the `VideoDecoder` output callback, and the sink is a **2D-canvas
`drawImage(VideoFrame)`** (`transport/render-sink.ts`).

That draw is the problem multiplier on Firefox:

1. **Firefox's 2D-canvas `drawImage(VideoFrame)` is a slow path** — it
   typically performs a synchronous CPU YUV→RGB conversion per frame
   (Chromium GPU-accelerates this; Gecko largely does not, especially when
   the decoder output is a software frame).
2. While the worker thread is stuck in `drawImage`, **the browser's small
   incoming-datagram queue overflows and silently drops datagrams** — one
   lost chunk discards the whole frame, surfacing as "Dropped (incomplete)".
   (This is invisible on Firefox: it lacks `WebTransport.getStats()`, so the
   overlay's "Dgrams dropped (in)" row is `—` there.)
3. The decoder's output callbacks queue behind the same busy thread, so
   decoded fps lags received fps; once `decoderQueueDepth` passes
   `maxDecoderQueueSize` (5), the pipeline requests a reorder-buffer resync
   and waits for the next keyframe — with a 500 ms GOP, a decoder that is
   persistently ~10% too slow cycles through overflow → dump → keyframe
   wait, amplifying a small deficit into a visible fps gap.

Independently, **Firefox's WebCodecs H.264 decode is often software**
(ffvpx), especially on Linux. Note the overlay's "Decode mode" row is *not*
ground truth: `Decoder.isHardwareAccelerated` only records that the
`prefer-hardware` **probe** was accepted, and Firefox treats the hint as
advisory — it can report "Hardware" while decoding in software.

**Why YouTube is fine on the same machine**: `<video>` + MSE is a 5–30 s
buffer over TCP (loss invisible), ABR that quietly degrades, and decode +
presentation entirely inside the browser's native, hardware-backed media
pipeline with zero JavaScript per frame. gawk deliberately trades all of
that for sub-500 ms live edge and runs Firefox's newest, least-optimized
media path (WebCodecs, shipped 2024) with JS in the per-frame loop. The gap
is the price of the latency target; this doc's job is to stop paying more
of it than necessary.

## The ideas (full list, including deferred)

| # | Idea | Attacks | Status |
|---|------|---------|--------|
| 1 | **Coalesced rendering** — latest-frame-wins, one draw per `requestAnimationFrame` tick instead of one per decoded frame | Worker-thread starvation (both symptoms) | **P1, this doc** |
| 2 | **WebGL render sink** — replace 2D `drawImage` with a WebGL textured quad (`texImage2D(VideoFrame)`); 2D stays as fallback | Per-draw CPU cost on Firefox | **P2, this doc** |
| 3 | **Transport/decode worker split** — WebTransport read loops in their own worker, posting transferable buffers to the decode/render worker, so no render/decode pressure can ever starve the datagram queue | Residual datagram drops under load | **P3, this doc** (pulled forward from "deferred" by broadcaster decision, ahead of P1/P2 measurements) |
| 4 | **Decode-load reduction & backpressure tuning** — confirm the real decode path via `about:support`; broadcaster picks a lower rung when viewers are software-decoding; a larger decoder queue bound or stickier overload response to break the overflow→dump→wait cycle | Software-decode deficit | Partially pulled forward: **decoder queue bound raised 5 → 10** (see Decision 8); the rest stays deferred pending measurements |

Per-viewer quality (simulcast/SVC) remains explicitly out of scope, as in R4.

## Decisions (locked)

1. **WebGL over `bitmaprenderer`.** Both avoid Firefox's software 2D path,
   but WebGL is expected to be faster and more predictable:
   `texImage2D(VideoFrame)` is the classic video-upload path every WebGL
   player exercises, it is synchronous with no per-frame garbage, and
   browsers keep it on the GPU where the platform allows. `bitmaprenderer`
   needs `createImageBitmap(frame)` per frame — a per-frame allocation plus
   an async hop, and in Gecko the ImageBitmap conversion can hit the same
   software conversion the 2D path does, i.e. it may just move the cost, not
   remove it. The `RenderSink` seam keeps a `bitmaprenderer` variant cheap
   to add later **if measurements disagree** — decisions here are
   falsifiable by the overlay funnel, by design.
2. **Latest-frame-wins at display cadence.** The sink draws at most once per
   `requestAnimationFrame` tick (worker rAF: Chrome and Firefox ≥ 105); a
   frame still pending when a newer one decodes is closed unseen. Painting
   faster than the display refreshes is pure waste, and this is the R5
   "latest-frame-first rendering" bullet landing early, at the sink level.
   Where worker rAF is unavailable, fall back to a ~16 ms timer — coalescing
   is the point, the exact clock is not.
   **Consequence**: the overlay's "Rendered fps" now reads
   ≈ min(decoded fps, display Hz) and *lower than decoded fps under load is
   healthy* — it means coalescing is shedding paints, not that frames are
   lost. The funnel's loss signals stay "Dropped (incomplete)" and the
   decoded-vs-received gap.
3. **Worker path only; the 2D sink survives as fallback.** The main-thread
   `ViewerSession` fallback (browsers without worker WebCodecs/WebTransport)
   keeps its existing draw-in-callback path — it isn't the population with
   the problem, and rAF pacing on the main thread has different trade-offs.
   The 2D `OffscreenCanvasRenderSink` also remains as the in-worker fallback
   when WebGL context creation fails.
4. **Where things run becomes observable — ground truth, not intent.**
   `ViewerStats` grows three placement fields, all shown in the stats
   overlay and included in Copy-diagnostics JSON, because every fallback in
   this design degrades *silently* and remote "is your Firefox actually on
   the fast path?" must be answerable from data, per R9:
   - `renderer` (`'webgl' | '2d' | null`) — which sink paints ("Renderer"
     row; `null` = main-thread path, where the pipeline doesn't draw);
   - `pipelineContext` (`'worker' | 'main-thread'`) — whether the pipeline
     runs in the viewer worker or the main-thread fallback ("Pipeline" row).
     *Detected* from the absence of `window` in the pipeline's own scope,
     not plumbed from the code that chose the path — a misrouted fallback
     reports what actually happened;
   - `transport` (`'worker' | 'in-process' | null`) — whether the read
     loops run in the nested transport worker or in-process next to decode
     ("Transport" row, Network section), from the active `ViewerTransport`'s
     `kind`.
   The healthy worker-path signature is `webgl / Worker / Worker`.
5. **Zero wire/server/broadcaster changes.** Everything lands behind the
   `RenderSink` interface; `ViewerPipeline`, `ViewerWorkerCore`, and the
   worker shell keep their contracts (the shell just constructs the sink via
   a factory now).
6. **The transport split rides a seam, not a rewrite (P3).** `ViewerPipeline`
   now consumes a `ViewerTransport` interface (`transport/viewer-transport.ts`:
   `connect(callbacks)` / `sampleConnectionStats()` / `close()`, with
   `onClosed` as the **single** authoritative session-end signal — the
   wt.closed vs read-loop settle-order race lives *inside* the transport,
   per the CODE-REVIEW "one event, one signal" rule). Two implementations:
   `LocalViewerTransport` (the extracted in-process connection code — the
   main-thread fallback and the no-nested-worker fallback) and
   `WorkerViewerTransport`, which proxies to a **nested** transport worker
   (`transport.worker.ts` around a host-agnostic `TransportWorkerCore` that
   itself reuses `LocalViewerTransport` — one connection implementation,
   two homes). The existing `viewer.test.ts` close-race/freeze-on-gap suites
   run unchanged against the seam, which is the compatibility proof.
7. **Nested worker, one per pipeline attempt.** The viewer worker spawns the
   transport worker (main thread untouched — no third context to plumb);
   each `ViewerPipeline` attempt gets a fresh transport worker, so reconnect
   teardown is simply worker death. `close()` posts a graceful close (the
   session close frame frees the relay's subscriber slot immediately instead
   of waiting out the 30 s idle timeout), then reaps the worker after a
   250 ms backstop. Datagram/keyframe buffers cross the boundary as
   **transferables** (zero-copy); keyframe payload + embedded-config
   extradata usually share one buffer and the transfer list is deduped.
   Backpressure consequence: the lossy browser datagram queue is replaced by
   a lossless message queue into the decode worker — under sustained decode
   overload frames now *arrive* and are shed by the existing reorder-buffer
   resync/late-drop policies (visible in stats) instead of vanishing as
   silent datagram drops. Where nested workers don't exist, the factory
   falls back to `LocalViewerTransport` in the viewer worker — pre-P3
   behavior.
8. **Decoder queue bound: 10, globally (was 5).** Broadcaster decision,
   pulled forward from P4: with a persistent ~10%-too-slow decoder, a bound
   of 5 cycles overflow → drop-to-keyframe → GOP wait, amplifying a small
   deficit. 10 absorbs a burst (~330 ms at 30 fps worst case before resync)
   against a 500 ms GOP recovery — still Firefox-friendly, not
   Firefox-only, since the runtime `maxDecoderQueueSize` config override
   remains for tuning.

## Status

| Chunk | Scope | Acceptance criteria | Status |
|-------|-------|---------------------|--------|
| P1 | **Coalescing sink** — `CoalescingRenderSink` wrapping any inner sink: latest-frame-wins, one inner `draw()` per scheduler tick (worker rAF, ~16 ms timer fallback), superseded frames closed unseen | Unit tests (fake scheduler, fake frames): N draws before a tick → 1 inner draw of the newest frame, N−1 older frames closed exactly once, nothing scheduled twice per tick; a frame arriving after a flush schedules a new tick; `drawnFrames()` delegates to the inner sink so "Rendered fps" counts real paints | ✅ done |
| P2 | **WebGL sink + factory + renderer stat** — `WebGLRenderSink` (textured quad, `texImage2D(VideoFrame)`, resize + viewport tracking, context-lost tolerance); `createRenderSink()` factory (WebGL2 → WebGL → 2D, wrapped in P1's coalescer) used by `viewer.worker.ts`; `ViewerStats.renderer` + overlay "Renderer" row | Unit tests (fake GL): draw uploads the frame via `texImage2D` and closes it (also on context-lost and on exceptions); canvas resize + `viewport` only when frame dimensions change; factory falls back to the 2D sink when WebGL contexts are unavailable and the result is always coalescing-wrapped; stats carry `renderer` on the worker path (`null` main-thread), overlay renders the row; all gates green | ✅ done |
| P3 | **Transport/decode worker split** — `ViewerTransport` seam in `ViewerPipeline` (`viewer-transport.ts`: `LocalViewerTransport` = extracted in-process connection code); nested `transport.worker.ts` around a host-agnostic `TransportWorkerCore` (reuses `LocalViewerTransport`); `WorkerViewerTransport` proxy; one nested worker per pipeline attempt, spawned by `viewer.worker.ts` when nested workers exist | Existing `viewer.test.ts` close-race + freeze-on-gap + stats suites pass **unchanged** against the seam (behavior parity); core unit tests pin the message protocol (connected/connect-error/datagram/keyframe/closed/connStats), transferable lists (incl. shared-buffer dedupe), close-code passthrough (4000 stays terminal end-to-end), and graceful-close-then-reap; a proxy↔core pairing test runs both sides unmocked; `vite build` emits a `transport.worker` asset (nested-worker bundling verified); all gates green | ✅ done |
| P4 | **Decode-load reduction & backpressure tuning** — decoder queue bound 5 → 10 pulled forward (Decision 8); the rest (decode-path confirmation, rung guidance, stickier overload response) | Queue bound: default `getMaxDecoderQueueSize()` returns 10, runtime override still honored. Rest: design when picked up — requires P1–P3 measurements first | 🚧 queue bump done; rest deferred |

## Verification plan (manual, Firefox on the affected machine)

Before/after comparison using the stats overlay (`Ctrl+Alt+Shift+D`) at the
same broadcaster settings, ~60 s samples via Copy diagnostics:

- "Renderer" / "Pipeline" / "Transport" rows read **WebGL / Worker /
  Worker** — the full fast-path signature (fallback check: force WebGL off →
  `Canvas 2D`, picture still renders; on a browser without worker
  WebCodecs the trio reads — / Main thread / In-process).
- "Dropped (incomplete)" rate collapses toward the Chrome baseline —
  confirms the starvation theory. (With P3 landed, residual "dropped"
  entries should be genuine network loss plus reorder-buffer resyncs —
  check "Gap resyncs" to tell them apart.)
- Decoded fps ≈ received fps; "Gap resyncs" stops climbing during steady
  playback.
- Rendered fps ≈ min(decoded fps, display Hz) — coalescing active, no
  visible jank added.
- Cross-check on Chrome: no regression (Chrome was already fine; the WebGL
  sink now serves it too, and "Dgrams dropped (in)" — visible there via
  `getStats()` — should stay ~0).

If the decoded-vs-received gap persists after P1–P3 on Firefox, that is a
pure decode deficit — pick up the rest of P4 (decode-path confirmation,
rung guidance, stickier overload response): re-measure, then design.

## Field findings (2026-07-14 diagnostics session)

First real capture with P1–P3 deployed (Chrome broadcaster 3440×1440@60
native/HW, Chrome + Firefox viewers, relay `/statusz` deltas + Copy
diagnostics from all three, one shared window). Firefox: much improved,
usable. Chrome: worse than before — and the data attributed it precisely.

1. **Keyframes ride a much slower path than their deltas, and the reorder
   buffer's 200 ms wait couldn't cover it.** Deltas arrive as datagrams
   near-instantly; a keyframe is a ~236 KB store-and-forward stream and was
   measured landing > 500 ms behind its trailing deltas on a congested peer
   (relay-side evidence: that subscriber's `superseded` keyframe drops — the
   previous keyframe write still in flight when the next arrived — plus a
   26–55-deep egress datagram queue). At `KEYFRAME_WAIT_MS = 200` the held
   deltas expired before their keyframe landed → guaranteed frame-id gap →
   freeze until the *next* (also late) keyframe → **one gap resync per GOP
   with zero reassembler loss** and keyframe-only ~2 fps playback. Firefox
   played fine on the identical stream because its keyframe latency was
   ~50 ms — inside the window. **Fix (implemented, test-first)**:
   `KEYFRAME_WAIT_MS` 200 → **1000 ms**, which covers the measured worst
   case and stays inside the `MAX_BUFFERED_FRAMES` memory bound (~1.07 s at
   60 fps). Amends docs/12 Decision 7's latency assumption; this hold only
   engages while a keyframe is in flight — the steady path is untouched.
2. **A zombie subscriber can absorb keyframe fan-out forever.** `/statusz`
   showed a third subscriber whose *every* keyframe stream open had failed
   for ~20 minutes (`openFailed` ticking at exactly the keyframe rate,
   lifetime sent+dropped ≈ the broadcast's lifetime keyframes) — the
   signature of a leaked session whose client stopped reading uni streams
   and exhausted its stream credit. QUIC keepalives keep such a session
   alive indefinitely. **Fix (implemented, test-first)**: the relay evicts a
   subscriber after `KeyframeOpenFailEvictThreshold` (10) *consecutive* open
   failures — closed with new close code **4001
   (`CloseCodeSubscriberUnresponsive`**, mirrored as
   `CLOSE_CODE_SUBSCRIBER_UNRESPONSIVE` in `wire.ts`), which is deliberately
   **non-terminal**: a live client's auto-reconnect gets a fresh session
   (and fresh stream credit); a zombie stops costing work. The streak resets
   on any successful open. A constant, not a knob — leak cleanup is
   correctness, not tuning.
3. **`WebTransport.getStats()` is broken on Chrome 152** — `connection:
   null` on both surfaces including the main-thread broadcaster, so not a
   P3/nested-worker issue. Not fixed yet; tracked in [`BUGS.md`](../BUGS.md).
4. Healthy-by-data: broadcaster cadence (1.9 keyframes/s, encoder queue 0),
   broadcaster→relay leg (`ingressFramesLost` 0), and relay→Firefox
   delivery (2.0/s, zero drops) — the R9 playbook attribution worked as
   designed.
5. **R8 severed the reassembler's restart recovery** (found from a
   pause/restart report: viewers froze into a 2 fps keyframe slideshow).
   The reassembler drops "late" deltas against a watermark that only a
   *keyframe passing through it* could reset — and since R8, keyframes ride
   reliable streams and never pass through it. After a broadcaster restart
   (same broadcast ID, viewers stay connected, frameIds reset to 0) every
   new-session delta read as ≤ the old session's high watermark and was
   dropped as "Dropped (late)" — for ~36 minutes at 50 fps, until the ids
   caught up. Keyframes (stream path) played fine: exactly 2 fps.
   **Fix (implemented, test-first)**: the pipeline syncs the watermark from
   every stream keyframe (`Reassembler.noteStreamKeyframe`); all frameId
   comparisons in the reassembler and reorder buffer now use **serial
   (uint32 wrap-aware) arithmetic** (`wire.frameIdAhead` / `nextFrameId`, so
   natural rollover — the wire encoding wraps at 2^32 — also survives, and
   the broadcaster wraps its counter at source to match); and a keyframe
   arriving serially *behind* the decode position is treated as the restart
   signal in the reorder buffer — **immediate resync** instead of waiting
   out the delta-gap grace, which would stale-drop the new session's first
   deltas and cost an extra GOP of freeze. Diagnostic signature (vs. the
   late-keyframe loop in finding 1): `framesDroppedLate` climbing at full
   frame rate while `reorderKeyframeWaitDrops` stays flat.

Broadcast-scale note for the future: at native ultrawide the ~236 KB
keyframe consumes ~38 % of a 10 Mbps GOP budget; if late-keyframe pressure
returns even with the wider wait, the next lever is keyframe size
(broadcaster rung / bitrate), not more buffering.

**Re-verification (2026-07-14)**: with the field-finding fixes deployed, the
verification plan above passed on both Chrome and Firefox. The P4 remainder
(decode-path confirmation, rung guidance, stickier overload response) stays
deferred pending future measurements.

## Non-goals

- Main-thread fallback path rendering changes (Decision 3). The main-thread
  path also keeps its in-process transport — it exists only for browsers
  without worker WebCodecs/WebTransport, where a transport worker wouldn't
  have those APIs either.
- A `bitmaprenderer` sink (Decision 1 — add only if WebGL measurements
  disappoint).
- Per-viewer quality adaptation, playout buffering (R5), or any
  transport/protocol change.
